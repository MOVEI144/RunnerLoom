package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// RefreshDemand is called with a newly fetched server-side statistics snapshot,
// not a cached count. This releases the post-shutdown stale-demand barrier.
func (s *Store) RefreshDemand(ctx context.Context, pool string, desired int64) error {
	if !ValidName(pool) || desired < 0 || desired > 1000000 {
		return errors.New("invalid demand snapshot")
	}
	b, _ := json.Marshal(Demand{Pool: pool, Desired: desired, Seen: s.Now().UTC()})
	_, e := s.DB.ExecContext(ctx, "INSERT INTO demand(pool,payload,barrier) VALUES(?,?,0) ON CONFLICT(pool) DO UPDATE SET payload=excluded.payload,barrier=0", pool, b)
	return e
}

type Lock struct{ file *os.File }

func AcquireLock(dir, name string) (*Lock, error) {
	if !ValidName(name) {
		return nil, errors.New("invalid process lock")
	}
	if e := PrivateDir(dir); e != nil {
		return nil, e
	}
	path := filepath.Join(dir, name+".lock")
	if st, e := os.Lstat(path); e == nil && !st.Mode().IsRegular() {
		return nil, errors.New("unsafe lock path")
	} else if e != nil && !os.IsNotExist(e) {
		return nil, e
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, Fail("ALREADY_RUNNING", "この状態ディレクトリは別の常駐プロセスが使用しています", name)
	}
	return &Lock{f}, nil
}
func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	f := l.file
	l.file = nil
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return f.Close()
}

func SuggestedResources(dedicated bool) Resources {
	cpus := int64(runtime.NumCPU())
	reserve := max(int64(1), cpus/8)
	if !dedicated {
		reserve = max(int64(2), cpus/2)
	}
	memory := int64(8192)
	if b, e := os.ReadFile("/proc/meminfo"); e == nil {
		for _, line := range strings.Split(string(b), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "MemTotal:" {
				n, e := strconv.ParseInt(fields[1], 10, 64)
				if e == nil {
					memory = n / 1024
				}
			}
		}
	}
	reserveMem := max(int64(2048), memory/8)
	if !dedicated {
		reserveMem = max(int64(4096), memory/2)
	}
	return Resources{CPU: max(int64(1), cpus-reserve), Memory: max(int64(512), memory-reserveMem), Disk: 100}
}
func Example() Config {
	return Config{APIVersion: Version, Name: "home", GitHub: GitHub{URL: "https://github.com/MOVEI144", RunnerGroupID: 1, CredentialFile: "/var/lib/runnerloom/controller/github-credentials.json", AllowedRepositories: []string{"MOVEI144/RunnerLoom"}}, Images: []Image{{Name: "ubuntu-24", Digest: "sha256:" + strings.Repeat("0", 64), MinimumRootGiB: 20}}, Nodes: []Node{{Name: "node-a", Budget: Resources{14, 24576, 300}, LocalCeiling: Resources{14, 24576, 300}, AllowedPools: []string{"linux-lite", "linux-heavy"}}}, Pools: []Pool{{Name: "linux-lite", RunnerName: "home-linux-lite", Image: "ubuntu-24", VCPU: 4, MemoryMiB: 4096, OverheadMiB: 512, RootGiB: 20, ScratchGiB: 0, DiskOverheadGiB: 2, MaxRunners: 3, WarmIdle: 0, ExecutionMinutes: 120, Enabled: true}, {Name: "linux-heavy", RunnerName: "home-linux-heavy", Image: "ubuntu-24", VCPU: 8, MemoryMiB: 8192, OverheadMiB: 512, RootGiB: 40, ScratchGiB: 0, DiskOverheadGiB: 2, MaxRunners: 1, WarmIdle: 0, ExecutionMinutes: 360, Enabled: true}}, Reservations: []Reservation{{Name: "heavy-reserved", Node: "node-a", Pool: "linux-heavy", Slots: 1}}}
}

type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

func Doctor() []Check {
	checks := []Check{{"os", "pass", runtime.GOOS + "/" + runtime.GOARCH}}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		checks[0].Status = "fail"
	}
	for _, item := range []struct{ Name, Path string }{{"kvm", "/dev/kvm"}, {"libvirt", "/usr/bin/virsh"}, {"qemu", "/usr/bin/qemu-system-x86_64"}, {"qemu-img", "/usr/bin/qemu-img"}, {"cloud-init-seed", "/usr/bin/cloud-localds"}, {"firewall", "/usr/sbin/nft"}, {"dhcp", "/usr/sbin/dnsmasq"}} {
		state := "pass"
		detail := item.Path
		if _, e := os.Stat(item.Path); e != nil {
			state = "fail"
			detail = "必要な機能・コマンドが見つかりません: " + item.Path
		}
		checks = append(checks, Check{item.Name, state, detail})
	}
	checks = append(checks, Check{"resources", "info", fmt.Sprintf("推奨提供上限: %+v（ディスクは保存先で再確認）", SuggestedResources(true))}, Check{"live-github", "not_checked", "github check で権限と接続を確認してください"}, Check{"network-isolation", "not_checked", "network check で実際の隔離設定を検査してください"})
	return checks
}
