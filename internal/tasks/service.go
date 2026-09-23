package tasks

import (
	"context"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/MOVEI144/RunnerLoom/internal/core"
)

// API is the Controller surface the service needs; *Client implements it.
type API interface {
	Pools(context.Context) ([]core.TaskPool, error)
	Submit(context.Context, core.TaskSpec) (core.Task, error)
	Task(context.Context, string) (core.Task, error)
	Tasks(context.Context) ([]core.Task, error)
	Cancel(context.Context, string) (core.Task, error)
}

// Service turns a short request ("run codex on this repository") into a
// validated task spec, enforcing the local allow list before any credential
// is read.
type Service struct {
	Dir      string
	Config   ClientConfig
	API      API
	Profiles map[string]Profile
	Home     string
	Getenv   func(string) string
}

// NewService opens the client in dir with the real environment.
func NewService(dir string) (*Service, error) {
	cl, e := Open(dir)
	if e != nil {
		return nil, e
	}
	profiles, e := LoadProfiles(dir)
	if e != nil {
		cl.Close()
		return nil, e
	}
	home, _ := os.UserHomeDir()
	return &Service{Dir: dir, Config: cl.Config, API: cl, Profiles: profiles, Home: home, Getenv: os.Getenv}, nil
}

type StartRequest struct {
	Agent        string `json:"agent,omitempty"`
	Prompt       string `json:"prompt"`
	Pool         string `json:"pool,omitempty"`
	Repository   string `json:"repository,omitempty"`
	BaseRef      string `json:"baseRef,omitempty"`
	Branch       string `json:"branch,omitempty"`
	PullRequest  bool   `json:"pullRequest,omitempty"`
	ContinueFrom string `json:"continueFrom,omitempty"`
	RequestID    string `json:"requestId,omitempty"`
}

// AgentInfo tells the caller which agents can be used, never secret values.
type AgentInfo struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Builtin     bool        `json:"builtin"`
	Allowed     bool        `json:"allowed"`
	Credentials Credentials `json:"credentialsFound"`
	Problem     string      `json:"problem,omitempty"`
}

func (s *Service) allowed(name string) bool { return core.Contains(s.Config.Allow, name) }

func (s *Service) Agents(context.Context) ([]AgentInfo, error) {
	out := []AgentInfo{}
	for _, p := range SortedProfiles(s.Profiles) {
		info := AgentInfo{Name: p.Name, Description: p.Description, Builtin: p.Builtin, Allowed: s.allowed(p.Name)}
		// Presence only: nothing is read into the reply.
		_, _, found, e := p.Collect(s.Home, s.Getenv)
		info.Credentials = found
		if e != nil {
			info.Problem = e.Error()
		}
		if !info.Allowed {
			info.Problem = strings.TrimSpace("このPCで runnerloom client allow " + p.Name + " を実行するまで送信しません。 " + info.Problem)
		}
		out = append(out, info)
	}
	return out, nil
}

func (s *Service) Pools(ctx context.Context) ([]core.TaskPool, error) { return s.API.Pools(ctx) }
func (s *Service) Get(ctx context.Context, id string) (core.Task, error) {
	return s.API.Task(ctx, id)
}
func (s *Service) List(ctx context.Context) ([]core.Task, error) { return s.API.Tasks(ctx) }
func (s *Service) Cancel(ctx context.Context, id string) (core.Task, error) {
	return s.API.Cancel(ctx, id)
}

var shorthandRepo = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)

// NormalizeRepository accepts owner/name as a github.com shorthand.
func NormalizeRepository(r string) string {
	r = strings.TrimSpace(r)
	if shorthandRepo.MatchString(r) {
		return "https://github.com/" + strings.TrimSuffix(r, ".git") + ".git"
	}
	return r
}

