package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/MOVEI144/RunnerLoom/internal/agent"
	"github.com/MOVEI144/RunnerLoom/internal/core"
	"github.com/MOVEI144/RunnerLoom/internal/host"
	"github.com/spf13/cobra"
)

func (a *App) prompt(r *bufio.Reader, label, fallback string) (string, error) {
	if a.NonInteractive {
		return "", core.Fail("MISSING_INPUT", "非対話ではJSON設定と必要なフラグを指定してください", label)
	}
	_, _ = fmt.Fprintf(a.Err, "%s [%s]: ", label, fallback)
	s, e := r.ReadString('\n')
	if e != nil {
		return "", e
	}
	s = strings.TrimSpace(s)
	if s == "" {
		s = fallback
	}
	return s, nil
}
func (a *App) wizard(r *bufio.Reader) (core.Config, error) {
	c := core.Example()
	var e error
	c.Name, e = a.prompt(r, "環境の名前", "home")
	if e != nil {
		return c, e
	}
	c.GitHub.URL, e = a.prompt(r, "GitHub OrganizationのURL", "https://github.com/MOVEI144")
	if e != nil {
		return c, e
	}
	s, e := a.prompt(r, "許可Repository（owner/repo、複数はカンマ区切り）", "MOVEI144/RunnerLoom")
	if e != nil {
		return c, e
	}
	c.GitHub.AllowedRepositories = nil
	for _, v := range strings.Split(s, ",") {
		c.GitHub.AllowedRepositories = append(c.GitHub.AllowedRepositories, strings.TrimSpace(v))
	}
	s, e = a.prompt(r, "GitHubのRunner Group ID（Selected repositoriesに設定してください）", "1")
	if e != nil {
		return c, e
	}
	c.GitHub.RunnerGroupID, e = strconv.Atoi(s)
	if e != nil {
		return c, e
	}
	c.GitHub.CredentialFile, e = a.prompt(r, "GitHub認証設定JSONのパス（トークン本文ではありません）", filepath.Join(a.State, "github-credentials.json"))
	if e != nil {
		return c, e
	}
	digest, e := a.prompt(r, "作成済みGolden Imageのsha256:...（image buildの出力）", "")
	if e != nil {
		return c, e
	}
	c.Images[0].Digest = digest
	n := c.Nodes[0]
	n.Name, e = a.prompt(r, "このPCのNode名", "node-a")
	if e != nil {
		return c, e
	}
	dedicated, e := a.prompt(r, "このPCは実行専用ですか？ yes/no", "yes")
	if e != nil {
		return c, e
	}
	budget := core.SuggestedResources(dedicated == "yes")
	for _, field := range []struct {
		label  string
		target *int64
	}{{"VMに提供するvCPU合計上限", &budget.CPU}, {"VMに提供するRAM合計上限 MiB", &budget.Memory}, {"VMに提供するディスク合計上限 GiB", &budget.Disk}} {
		s, e = a.prompt(r, field.label, strconv.FormatInt(*field.target, 10))
		if e != nil {
			return c, e
		}
		*field.target, e = strconv.ParseInt(s, 10, 64)
		if e != nil {
			return c, e
		}
	}
	n.Budget = budget
	n.LocalCeiling = budget
	n.AllowedPools = []string{"linux-lite"}
	p := c.Pools[0]
	p.VCPU = min(int64(4), budget.CPU)
	p.MemoryMiB = min(int64(4096), budget.Memory-512)
	p.MaxRunners = max(int64(1), min(budget.CPU/p.VCPU, budget.Memory/(p.MemoryMiB+p.OverheadMiB)))
	c.Nodes = []core.Node{n}
	c.Pools = []core.Pool{p}
	c.Reservations = []core.Reservation{}
	if e = c.Validate(); e != nil {
		return c, e
	}
	return c, nil
}
func (a *App) addSetup(root *cobra.Command) {
	var file, role, nodeName, nodeDir, diskDir, advertise, cidr, invitation string
	var apply bool
	cmd := add(root, "setup", "質問に答えるかJSONを指定して、ControllerとNodeの設定を作る", 0, func(c *cobra.Command, _ []string) error {
		reader := bufio.NewReader(a.In)
		if !a.NonInteractive && !c.Flags().Changed("role") {
			_, _ = fmt.Fprintln(a.Err, "1: 新しい環境。このPCでも実行\n2: 管理専用\n3: 既存の環境にNodeとして参加")
			s, e := a.prompt(reader, "使い方", "1")
			if e != nil {
				return e
			}
			switch s {
			case "1":
				role = "controller-node"
			case "2":
				role = "controller"
			case "3":
				role = "node"
			default:
				return errors.New("1・2・3を選んでください")
			}
		}
		if role == "node" {
			if file == "" || invitation == "" {
				return core.Fail("MISSING_INPUT", "Node参加には --file node.json と --invitation 招待.json が必要です", nil)
			}
			conf, e := agent.LoadConfig(file)
			if e != nil {
				return e
			}
			b, e := core.ReadSecret(invitation)
			if e != nil {
				return e
			}
			var inv core.Invitation
			if e = core.Decode(bytes.NewReader(b), &inv); e != nil {
				return e
			}
			if !apply {
				return a.output(map[string]any{"role": role, "node": conf.Node, "next": "内容を確認して --apply を付けて参加申請"})
			}
			v, e := agent.Enroll(c.Context(), inv, conf)
			if e != nil {
				return e
			}
			return a.output(map[string]any{"node": v.Name, "status": v.Status, "requestID": v.ID, "csrHash": v.CSRHash})
		}
		if role != "controller-node" && role != "controller" {
			return errors.New("roleはcontroller-node、controller、nodeです")
		}
		var conf core.Config
		var e error
		if file != "" {
			conf, e = readConfig(file)
		} else {
			if a.NonInteractive {
				return core.Fail("MISSING_INPUT", "非対話setupには --file が必要です", nil)
			}
			conf, e = a.wizard(reader)
		}
		if e != nil {
			return e
		}
		s, e := a.store()
		if e != nil {
			return e
		}
		defer s.Close()
		plan, e := s.Plan(c.Context(), conf)
		if e != nil {
			return e
		}
		if !apply && !a.NonInteractive {
			_, _ = fmt.Fprintf(a.Err, "環境: %s / Pool: %d / Node: %d\n保存先: %s\nこの操作は設定と認証IDを保存します。ネットワーク・サービス・ディスク初期化は行いません。\n", conf.Name, len(conf.Pools), len(conf.Nodes), a.State)
			answer, e := a.prompt(reader, "この設定を保存しますか？ yes/no", "no")
			if e != nil {
				return e
			}
			apply = answer == "yes"
		}
		if !apply {
			return a.output(map[string]any{"plan": plan, "next": "--apply を付けるかconfig applyで設定を保存してください"})
		}
		revision, e := s.Apply(c.Context(), plan.ID)
		if e != nil {
			return e
		}
		ca, e := core.InitCA(a.State, conf.Name)
		if e != nil {
			return e
		}
		result := map[string]any{"cluster": conf.Name, "revision": revision, "configured": true, "githubReady": false, "vmReady": false, "networkChanged": false, "serviceInstalled": false, "controllerCommand": "runnerloom controller run --state " + a.State + " --advertise " + advertise}
		if role == "controller-node" {
			if nodeName == "" {
				if len(conf.Nodes) == 0 {
					return errors.New("実行Nodeの定義が必要です")
				}
				nodeName = conf.Nodes[0].Name
			}
			n, ok := conf.Node(nodeName)
			if !ok {
				return errors.New("指定Nodeが設定にありません")
			}
			if nodeDir == "" {
				nodeDir = a.State + "-node"
			}
			if diskDir == "" {
				diskDir = "/var/lib/libvirt/images/runnerloom-" + conf.Name + "-" + nodeName
			}
			nc := agent.Config{Node: nodeName, Cluster: conf.Name, Controller: advertise, StateDir: nodeDir, DiskDir: diskDir, NetworkCIDR: cidr, Ceiling: n.LocalCeiling, CacheGiB: 100, QEMUUser: "libvirt-qemu"}
			if e = nc.Validate(); e != nil {
				return e
			}
			if e = localIdentity(c.Context(), s, ca, nc); e != nil {
				return e
			}
			result["nodeConfig"] = filepath.Join(nodeDir, "agent.json")
			result["next"] = "image import → network plan/apply → controller run → agent run。doctorとgithub checkで未確認項目を確認してください"
		}
		return a.output(result)
	})
	cmd.Flags().StringVar(&file, "file", "", "ClusterまたはNode設定JSON")
	cmd.Flags().StringVar(&role, "role", "controller-node", "controller-node / controller / node")
	cmd.Flags().BoolVar(&apply, "apply", false, "設定と認証情報を保存する（ホスト構築・サービス起動は別操作）")
	cmd.Flags().StringVar(&nodeName, "node", "", "兼用構成で使うNode名")
	cmd.Flags().StringVar(&nodeDir, "node-state", "", "Nodeの秘密状態保存先")
	cmd.Flags().StringVar(&diskDir, "disk-dir", "", "新規のVM保存先。専用ディレクトリのみ")
	cmd.Flags().StringVar(&advertise, "advertise", "https://127.0.0.1:8443", "Nodeから見えるController URL")
	cmd.Flags().StringVar(&cidr, "network-cidr", "172.30.240.0/24", "専用VMネットワーク。適用時に衝突を検査")
	cmd.Flags().StringVar(&invitation, "invitation", "", "Node参加用の招待ファイル")
}
func localIdentity(ctx context.Context, s *core.Store, ca core.CA, c agent.Config) error {
	if e := core.PrivateDir(c.StateDir); e != nil {
		return e
	}
	keyPath := filepath.Join(c.StateDir, "node-key.pem")
	csrPath := filepath.Join(c.StateDir, "node.csr")
	key, e := core.ReadSecret(keyPath)
	var csr []byte
	if os.IsNotExist(e) {
		key, csr, e = core.NewKeyCSR()
		if e != nil {
			return e
		}
		if e = core.WritePrivate(keyPath, key); e != nil {
			return e
		}
		if e = core.WritePrivate(csrPath, csr); e != nil {
			return e
		}
	} else if e != nil {
		return e
	} else {
		csr, e = core.ReadSecret(csrPath)
		if e != nil {
			return e
		}
	}
	if existing, e := core.ReadSecret(filepath.Join(c.StateDir, "ca.pem")); e == nil && core.Hash(existing) != core.Hash(ca.PEM) {
		return errors.New("Nodeは別のControllerに登録済みです")
	}
	var certificate, oldCSR []byte
	e = s.DB.QueryRowContext(ctx, "SELECT certificate,csr FROM enrollments WHERE name=? AND status='Approved' ORDER BY id LIMIT 1", c.Node).Scan(&certificate, &oldCSR)
	if e == nil {
		if core.Hash(csr) != core.Hash(oldCSR) {
			return errors.New("登録済みNode名とこのPCの鍵が異なります")
		}
		if e = s.Authorize(ctx, c.Node); e != nil {
			return e
		}
	} else {
		inv, e := s.Invite(ctx, ca, c.Controller, 10*time.Minute)
		if e != nil {
			return e
		}
		q := core.JoinRequest{ID: inv.ID, Secret: inv.Secret, Name: c.Node, CSR: csr, Ceiling: c.Ceiling}
		request, e := s.Join(ctx, q)
		if e != nil {
			return e
		}
		approved, e := s.Approve(ctx, request.ID, ca)
		if e != nil {
			return e
		}
		certificate = approved.Certificate
	}
	pair, e := tls.X509KeyPair(certificate, key)
	if e != nil {
		return e
	}
	leaf, e := x509.ParseCertificate(pair.Certificate[0])
	if e != nil {
		return e
	}
	name, e := core.NodeIdentity(leaf, c.Cluster)
	if e != nil || name != c.Node {
		return errors.New("Node証明書の識別情報が不一致です")
	}
	for name, data := range map[string][]byte{"node.pem": certificate, "ca.pem": ca.PEM, "ca.sha256": []byte(core.Hash(ca.Certificate.Raw))} {
		if e = core.WritePrivate(filepath.Join(c.StateDir, name), data); e != nil {
			return e
		}
	}
	return agent.SaveConfig(filepath.Join(c.StateDir, "agent.json"), c)
}

