package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/MOVEI144/RunnerLoom/internal/core"
	"github.com/MOVEI144/RunnerLoom/internal/mcp"
	"github.com/MOVEI144/RunnerLoom/internal/tasks"
	"github.com/spf13/cobra"
)

func (a *App) service(dir string) (*tasks.Service, error) {
	resolved, e := tasks.ResolveDir(dir)
	if e != nil {
		return nil, e
	}
	return tasks.NewService(resolved)
}

// checkControllerURL proves, before a bundle is issued, that a client using
// this URL will reach this Controller and verify its certificate name.
func checkControllerURL(ctx context.Context, ca core.CA, raw string) error {
	u, e := (&tasks.Bundle{Controller: raw}).ControllerOrigin()
	if e != nil {
		return e
	}
	cfg, e := core.ClientTLS(ca.PEM, core.Hash(ca.Certificate.Raw), u.Hostname(), nil)
	if e != nil {
		return e
	}
	address := u.Host
	if u.Port() == "" {
		address = net.JoinHostPort(u.Hostname(), "443")
	}
	dialer := tls.Dialer{Config: cfg, NetDialer: &net.Dialer{Timeout: 10 * time.Second}}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, e := dialer.DialContext(ctx, "tcp", address)
	if e != nil {
		return core.Fail("CONTROLLER_URL", "このURLでControllerに接続・証明書確認ができません。controller runの--advertiseのホスト名か--listenのIPを指定してください（確認を省くなら--skip-url-check）: "+e.Error(), raw)
	}
	return conn.Close()
}