// Start validates locally, collects only the allowed credentials, and submits.
func (s *Service) Start(ctx context.Context, r StartRequest) (core.Task, error) {
	if r.ContinueFrom != "" {
		prev, e := s.API.Task(ctx, r.ContinueFrom)
		if e != nil {
			return core.Task{}, e
		}
		if prev.Repository == "" || prev.Branch == "" {
			return core.Task{}, core.Fail("CANNOT_CONTINUE", "Repositoryを使ったタスクだけ続きを実行できます", r.ContinueFrom)
		}
		if r.Repository == "" {
			r.Repository = prev.Repository
		}
		if r.Branch == "" {
			r.Branch = prev.Branch
		}
		if r.BaseRef == "" {
			r.BaseRef = prev.BaseRef
		}
		if r.Agent == "" {
			r.Agent = prev.Agent
		}
		if r.Pool == "" {
			r.Pool = prev.Pool
		}
	}
	if r.Agent == "" {
		r.Agent = s.Config.DefaultAgent
	}
	p, ok := s.Profiles[r.Agent]
	if !ok {
		return core.Task{}, core.Fail("UNKNOWN_AGENT", "未定義のエージェントです。runnerloom_list_agents で確認してください", r.Agent)
	}
	if !s.allowed(p.Name) {
		return core.Task{}, core.Fail("AGENT_NOT_ALLOWED", "このエージェントの認証情報送信は許可されていません。PCの利用者が runnerloom client allow "+p.Name+" を実行してください", p.Name)
	}
	if r.Pool == "" {
		r.Pool = s.Config.DefaultPool
	}
	if r.Pool == "" {
		pools, e := s.API.Pools(ctx)
		if e != nil {
			return core.Task{}, e
		}
		if len(pools) != 1 {
			return core.Task{}, core.Fail("POOL_REQUIRED", "タスク用Poolを指定してください", pools)
		}
		r.Pool = pools[0].Name
	}
	spec := core.TaskSpec{RequestID: r.RequestID, Pool: r.Pool, Agent: p.Agent(), Prompt: r.Prompt, Repository: NormalizeRepository(r.Repository), BaseRef: r.BaseRef, Branch: r.Branch, PullRequest: r.PullRequest}
	if spec.RequestID == "" {
		spec.RequestID = core.ID()
	}
	if spec.Repository != "" {
		if spec.Branch == "" {
			spec.Branch = "runnerloom/" + time.Now().UTC().Format("20060102-150405") + "-" + core.ID()[:6]
		}
		if s.allowed(GitHubCredential) {
			token, e := GitHubToken(s.Dir, s.Getenv)
			if e != nil {
				return core.Task{}, e
			}
			spec.GitToken = token
		}
		if spec.PullRequest && spec.GitToken == "" {
			return core.Task{}, core.Fail("GITHUB_TOKEN_REQUIRED", "PR作成にはGitHubトークンが必要です。client allow github とGH_TOKENまたはgithub-tokenファイルを設定してください", nil)
		}
	}
	files, env, _, e := p.Collect(s.Home, s.Getenv)
	if e != nil {
		return core.Task{}, e
	}
	spec.Files, spec.Env = files, env
	if e = spec.Validate(); e != nil {
		return core.Task{}, e
	}
	return s.API.Submit(ctx, spec)
}

// Wait polls until the task is terminal or the wait budget ends.
func (s *Service) Wait(ctx context.Context, id string, budget time.Duration) (core.Task, error) {
	deadline := time.Now().Add(budget)
	for {
		t, e := s.API.Task(ctx, id)
		if e != nil || t.Terminal() || !time.Now().Add(3*time.Second).Before(deadline) {
			return t, e
		}
		timer := time.NewTimer(3 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return t, ctx.Err()
		case <-timer.C:
		}
	}
}

// SetAllow adds or removes names from the allow list.
func SetAllow(dir string, names []string, allow bool) (ClientConfig, error) {
	c, e := LoadConfig(dir)
	if e != nil {
		return c, e
	}
	for _, n := range names {
		if n != GitHubCredential && !core.ValidName(n) {
			return c, core.Fail("CLIENT_INPUT", "エージェント名が不正です", n)
		}
		if allow && !core.Contains(c.Allow, n) {
			c.Allow = append(c.Allow, n)
		}
		if !allow {
			kept := []string{}
			for _, v := range c.Allow {
				if v != n {
					kept = append(kept, v)
				}
			}
			c.Allow = kept
		}
	}
	return c, SaveConfig(dir, c)
}
