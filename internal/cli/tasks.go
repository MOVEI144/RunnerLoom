package cli

import (
	"bytes"
	"context"
	"crypto/rand"
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

func (a *App) service(dir string) (*tasks.Service, error) { return tasks.NewService(dir) }

func (a *App) addTasks(root *cobra.Command) {
	clientDir := tasks.DefaultDir()

	client := &cobra.Command{Use: "client", Short: "Agentタスクを投げるPC（Claude Code・Codex・ChatGPT側）の登録と許可"}
	root.AddCommand(client)
	client.PersistentFlags().StringVar(&clientDir, "client-dir", clientDir, "このPCのタスククライアント設定ディレクトリ")

	var name string
	initCmd := add(client, "init", "このPCで鍵とCSRを作成。秘密鍵はPCから出ません", 0, func(*cobra.Command, []string) error {
		csr, hash, e := tasks.Init(clientDir, name)
		if e != nil {
			return e
		}
		return a.output(map[string]any{"csr": csr, "csrHash": hash, "next": "CSRファイルをControllerへ渡し、runnerloom client approve で承認してください（CSRは秘密ではありません）"})
	})
	initCmd.Flags().StringVar(&name, "name", "", "このPCの名前（英小文字・数字・ハイフン）")
	_ = initCmd.MarkFlagRequired("name")

	var csrFile, approveName, approveURL, bundleOut string
	var replace bool
	approve := add(client, "approve", "Controller側: CSRを承認し、公開情報だけのbundleを新規ファイルに保存", 0, func(c *cobra.Command, _ []string) error {
		csr, e := os.ReadFile(csrFile)
		if e != nil {
			return e
		}
		parsed, e := core.ParseCSR(csr)
		if e != nil {
			return e
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
		if e = core.PrivateDir(filepath.Dir(bundleOut)); e != nil {
			return e
		}
		if _, e = os.Lstat(bundleOut); !os.IsNotExist(e) {
			return errors.New("bundleの保存先は新規ファイルにしてください")
		}
		cert, cluster, e := s.ApproveClient(c.Context(), ca, approveName, csr, replace)
		if e != nil {
			return e
		}
		b, _ := json.MarshalIndent(tasks.Bundle{Name: approveName, Cluster: cluster, Controller: approveURL, CA: ca.PEM, Fingerprint: core.Hash(ca.Certificate.Raw), Certificate: cert}, "", "  ")
		if e = core.WritePrivate(bundleOut, b); e != nil {
			return e
		}
		return a.output(map[string]any{"name": approveName, "csrHash": core.Hash(parsed.Raw), "bundle": bundleOut, "next": "csrHashがPCで表示された値と同じか確認し、bundleをPCへ渡して runnerloom client install を実行してください"})
	})
	approve.Flags().StringVar(&csrFile, "csr", "", "PCが作成したclient.csr")
	approve.Flags().StringVar(&approveName, "name", "", "PCの名前（client initと同じ）")
	approve.Flags().StringVar(&approveURL, "url", "", "PCから到達できる https://Controller:port")
	approve.Flags().StringVar(&bundleOut, "out", "", "bundleの保存先（新規・絶対パス）")
	approve.Flags().BoolVar(&replace, "replace", false, "同名クライアントの証明書を置き換える")
	for _, f := range []string{"csr", "name", "url", "out"} {
		_ = approve.MarkFlagRequired(f)
	}

	var bundleFile string
	install := add(client, "install", "Controllerから受け取ったbundleを検証して取り込む", 0, func(*cobra.Command, []string) error {
		b, e := os.ReadFile(bundleFile)
		if e != nil {
			return e
		}
		var bundle tasks.Bundle
		if e = core.Decode(bytes.NewReader(b), &bundle); e != nil {
			return e
		}
		conf, e := tasks.Install(clientDir, bundle)
		if e != nil {
			return e
		}
		return a.output(map[string]any{"name": conf.Name, "controller": conf.Controller, "cluster": conf.Cluster, "allow": conf.Allow, "next": "送信を許すエージェントを runnerloom client allow codex のように明示してください"})
	})
	install.Flags().StringVar(&bundleFile, "bundle", "", "client approveが作成したbundle")
	_ = install.MarkFlagRequired("bundle")

	add(client, "show", "このPCのクライアント設定と許可一覧を表示", 0, func(*cobra.Command, []string) error {
		conf, e := tasks.LoadConfig(clientDir)
		if e != nil {
			return e
		}
		return a.output(map[string]any{"dir": clientDir, "config": conf})
	})
	for _, allow := range []bool{true, false} {
		use, desc := "allow NAME...", "指定エージェント（またはgithub）の認証情報をVMへ送ることを許可"
		if !allow {
			use, desc = "disallow NAME...", "送信許可を取り消す"
		}
		value := allow
		cmd := &cobra.Command{Use: use, Short: desc, Args: cobra.MinimumNArgs(1), RunE: func(_ *cobra.Command, args []string) error {
			conf, e := tasks.SetAllow(clientDir, args, value)
			if e != nil {
				return e
			}
			return a.output(map[string]any{"allow": conf.Allow})
		}}
		client.AddCommand(cmd)
	}
	var defPool, defAgent string
	defaults := add(client, "defaults", "既定のPool・エージェントを保存", 0, func(*cobra.Command, []string) error {
		conf, e := tasks.LoadConfig(clientDir)
		if e != nil {
			return e
		}
		if defPool != "" {
			conf.DefaultPool = defPool
		}
		if defAgent != "" {
			conf.DefaultAgent = defAgent
		}
		if e = tasks.SaveConfig(clientDir, conf); e != nil {
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
	add(client, "revoke NAME", "Controller側: クライアントを失効し、待機中タスクを取り消す", 1, func(c *cobra.Command, args []string) error {
		s, e := a.store()
		if e != nil {
			return e
		}
		defer s.Close()
		n, e := s.RevokeClient(c.Context(), args[0])
		if e != nil {
			return e
		}
		return a.output(map[string]any{"client": args[0], "revoked": true, "queuedCancelled": n, "runningVMs": "実行中VMは停止扱いにしません。必要なら vm stop を使ってください"})
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
		backend := &mcp.LazyBackend{Open: func() (mcp.Backend, error) { return a.service(clientDir) }}
		server := mcp.NewRunnerLoom(backend, Version)
		ctx, stop := signal.NotifyContext(c.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if listen == "" {
			return server.ServeStdio(ctx, a.In, a.Out)
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
	serve.Flags().StringVar(&secretFile, "secret-file", filepath.Join(clientDir, "mcp-secret"), "HTTP用の秘密（無ければ0600で生成）")
	serve.Flags().StringSliceVar(&origins, "allow-origin", []string{"https://chatgpt.com", "https://chat.openai.com"}, "許可するブラウザOrigin")
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