func (a *App) addTasks(root *cobra.Command) {
	clientDir := tasks.DefaultDir()

	client := &cobra.Command{Use: "client", Short: "Agentタスクを投げるPC（Claude Code・Codex・ChatGPT側）の登録と許可"}
	root.AddCommand(client)
	client.PersistentFlags().StringVar(&clientDir, "client-dir", clientDir, "このPCのタスククライアント設定ディレクトリ")

	var name string
	initCmd := add(client, "init", "このPCで鍵とCSRを作成。秘密鍵はPCから出ません", 0, func(*cobra.Command, []string) error {
		dir, e := tasks.ResolveDir(clientDir)
		if e != nil {
			return e
		}
		csr, hash, e := tasks.Init(dir, name)
		if e != nil {
			return e
		}
		return a.output(map[string]any{"csr": csr, "csrHash": hash, "next": "CSRファイルとこのcsrHashをControllerの管理者へ渡し、runnerloom client approve --csr-hash で承認してください（CSRは秘密ではありません）"})
	})
	initCmd.Flags().StringVar(&name, "name", "", "このPCの名前（英小文字・数字・ハイフン）")
	_ = initCmd.MarkFlagRequired("name")

	var csrFile, csrHash, approveName, approveURL, bundleOut string
	var replace, skipURLCheck bool
	approve := add(client, "approve", "Controller側: CSRを確認して承認し、改ざん検出用の指紋付きbundleを新規ファイルに保存", 0, func(c *cobra.Command, _ []string) error {
		csr, e := os.ReadFile(csrFile)
		if e != nil {
			return e
		}
		parsed, e := core.ParseCSR(csr)
		if e != nil {
			return e
		}
		// The hash shown on the PC is compared before anything is signed.
		if tasks.NormalizeFingerprint(csrHash) != core.Hash(parsed.Raw) {
			return core.Fail("CSR_MISMATCH", "CSRの指紋がPCで表示された値と一致しません。受け渡しの途中で差し替えられていないか確認してください", nil)
		}
		s, e := a.store()
		if e != nil {
			return e
		}
		defer s.Close()
		ca, e := core.LoadCA(a.State)
		if e != nil {
			return e
		}
		if _, e = (&tasks.Bundle{Controller: approveURL}).ControllerOrigin(); e != nil {
			return e
		}
		if !skipURLCheck {
			if e = checkControllerURL(c.Context(), ca, approveURL); e != nil {
				return e
			}
		}
		out, e := os.OpenFile(bundleOut, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if e != nil {
			if os.IsExist(e) {
				return errors.New("bundleの保存先は新規ファイルにしてください")
			}
			return e
		}
		published := false
		defer func() {
			_ = out.Close()
			if !published {
				_ = os.Remove(bundleOut)
			}
		}()
		cert, cluster, e := s.ApproveClient(c.Context(), ca, approveName, csr, replace)
		if e != nil {
			return e
		}
		fingerprint := core.Hash(ca.Certificate.Raw)
		b, _ := json.MarshalIndent(tasks.Bundle{Name: approveName, Cluster: cluster, Controller: approveURL, CA: ca.PEM, Fingerprint: fingerprint, Certificate: cert}, "", "  ")
		if _, e = out.Write(append(b, '\n')); e == nil {
			e = out.Sync()
		}
		if e != nil {
			return e
		}
		published = true
		return a.output(map[string]any{"name": approveName, "bundle": bundleOut, "caFingerprint": fingerprint, "next": "bundleをPCへ渡し、PCで runnerloom client install --bundle <file> --ca-fingerprint " + fingerprint + " を実行してください（指紋はbundleとは別の経路で伝えてください）"})
	})
	approve.Flags().StringVar(&csrFile, "csr", "", "PCが作成したclient.csr")
	approve.Flags().StringVar(&csrHash, "csr-hash", "", "PCのclient initが表示したcsrHash")
	approve.Flags().StringVar(&approveName, "name", "", "PCの名前（client initと同じ）")
	approve.Flags().StringVar(&approveURL, "url", "", "PCから到達できる https://Controller:port（controller runの--advertiseと同じ名前）")
	approve.Flags().StringVar(&bundleOut, "out", "", "bundleの保存先（新規ファイル）")
	approve.Flags().BoolVar(&replace, "replace", false, "同名クライアントの証明書を置き換える（失効済みの名前は再利用できません）")
	approve.Flags().BoolVar(&skipURLCheck, "skip-url-check", false, "URLへの接続確認を省く（Controller停止中など）")
	for _, f := range []string{"csr", "csr-hash", "name", "url", "out"} {
		_ = approve.MarkFlagRequired(f)
	}

	var bundleFile, caFingerprint string
	var replaceCA bool
	install := add(client, "install", "Controllerから受け取ったbundleを、別経路で受け取ったCA指紋と照合して取り込む", 0, func(*cobra.Command, []string) error {
		dir, e := tasks.ResolveDir(clientDir)
		if e != nil {
			return e
		}
		b, e := os.ReadFile(bundleFile)
		if e != nil {
			return e
		}
		var bundle tasks.Bundle
		if e = core.Decode(bytes.NewReader(b), &bundle); e != nil {
			return e
		}
		conf, e := tasks.Install(dir, bundle, caFingerprint, replaceCA)
		if e != nil {
			return e
		}
		return a.output(map[string]any{"name": conf.Name, "controller": conf.Controller, "cluster": conf.Cluster, "allow": conf.Allow, "next": "送信を許すエージェントを runnerloom client allow codex のように明示してください"})
	})
	install.Flags().StringVar(&bundleFile, "bundle", "", "client approveが作成したbundle")
	install.Flags().StringVar(&caFingerprint, "ca-fingerprint", "", "client approveが表示したcaFingerprint")
	install.Flags().BoolVar(&replaceCA, "replace", false, "別のControllerのCAを置き換える")
	_ = install.MarkFlagRequired("bundle")
	_ = install.MarkFlagRequired("ca-fingerprint")

	add(client, "show", "このPCのクライアント設定と許可一覧を表示", 0, func(*cobra.Command, []string) error {
		dir, e := tasks.ResolveDir(clientDir)
		if e != nil {
			return e
		}
		conf, e := tasks.LoadConfig(dir)
		if e != nil {
			return e
		}
		return a.output(map[string]any{"dir": dir, "config": conf})
	})
	for _, allow := range []bool{true, false} {
		use, desc := "allow NAME...", "指定エージェント（またはgithub）の認証情報をVMへ送ることを許可"
		if !allow {
			use, desc = "disallow NAME...", "送信許可を取り消す"
		}
		value := allow
		cmd := &cobra.Command{Use: use, Short: desc, Args: cobra.MinimumNArgs(1), RunE: func(_ *cobra.Command, args []string) error {
			dir, e := tasks.ResolveDir(clientDir)
			if e != nil {
				return e
			}
			conf, e := tasks.SetAllow(dir, args, value)
			if e != nil {
				return e
			}
			return a.output(map[string]any{"allow": conf.Allow})
		}}
		client.AddCommand(cmd)
	}
	var defPool, defAgent string
	defaults := add(client, "defaults", "既定のPool・エージェントを保存", 0, func(*cobra.Command, []string) error {
		dir, e := tasks.ResolveDir(clientDir)
		if e != nil {
			return e
		}
		conf, e := tasks.LoadConfig(dir)
		if e != nil {
			return e
		}
		if defPool != "" {
			conf.DefaultPool = defPool
		}
		if defAgent != "" {
			conf.DefaultAgent = defAgent
		}
		if e = tasks.SaveConfig(dir, conf); e != nil {
			return e
		}
		return a.output(conf)
	})
	defaults.Flags().StringVar(&defPool, "pool", "", "既定のタスク用Pool")
	defaults.Flags().StringVar(&defAgent, "agent", "", "既定のエージェント")
	add(client, "list", "Controller側: 登録済みクライアントを表示", 0, func(c *cobra.Command, _ []string) error {
		s, e := a.store()
		if e != nil {
			return e
		}
		defer s.Close()
		v, e := s.Clients(c.Context())
		if e != nil {
			return e
		}
		return a.output(v)
	})
	add(client, "revoke NAME", "Controller側: クライアントを失効し、未完了タスクを取り消して実行中VMを停止する", 1, func(c *cobra.Command, args []string) error {
		s, e := a.store()
		if e != nil {
			return e
		}
		defer s.Close()
		n, e := s.RevokeClient(c.Context(), args[0])
		if e != nil {
			return e
		}
		return a.output(map[string]any{"client": args[0], "revoked": true, "tasksCancelled": n, "runningVMs": "停止を指示しました。資源はNodeが停止・削除を確認してから解放されます"})
	})

	task := &cobra.Command{Use: "task", Short: "VM上のコーディングエージェントに長時間タスクを依頼"}
	root.AddCommand(task)
	task.PersistentFlags().StringVar(&clientDir, "client-dir", clientDir, "このPCのタスククライアント設定ディレクトリ")
	add(task, "agents", "使えるエージェント・送信許可・見つかった認証情報（名前のみ）", 0, func(c *cobra.Command, _ []string) error {
		svc, e := a.service(clientDir)
		if e != nil {
			return e
		}
		v, e := svc.Agents(c.Context())
		if e != nil {
			return e
		}
		return a.output(v)
	})
	add(task, "pools", "Controllerが提供するタスク用Pool", 0, func(c *cobra.Command, _ []string) error {
		svc, e := a.service(clientDir)
		if e != nil {
			return e
		}
		v, e := svc.Pools(c.Context())
		if e != nil {
			return e
		}
		return a.output(v)
	})
	var req tasks.StartRequest
	var promptFile string
	start := add(task, "start", "タスクを投げてすぐ戻る。結果は task show で確認", 0, func(c *cobra.Command, _ []string) error {
		if promptFile != "" {
			if req.Prompt != "" {
				return errors.New("--prompt と --prompt-file は片方だけ指定してください")
			}
			b, e := os.ReadFile(promptFile)
			if e != nil {
				return e
			}
			req.Prompt = string(b)
		}
		svc, e := a.service(clientDir)
		if e != nil {
			return e
		}
		v, e := svc.Start(c.Context(), req)
		if e != nil {
			return e
		}
		return a.output(v)
	})
	start.Flags().StringVar(&req.Agent, "agent", "", "エージェント名（codex、claude など）")
	start.Flags().StringVar(&req.Prompt, "prompt", "", "指示")
	start.Flags().StringVar(&promptFile, "prompt-file", "", "指示を書いたファイル")
	start.Flags().StringVar(&req.Repository, "repo", "", "owner/name または https URL")
	start.Flags().StringVar(&req.BaseRef, "base", "", "新しい作業ブランチの基点")
	start.Flags().StringVar(&req.Branch, "branch", "", "push先の作業ブランチ")
	start.Flags().BoolVar(&req.PullRequest, "pr", false, "push後にdraft PRを作成")
	start.Flags().StringVar(&req.Pool, "pool", "", "タスク用Pool")
	start.Flags().StringVar(&req.ContinueFrom, "continue", "", "続きを行う元のタスクID")
	var wait time.Duration
	var patch bool
	show := add(task, "show ID", "状態・途中経過・結果を表示", 1, func(c *cobra.Command, args []string) error {
		svc, e := a.service(clientDir)
		if e != nil {
			return e
		}
		v, e := svc.Wait(c.Context(), args[0], wait)
		if e != nil {
			return e
		}
		if v.Result != nil && !patch && v.Result.Patch != "" {
			v.Result.Patch = "(--patch で表示)"
		}
		return a.output(v)
	})
	show.Flags().DurationVar(&wait, "wait", 0, "終わるまで最大この時間待つ")
	show.Flags().BoolVar(&patch, "patch", false, "push できなかった変更のpatchも表示")
	add(task, "list", "このPCのタスク一覧", 0, func(c *cobra.Command, _ []string) error {
		svc, e := a.service(clientDir)
		if e != nil {
			return e
		}
		v, e := svc.List(c.Context())
		if e != nil {
			return e
		}
		return a.output(v)
	})
	add(task, "cancel ID", "タスクを取り消す。実行中VMは停止を依頼", 1, func(c *cobra.Command, args []string) error {
		svc, e := a.service(clientDir)
		if e != nil {
			return e
		}
		v, e := svc.Cancel(c.Context(), args[0])
		if e != nil {
			return e
		}
		return a.output(v)
	})
	add(task, "list-all", "Controller側: 全クライアントのタスクを表示（秘密情報なし）", 0, func(c *cobra.Command, _ []string) error {
		s, e := a.store()
		if e != nil {
			return e
		}
		defer s.Close()
		v, e := s.Tasks(c.Context(), "", 200)
		if e != nil {
			return e
		}
		return a.output(v)
	})

	mcpCmd := &cobra.Command{Use: "mcp", Short: "Claude Code・Codex・ChatGPTから使うMCPサーバー"}
	root.AddCommand(mcpCmd)
	mcpCmd.PersistentFlags().StringVar(&clientDir, "client-dir", clientDir, "このPCのタスククライアント設定ディレクトリ")
	var listen, secretFile string
	var origins []string
	serve := add(mcpCmd, "serve", "MCPサーバーを起動。既定はstdio、--httpでStreamable HTTP", 0, func(c *cobra.Command, _ []string) error {
		dir, e := tasks.ResolveDir(clientDir)
		if e != nil {
			return e
		}
		backend := &mcp.LazyBackend{Open: func() (mcp.Backend, error) { return tasks.NewService(dir) }}
		server := mcp.NewRunnerLoom(backend, Version)
		ctx, stop := signal.NotifyContext(c.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if listen == "" {
			return server.ServeStdio(ctx, a.In, a.Out)
		}
		if secretFile == "" {
			secretFile = filepath.Join(dir, "mcp-secret")
		}
		secret, e := mcpSecret(secretFile)
		if e != nil {
			return e
		}
		host, _, e := net.SplitHostPort(listen)
		if e != nil {
			return e
		}
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return core.Fail("MCP_LISTEN", "HTTPはloopbackだけで待ち受けます。外部公開はトンネル（ngrok等）経由にしてください", listen)
		}
		ln, e := net.Listen("tcp", listen)
		if e != nil {
			return e
		}
		srv := &http.Server{Handler: server.HTTPHandler(secret, origins), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: mcp.MaxWait + 30*time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
		go func() {
			<-ctx.Done()
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdown)
		}()
		_, _ = io.WriteString(a.Err, "RunnerLoom MCP: http://"+ln.Addr().String()+"/mcp/<secret> (secret: "+secretFile+")\n")
		if e = srv.Serve(ln); errors.Is(e, http.ErrServerClosed) {
			return nil
		}
		return e
	})
	serve.Flags().StringVar(&listen, "http", "", "Streamable HTTPで待ち受けるloopbackアドレス（例: 127.0.0.1:8765）")
	serve.Flags().StringVar(&secretFile, "secret-file", "", "HTTP用の秘密（既定はクライアント設定ディレクトリのmcp-secret。無ければ0600で生成）")
	serve.Flags().StringSliceVar(&origins, "allow-origin", nil, "許可するブラウザOrigin（コネクタはサーバー間通信なので通常は不要）")
}

// mcpSecret reads the HTTP secret or creates a random one (0600).
func mcpSecret(path string) (string, error) {
	b, e := core.ReadSecret(path)
	if e == nil {
		s := strings.TrimSpace(string(b))
		if len(s) < 32 || strings.ContainsAny(s, "/ \t") {
			return "", core.Fail("MCP_SECRET", "MCPの秘密は32文字以上で、/や空白を含められません", path)
		}
		return s, nil
	}
	if !os.IsNotExist(e) {
		return "", e
	}
	raw := make([]byte, 32)
	if _, e = rand.Read(raw); e != nil {
		return "", e
	}
	s := base64.RawURLEncoding.EncodeToString(raw)
	return s, core.WritePrivate(path, []byte(s+"\n"))
}
