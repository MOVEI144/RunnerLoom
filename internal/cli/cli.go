// Package cli provides the same operations to humans and automation. --json
// never includes JIT credentials, invitation secrets or private keys.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MOVEI144/RunnerLoom/internal/agent"
	"github.com/MOVEI144/RunnerLoom/internal/control"
	"github.com/MOVEI144/RunnerLoom/internal/core"
	"github.com/MOVEI144/RunnerLoom/internal/discovery"
	gh "github.com/MOVEI144/RunnerLoom/internal/github"
	"github.com/MOVEI144/RunnerLoom/internal/host"
	"github.com/spf13/cobra"
)

var Version = "0.1.0-dev"
var Commit = "development"

type App struct {
	State          string
	JSON           bool
	NonInteractive bool
	In             io.Reader
	Out            io.Writer
	Err            io.Writer
}

func (a *App) output(v any) error {
	if !a.JSON {
		if handled, e := humanOutput(a.Out, v); handled {
			return e
		}
	}
	e := json.NewEncoder(a.Out)
	e.SetIndent("", "  ")
	if a.JSON {
		return e.Encode(map[string]any{"ok": true, "data": v})
	}
	return e.Encode(v)
}
func (a *App) store() (*core.Store, error) { return core.OpenStore(a.State) }
func readConfig(path string) (core.Config, error) {
	var c core.Config
	f, e := os.Open(path)
	if e != nil {
		return c, e
	}
	defer f.Close()
	if e = core.Decode(f, &c); e != nil {
		return c, e
	}
	return c, c.Validate()
}
func add(parent *cobra.Command, use, help string, nargs int, fn func(*cobra.Command, []string) error) *cobra.Command {
	c := &cobra.Command{Use: use, Short: help, Args: cobra.ExactArgs(nargs), RunE: fn}
	parent.AddCommand(c)
	return c
}
func (a *App) Command() *cobra.Command {
	root := &cobra.Command{Use: "runnerloom", Short: "自分のPC群を、使い捨てGitHub Actions Runnerとして使う", SilenceErrors: true, SilenceUsage: true}
	root.SetIn(a.In)
	root.SetOut(a.Out)
	root.SetErr(a.Err)
	root.PersistentFlags().StringVar(&a.State, "state", a.State, "Controllerの状態保存先（絶対パス）")
	root.PersistentFlags().BoolVar(&a.JSON, "json", false, "安定したJSON出力。秘密情報は表示しません")
	root.PersistentFlags().BoolVar(&a.NonInteractive, "non-interactive", false, "質問せず、不足設定はエラーとして返す")
	a.addInteractive(root)
	root.RunE = func(c *cobra.Command, _ []string) error {
		if a.shouldStartInteractive() {
			return a.runInteractive(c.Context())
		}
		return c.Help()
	}
	defaultHelp := root.HelpFunc()
	root.SetHelpFunc(func(c *cobra.Command, args []string) {
		if !a.JSON {
			defaultHelp(c, args)
			return
		}
		var commands []map[string]any
		var walk func(*cobra.Command)
		walk = func(c *cobra.Command) {
			if c.RunE != nil || c.Run != nil {
				commands = append(commands, map[string]any{"command": c.CommandPath(), "usage": c.UseLine(), "description": c.Short, "flags": c.Flags().FlagUsages()})
			}
			for _, child := range c.Commands() {
				walk(child)
			}
		}
		walk(root)
		_ = a.output(map[string]any{"version": Version, "commands": commands, "exitCodes": map[string]int{"ok": 0, "invalidInput": 2, "operationFailed": 3, "authorization": 4, "conflict": 5}})
	})
	add(root, "version", "バージョンを表示", 0, func(*cobra.Command, []string) error {
		return a.output(map[string]string{"version": Version, "configVersion": core.Version, "commit": Commit})
	})
	add(root, "doctor", "ホスト環境を読み取り診断する（設定変更なし）", 0, func(*cobra.Command, []string) error { return a.output(core.Doctor()) })
	add(root, "status", "宣言設定・Node・VM・GitHub需要を表示", 0, func(c *cobra.Command, _ []string) error {
		s, e := a.store()
		if e != nil {
			return e
		}
		defer s.Close()
		config, rev, e := s.Config(c.Context())
		if e != nil {
			return e
		}
		nodes, e := s.Nodes(c.Context())
		if e != nil {
			return e
		}
		instances, e := s.Instances(c.Context())
		if e != nil {
			return e
		}
		demand, e := s.Demands(c.Context())
		if e != nil {
			return e
		}
		return a.output(map[string]any{"cluster": config.Name, "revision": rev, "nodes": nodes, "instances": instances, "demand": demand, "configured": rev > 0})
	})
	cfg := &cobra.Command{Use: "config", Short: "JSON設定の検査・変更計画・適用"}
	root.AddCommand(cfg)
	add(cfg, "sample", "編集用の設定例を出力。ImageのSHAは実物へ置換してください", 0, func(*cobra.Command, []string) error {
		enc := json.NewEncoder(a.Out)
		enc.SetIndent("", "  ")
		return enc.Encode(core.Example())
	})
	var file string
	validate := add(cfg, "validate", "設定ファイルを検査（ホスト変更なし）", 0, func(c *cobra.Command, _ []string) error {
		v, e := readConfig(file)
		if e != nil {
			return e
		}
		return a.output(map[string]any{"valid": true, "cluster": v.Name})
	})
	validate.Flags().StringVar(&file, "file", "", "設定JSONのパス")
	_ = validate.MarkFlagRequired("file")
	var planFile string
	plan := add(cfg, "plan", "設定の変更計画を保存。有効期限10分、資源の削除は暗黙実行しません", 0, func(c *cobra.Command, _ []string) error {
		v, e := readConfig(planFile)
		if e != nil {
			return e
		}
		s, e := a.store()
		if e != nil {
			return e
		}
		defer s.Close()
		p, e := s.Plan(c.Context(), v)
		if e != nil {
			return e
		}
		return a.output(p)
	})
	plan.Flags().StringVar(&planFile, "file", "", "設定JSONのパス")
	_ = plan.MarkFlagRequired("file")
	var planID string
	apply := add(cfg, "apply", "保存済み計画を適用。競合・期限切れは拒否、ホスト構築とは別です", 0, func(c *cobra.Command, _ []string) error {
		s, e := a.store()
		if e != nil {
			return e
		}
		defer s.Close()
		r, e := s.Apply(c.Context(), planID)
		if e != nil {
			return e
		}
		return a.output(map[string]any{"revision": r, "hostChanged": false, "restartController": true})
	})
	apply.Flags().StringVar(&planID, "plan", "", "planコマンドが返したID")
	_ = apply.MarkFlagRequired("plan")
	pools := &cobra.Command{Use: "pool", Short: "提供メニューと配置可能なNodeを確認"}
	root.AddCommand(pools)
	add(pools, "list", "Controllerに定義したPoolを表示", 0, func(c *cobra.Command, _ []string) error {
		s, e := a.store()
		if e != nil {
			return e
		}
		defer s.Close()
		v, _, e := s.Config(c.Context())
		if e != nil {
			return e
		}
		return a.output(v.Pools)
	})
	add(pools, "explain NAME", "配置できる・できない理由をNode別に表示", 1, func(c *cobra.Command, args []string) error {
		s, e := a.store()
		if e != nil {
			return e
		}
		defer s.Close()
		v, e := s.Explain(c.Context(), args[0])
		if e != nil {
			return e
		}
		return a.output(v)
	})
	nodes := &cobra.Command{Use: "node", Short: "Nodeの招待・参加承認・排出・失効"}
	root.AddCommand(nodes)
	add(nodes, "list", "Nodeの最後の報告を表示。古い報告は空き扱いしません", 0, func(c *cobra.Command, _ []string) error {
		s, e := a.store()
		if e != nil {
			return e
		}
		defer s.Close()
		v, e := s.Nodes(c.Context())
		if e != nil {
			return e
		}
		return a.output(v)
	})
	for _, drain := range []bool{true, false} {
		name := "resume"
		desc := "Nodeの新規受付を再開"
		if drain {
			name = "drain"
			desc = "新規受付だけを停止。実行中Jobを中断しません"
		}
		value := drain
		add(nodes, name+" NAME", desc, 1, func(c *cobra.Command, args []string) error {
			s, e := a.store()
			if e != nil {
				return e
			}
			defer s.Close()
			if e = s.Drain(c.Context(), args[0], value); e != nil {
				return e
			}
			return a.output(map[string]any{"node": args[0], "drained": value})
		})
	}
	add(nodes, "pending", "承認待ちNodeと鍵の指紋を表示", 0, func(c *cobra.Command, _ []string) error {
		s, e := a.store()
		if e != nil {
			return e
		}
		defer s.Close()
		v, e := s.Pending(c.Context())
		if e != nil {
			return e
		}
		return a.output(v)
	})
	add(nodes, "approve ID", "鍵の指紋を確認した参加申請を承認", 1, func(c *cobra.Command, args []string) error {
		s, e := a.store()
		if e != nil {
			return e
		}
		defer s.Close()
		ca, e := core.LoadCA(a.State)
		if e != nil {
			return e
		}
		v, e := s.Approve(c.Context(), args[0], ca)
		if e != nil {
			return e
		}
		return a.output(map[string]any{"name": v.Name, "status": v.Status, "csrHash": v.CSRHash, "restartController": true})
	})
	add(nodes, "revoke NAME", "Node証明書を失効扱いにし、以後の要求を拒否", 1, func(c *cobra.Command, args []string) error {
		s, e := a.store()
		if e != nil {
			return e
		}
		defer s.Close()
		if e = s.Revoke(c.Context(), args[0]); e != nil {
			return e
		}
		return a.output(map[string]any{"node": args[0], "revoked": true, "existingVMs": "停止したとは扱いません。必要に応じてNodeで回収してください"})
	})
	var endpoint, inviteOut string
	var ttl time.Duration
	invite := add(nodes, "invite", "一回限りの招待を0600のファイルへ保存。秘密は画面に出しません", 0, func(c *cobra.Command, _ []string) error {
		s, e := a.store()
		if e != nil {
			return e
		}
		defer s.Close()
		ca, e := core.LoadCA(a.State)
		if e != nil {
			return e
		}
		if e = core.PrivateDir(filepath.Dir(inviteOut)); e != nil {
			return e
		}
		f, e := os.OpenFile(inviteOut, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			if os.IsExist(e) {
				return errors.New("招待の保存先は新規ファイルにしてください")
			}
			return e
		}
		published := false
		defer func() {
			_ = f.Close()
			if !published {
				_ = os.Remove(inviteOut)
			}
		}()
		v, e := s.Invite(c.Context(), ca, endpoint, ttl)
		if e != nil {
			return e
		}
		b, _ := json.MarshalIndent(v, "", "  ")
		if _, e = f.Write(b); e != nil {
			return e
		}
		if e = f.Sync(); e != nil {
			return e
		}
		published = true
		return a.output(map[string]any{"path": inviteOut, "expires": v.Expires, "fingerprint": v.Fingerprint})
	})
	invite.Flags().StringVar(&endpoint, "url", "", "Nodeから到達できるhttps://Controller:port")
	invite.Flags().StringVar(&inviteOut, "out", "", "招待の保存先（絶対パス）")
	invite.Flags().DurationVar(&ttl, "ttl", 10*time.Minute, "招待の有効期間（1〜60分）")
	_ = invite.MarkFlagRequired("url")
	_ = invite.MarkFlagRequired("out")
	var joinFile, invitationFile string
	join := add(nodes, "join", "招待で参加申請。承認後に同じコマンドで証明書を受け取る", 0, func(c *cobra.Command, _ []string) error {
		conf, e := agent.LoadConfig(joinFile)
		if e != nil {
			return e
		}
		b, e := core.ReadSecret(invitationFile)
		if e != nil {
			return e
		}
		var inv core.Invitation
		if e = core.Decode(bytes.NewReader(b), &inv); e != nil {
			return e
		}
		v, e := agent.Enroll(c.Context(), inv, conf)
		if e != nil {
			return e
		}
		return a.output(map[string]any{"id": v.ID, "name": v.Name, "status": v.Status, "csrHash": v.CSRHash, "next": "PendingApprovalならControllerでnode approve、その後同じjoinを再実行"})
	})
	join.Flags().StringVar(&joinFile, "config", "", "このNodeの設定JSON")
	join.Flags().StringVar(&invitationFile, "invitation", "", "招待ファイル")
	_ = join.MarkFlagRequired("config")
	_ = join.MarkFlagRequired("invitation")
	var backupOut string
	backup := add(root, "backup", "SQLiteの整合したスナップショットを新規ファイルへ保存", 0, func(c *cobra.Command, _ []string) error {
		s, e := a.store()
		if e != nil {
			return e
		}
		defer s.Close()
		if e = s.Backup(c.Context(), backupOut); e != nil {
			return e
		}
		return a.output(map[string]any{"database": backupOut, "requiredSecrets": "復旧にはmaster.key、ca.pem、ca-key.pem、bindings、GitHub認証ファイルも別途安全に保存してください"})
	})
	backup.Flags().StringVar(&backupOut, "out", "", "バックアップの絶対パス")
	_ = backup.MarkFlagRequired("out")
	controller := &cobra.Command{Use: "controller", Short: "管理サービスを起動"}
	root.AddCommand(controller)
	var listen, advertise string
	var offline, discoverable bool
	runController := add(controller, "run", "HTTPS管理サービスとGitHub listenerを常駐起動", 0, func(c *cobra.Command, _ []string) error {
		lock, e := core.AcquireLock(a.State, "controller")
		if e != nil {
			return e
		}
		defer lock.Close()
		s, e := a.store()
		if e != nil {
			return e
		}
		defer s.Close()
		conf, rev, e := s.Config(c.Context())
		if e != nil {
			return e
		}
		if rev == 0 {
			return core.Fail("NOT_CONFIGURED", "setup または config apply が必要です", nil)
		}
		ca, e := core.LoadCA(a.State)
		if e != nil {
			return e
		}
		if discoverable {
			stop, e := discovery.Publish(c.Context(), conf.Name, advertise, core.Hash(ca.Certificate.Raw))
			if e != nil {
				return e
			}
			defer stop()
		}
		server := control.New(s, ca, conf.Name)
		ctx, cancel := context.WithCancel(c.Context())
		defer cancel()
		errs := make(chan error, 2)
		workers := 1
		if !offline {
			manager, e := gh.New(s, conf)
			if e != nil {
				return e
			}
			if e = manager.Check(ctx); e != nil {
				return e
			}
			workers++
			go func() { errs <- manager.Run(ctx) }()
		}
		go func() { errs <- server.Serve(ctx, listen, advertise) }()
		e = <-errs
		cancel()
		for i := 1; i < workers; i++ {
			other := <-errs
			if e == nil {
				e = other
			}
		}
		if errors.Is(e, context.Canceled) {
			return nil
		}
		return e
	})
	runController.Flags().StringVar(&listen, "listen", "127.0.0.1:8443", "明示的なLAN待受アドレス")
	runController.Flags().StringVar(&advertise, "advertise", "https://127.0.0.1:8443", "Nodeに案内するURL")
	runController.Flags().BoolVar(&discoverable, "discoverable", false, "AvahiでLAN上へ公開。発見は参加承認の代わりにはなりません")
	runController.Flags().BoolVar(&offline, "offline", false, "GitHubへ接続せず参加受付だけを行う。テスト・初期設定用")
	agentCmd := &cobra.Command{Use: "agent", Short: "実行Nodeの常駐サービス"}
	root.AddCommand(agentCmd)
	var agentFile string
	var restoreNetwork bool
	runAgent := add(agentCmd, "run", "Controllerへ外向き接続し、VMの作成・停止・削除を実行", 0, func(c *cobra.Command, _ []string) error {
		conf, e := agent.LoadConfig(agentFile)
		if e != nil {
			return e
		}
		lock, e := core.AcquireLock(conf.StateDir, "agent")
		if e != nil {
			return e
		}
		defer lock.Close()
		if restoreNetwork {
			n := host.Network{Cluster: conf.Cluster, CIDR: conf.NetworkCIDR, Dir: filepath.Join(conf.StateDir, "network"), Exec: host.SystemExecutor{}}
			if _, e = core.ReadSecret(filepath.Join(n.Dir, "network-seal.json")); e != nil {
				return errors.New("network applyで明示承認したネットワークだけを復元できます")
			}
			if e = n.Apply(c.Context()); e != nil {
				return e
			}
		}
		ag, e := agent.New(conf)
		if e != nil {
			return e
		}
		ag.Log = slog.New(slog.NewJSONHandler(a.Err, nil))
		return ag.Run(c.Context())
	})
	runAgent.Flags().StringVar(&agentFile, "config", "", "Node設定JSON")
	runAgent.Flags().BoolVar(&restoreNetwork, "restore-network", false, "明示承認済みの専用ネットワークを起動時に復元")
	_ = runAgent.MarkFlagRequired("config")
	network := &cobra.Command{Use: "network", Short: "Runner専用ネットワークだけを計画・設定・検査"}
	root.AddCommand(network)
	for _, operation := range []string{"plan", "apply", "check"} {
		op := operation
		var nodeFile string
		cmd := add(network, op, "専用NAT・LAN隔離。ホストIP、DNS、DHCPは変更しません", 0, func(c *cobra.Command, _ []string) error {
			conf, e := agent.LoadConfig(nodeFile)
			if e != nil {
				return e
			}
			n := host.Network{Cluster: conf.Cluster, CIDR: conf.NetworkCIDR, Dir: filepath.Join(conf.StateDir, "network"), Exec: host.SystemExecutor{}}
			switch op {
			case "plan":
				p, e := n.Plan()
				if e != nil {
					return e
				}
				return a.output(p)
			case "apply":
				lock, e := core.AcquireLock(conf.StateDir, "agent")
				if e != nil {
					return e
				}
				defer lock.Close()
				if e = n.Apply(c.Context()); e != nil {
					return e
				}
			case "check":
				if e = n.Check(c.Context()); e != nil {
					return e
				}
			}
			return a.output(map[string]any{"operation": op, "verified": true})
		})
		cmd.Flags().StringVar(&nodeFile, "config", "", "Node設定JSON")
		_ = cmd.MarkFlagRequired("config")
	}
	images := &cobra.Command{Use: "image", Short: "署名・ハッシュを確認した実行環境イメージの管理"}
	root.AddCommand(images)
	var imageFile, digest string
	var cacheLimit int64
	imp := add(images, "import", "qcow2をSHA-256検証してController配布用キャッシュへ登録", 0, func(c *cobra.Command, _ []string) error {
		f, e := os.Open(imageFile)
		if e != nil {
			return e
		}
		defer f.Close()
		store := &host.Images{Dir: filepath.Join(a.State, "images"), LimitGiB: cacheLimit, Exec: host.SystemExecutor{}}
		path, e := store.Import(c.Context(), f, digest)
		if e != nil {
			return e
		}
		return a.output(map[string]any{"path": path, "digest": digest, "verified": true})
	})
	imp.Flags().StringVar(&imageFile, "file", "", "独立したqcow2ファイル")
	imp.Flags().StringVar(&digest, "digest", "", "sha256:と64桁の期待値")
	imp.Flags().Int64Var(&cacheLimit, "cache-gib", 100, "全Imageのキャッシュ上限GiB")
	_ = imp.MarkFlagRequired("file")
	_ = imp.MarkFlagRequired("digest")
	githubCmd := &cobra.Command{Use: "github", Short: "GitHub接続とRunner Groupの許可範囲を確認"}
	root.AddCommand(githubCmd)
	add(githubCmd, "check", "設定された認証と非公開Repository・Runner Groupを検証", 0, func(c *cobra.Command, _ []string) error {
		s, e := a.store()
		if e != nil {
			return e
		}
		defer s.Close()
		conf, _, e := s.Config(c.Context())
		if e != nil {
			return e
		}
		m, e := gh.New(s, conf)
		if e != nil {
			return e
		}
		if e = m.Check(c.Context()); e != nil {
			return e
		}
		return a.output(map[string]any{"accessVerified": true, "url": conf.GitHub.URL})
	})
	var poolName string
	var setID int
	adopt := add(githubCmd, "adopt", "内容を確認した既存Scale Setを明示的に引き継ぐ", 0, func(c *cobra.Command, _ []string) error {
		s, e := a.store()
		if e != nil {
			return e
		}
		defer s.Close()
		conf, _, e := s.Config(c.Context())
		if e != nil {
			return e
		}
		m, e := gh.New(s, conf)
		if e != nil {
			return e
		}
		if e = m.Adopt(c.Context(), poolName, setID); e != nil {
			return e
		}
		return a.output(map[string]any{"adopted": true, "pool": poolName, "id": setID})
	})
	adopt.Flags().StringVar(&poolName, "pool", "", "Pool名")
	adopt.Flags().IntVar(&setID, "id", 0, "確認済みのScale Set ID")
	_ = adopt.MarkFlagRequired("pool")
	_ = adopt.MarkFlagRequired("id")
	a.addOperations(root)
	a.addMaintenance(root)
	a.addCache(root)
	a.addSetup(root)
	a.addService(root)
	a.addSmoke(root)
	return root
}
func Run(ctx context.Context, args []string, in io.Reader, out, errout io.Writer) int {
	home, _ := os.UserHomeDir()
	a := &App{State: filepath.Join(home, ".local", "state", "runnerloom"), In: in, Out: out, Err: errout}
	root := a.Command()
	root.SetArgs(args)
	if e := root.ExecuteContext(ctx); e != nil {
		var fault *core.Error
		if !errors.As(e, &fault) {
			fault = &core.Error{Code: "OPERATION_FAILED", Message: e.Error()}
		}
		if a.JSON {
			_ = json.NewEncoder(out).Encode(map[string]any{"ok": false, "error": fault})
		} else {
			_, _ = fmt.Fprintln(errout, fault.Error())
		}
		switch fault.Code {
		case "INVALID_CONFIG", "INVALID_JSON", "NOT_CONFIGURED", "MISSING_INPUT":
			return 2
		case "NODE_UNAUTHORIZED", "INVITE_INVALID":
			return 4
		case "NO_CAPACITY", "ALREADY_RUNNING", "REVISION_CONFLICT", "PLAN_EXPIRED", "CACHE_OWNER_RUNNING", "CACHE_UNSAFE", "CACHE_PRUNE_PARTIAL":
			return 5
		}
		if strings.Contains(e.Error(), "unknown flag") || strings.Contains(e.Error(), "required flag") {
			return 2
		}
		return 3
	}
	return 0
}