func (a *App) addSmoke(root *cobra.Command) {
	var file, digest, nodeFile string
	var timeout time.Duration
	var tcg bool
	cmd := add(root, "smoke-vm", "実VMで固定診断を実行し、停止・削除まで確認。既存VMには触れません", 0, func(c *cobra.Command, _ []string) error {
		conf, e := agent.LoadConfig(nodeFile)
		if e != nil {
			return e
		}
		if timeout < time.Minute || timeout > 20*time.Minute {
			return errors.New("timeoutは1〜20分です")
		}
		lock, e := core.AcquireLock(conf.StateDir, "agent")
		if e != nil {
			return e
		}
		defer lock.Close()
		ex := host.SystemExecutor{}
		images := &host.Images{Dir: filepath.Join(conf.StateDir, "images"), LimitGiB: conf.CacheGiB, Exec: ex}
		f, e := os.Open(file)
		if e != nil {
			return e
		}
		_, e = images.Import(c.Context(), f, digest)
		f.Close()
		if e != nil {
			return e
		}
		network := host.Network{Cluster: conf.Cluster, CIDR: conf.NetworkCIDR, Dir: filepath.Join(conf.StateDir, "network"), Exec: ex}
		if e = network.Check(c.Context()); e != nil {
			return e
		}
		mode := "kvm"
		if tcg {
			mode = "tcg"
		}
		p := &host.Libvirt{StateDir: conf.StateDir, DiskDir: conf.DiskDir, Node: conf.Node, Cluster: conf.Cluster, Ceiling: conf.Ceiling, Network: network, Images: images, Exec: ex, QEMUUser: conf.QEMUUser, Emulator: mode}
		defer p.Close()
		prefix, e := netip.ParsePrefix(conf.NetworkCIDR)
		if e != nil {
			return e
		}
		ip := prefix.Addr().As4()
		ip[3] = 1
		probeListener, e := net.Listen("tcp", net.JoinHostPort(netip.AddrFrom4(ip).String(), "0"))
		if e != nil {
			return e
		}
		probeServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "RunnerLoom host isolation probe")
		}), ReadHeaderTimeout: 3 * time.Second}
		defer probeServer.Close()
		go func() { _ = probeServer.Serve(probeListener) }()
		p.DiagnosticProbe = probeListener.Addr().String()
		localClient := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second}
		defer localClient.CloseIdleConnections()
		baseline, e := localClient.Get("http://" + p.DiagnosticProbe)
		if e != nil {
			return e
		}
		baseline.Body.Close()
		if baseline.StatusCode != 200 {
			return errors.New("host probe baseline failed")
		}
		now := time.Now().UTC()
		instance := core.Instance{ID: core.ID(), RequestID: core.ID(), Node: conf.Node, Pool: core.Pool{Name: "smoke", RunnerName: "smoke", Image: "smoke", VCPU: 2, MemoryMiB: 2048, OverheadMiB: 512, RootGiB: 20, DiskOverheadGiB: 2, MaxRunners: 1, ExecutionMinutes: 10, Enabled: true}, Image: core.Image{Name: "smoke", Digest: digest, MinimumRootGiB: 20}, Created: now, Deadline: now.Add(timeout), State: "Reserved"}
		ctx, cancel := context.WithTimeout(c.Context(), timeout)
		defer cancel()
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = p.Stop(cleanup, instance.ID)
			_ = p.Delete(cleanup, instance.ID)
		}()
		if e = p.EnsureDiagnostic(ctx, instance); e != nil {
			return e
		}
		for {
			reports, e := p.Inventory(ctx)
			if e != nil {
				return e
			}
			for _, r := range reports {
				if r.ID == instance.ID && r.State == "Stopped" {
					log, e := core.ReadSecret(filepath.Join(conf.StateDir, "logs", instance.ID+".log"))
					if e != nil {
						return e
					}
					if !bytes.Contains(log, []byte("RUNNERLOOM_REAL_VM_COMPLETED")) || !bytes.Contains(log, []byte("RUNNERLOOM_LAN_PROBES_BLOCKED")) {
						return errors.New("実VMの診断完了を確認できません。ログを確認してください")
					}
					if !bytes.Contains(log, []byte("RUNNERLOOM_PUBLIC_HTTPS_OK")) || !bytes.Contains(log, []byte("RUNNERLOOM_CPU_AND_DISK_JOB_OK")) {
						return errors.New("VMの公開HTTPS接続またはCPU・ディスク検証が完了していません")
					}
					if e = p.Delete(ctx, instance.ID); e != nil {
						return e
					}
					return a.output(map[string]any{"realVM": true, "emulator": mode, "started": true, "guestDiagnostic": true, "deleted": true, "instanceID": instance.ID, "log": string(log)})
				}
			}
			timer := time.NewTimer(2 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	})
	cmd.Flags().StringVar(&nodeFile, "config", "", "Node設定JSON")
	cmd.Flags().StringVar(&file, "image", "", "SHA確認済みUbuntu cloud qcow2。Runner未導入でも固定診断可能")
	cmd.Flags().StringVar(&digest, "digest", "", "期待するsha256:...")
	cmd.Flags().DurationVar(&timeout, "timeout", 8*time.Minute, "VM診断の上限")
	cmd.Flags().BoolVar(&tcg, "tcg", false, "KVMなしのソフトウェア仮想化。結果に明示します")
	_ = cmd.MarkFlagRequired("config")
	_ = cmd.MarkFlagRequired("image")
	_ = cmd.MarkFlagRequired("digest")
}
