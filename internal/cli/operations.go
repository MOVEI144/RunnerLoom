package cli

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/MOVEI144/RunnerLoom/internal/agent"
	"github.com/MOVEI144/RunnerLoom/internal/core"
	"github.com/MOVEI144/RunnerLoom/internal/discovery"
	"github.com/MOVEI144/RunnerLoom/internal/host"
	"github.com/spf13/cobra"
)

//go:embed image-build.sh
var imageBuildScript string

func (a *App) addOperations(root *cobra.Command) {
	var discoveryTimeout time.Duration
	discover := add(root, "discover", "LAN上のController候補を表示。参加には招待・鍵の確認が必要", 0, func(c *cobra.Command, _ []string) error {
		v, e := discovery.Discover(c.Context(), discoveryTimeout)
		if e != nil {
			return e
		}
		return a.output(map[string]any{"controllers": v, "trusted": false, "next": "招待ファイルのCA指紋を確認してnode joinを実行"})
	})
	discover.Flags().DurationVar(&discoveryTimeout, "timeout", 5*time.Second, "自動発見の上限時間")

	images, _, e := root.Find([]string{"image"})
	if e != nil {
		panic(e)
	}
	var out, runnerVersion string
	build := add(images, "build", "署名済みUbuntuと公式Runnerから、未登録Golden Imageを作成", 0, func(c *cobra.Command, _ []string) error {
		if !filepath.IsAbs(out) || !strings.HasSuffix(out, ".qcow2") || strings.ContainsAny(out, "\x00\r\n:,") {
			return errors.New("outは改行・コロンを含まない絶対パスの.qcow2にしてください")
		}
		if runnerVersion != "latest" && !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(runnerVersion) {
			return errors.New("runner-versionはlatestまたは数値の版を指定してください")
		}
		if e = core.PrivateDir(filepath.Dir(out)); e != nil {
			return e
		}
		lock, e := core.AcquireLock(filepath.Dir(out), "image-build")
		if e != nil {
			return e
		}
		defer lock.Close()
		ctx, cancel := context.WithTimeout(c.Context(), 45*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/bin/bash", "-s", "--", out, runnerVersion)
		cmd.Stdin = strings.NewReader(imageBuildScript)
		cmd.Stdout = a.Err
		cmd.Stderr = a.Err
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			if cmd.Process == nil {
				return nil
			}
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		}
		cmd.WaitDelay = 10 * time.Second
		if e = cmd.Run(); e != nil {
			return fmt.Errorf("イメージ作成に失敗しました。出力は公開されていないか、manifestで確認してください: %w", e)
		}
		b, e := core.ReadSecret(out + ".manifest.json")
		if e != nil {
			return e
		}
		var manifest map[string]any
		if e = json.Unmarshal(b, &manifest); e != nil {
			return e
		}
		return a.output(map[string]any{"path": out, "manifest": manifest, "registered": false})
	})
	build.Flags().StringVar(&out, "out", "", "新規イメージの絶対パス。既存ファイルは置換しません")
	build.Flags().StringVar(&runnerVersion, "runner-version", "latest", "公式Runnerの安定版。解決した版とハッシュをmanifestへ記録")
	_ = build.MarkFlagRequired("out")
	vm := &cobra.Command{Use: "vm", Short: "実行記録・停止指示・Node上のログと残存VMを管理"}
	root.AddCommand(vm)
	add(vm, "list", "ControllerのVM記録を一覧表示", 0, func(c *cobra.Command, _ []string) error {
		s, e := a.store()
		if e != nil {
			return e
		}
		defer s.Close()
		v, e := s.Instances(c.Context())
		if e != nil {
			return e
		}
		return a.output(v)
	})
	add(vm, "stop ID", "選んだJobを中断してVMの停止を依頼。資源は停止確認まで保持", 1, func(c *cobra.Command, args []string) error {
		if !core.ValidID(args[0]) {
			return errors.New("不正なVM IDです")
		}
		s, e := a.store()
		if e != nil {
			return e
		}
		defer s.Close()
		if e = s.StopInstance(c.Context(), args[0]); e != nil {
			return e
		}
		return a.output(map[string]any{"id": args[0], "stopRequested": true, "resourcesReleased": false})
	})
	var logConfig string
	logs := add(vm, "logs ID", "Node上に残した診断ログを表示（最大16MiB）", 1, func(c *cobra.Command, args []string) error {
		if !core.ValidID(args[0]) {
			return errors.New("不正なVM IDです")
		}
		conf, e := agent.LoadConfig(logConfig)
		if e != nil {
			return e
		}
		path := filepath.Join(conf.StateDir, "logs", args[0]+".log")
		st, e := os.Lstat(path)
		if e != nil {
			return e
		}
		if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
			return errors.New("不正なログファイルです")
		}
		f, e := os.Open(path)
		if e != nil {
			return e
		}
		defer f.Close()
		b, e := io.ReadAll(io.LimitReader(f, 16*(1<<20)+1))
		if e != nil {
			return e
		}
		if len(b) > 16*(1<<20) {
			return errors.New("ログサイズ上限を超えています")
		}
		if a.JSON {
			return a.output(map[string]any{"id": args[0], "log": string(b)})
		}
		_, e = a.Out.Write(b)
		return e
	})
	logs.Flags().StringVar(&logConfig, "config", "", "Nodeのagent.json")
	_ = logs.MarkFlagRequired("config")
	var recoverConfig string
	recover := add(vm, "recover ID", "停止済みの自分のVMだけを片付ける。Agentを止めて実行", 1, func(c *cobra.Command, args []string) error {
		if !core.ValidID(args[0]) {
			return errors.New("不正なVM IDです")
		}
		conf, e := agent.LoadConfig(recoverConfig)
		if e != nil {
			return e
		}
		lock, e := core.AcquireLock(conf.StateDir, "agent")
		if e != nil {
			return e
		}
		defer lock.Close()
		p := localProvider(conf)
		defer p.Close()
		if e = p.Delete(c.Context(), args[0]); e != nil {
			return e
		}
		return a.output(map[string]any{"id": args[0], "deleted": true, "next": "Agentを再起動するとControllerへ削除確認を返します"})
	})
	recover.Flags().StringVar(&recoverConfig, "config", "", "Nodeのagent.json")
	_ = recover.MarkFlagRequired("config")
}
func localProvider(c agent.Config) *host.Libvirt {
	ex := host.SystemExecutor{}
	return &host.Libvirt{StateDir: c.StateDir, DiskDir: c.DiskDir, Node: c.Node, Cluster: c.Cluster, Ceiling: c.Ceiling, Network: host.Network{Cluster: c.Cluster, CIDR: c.NetworkCIDR, Dir: filepath.Join(c.StateDir, "network"), Exec: ex}, Images: &host.Images{Dir: filepath.Join(c.StateDir, "images"), LimitGiB: c.CacheGiB, Exec: ex}, Exec: ex, QEMUUser: c.QEMUUser}
}
