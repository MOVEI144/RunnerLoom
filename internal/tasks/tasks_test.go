package tasks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MOVEI144/RunnerLoom/internal/core"
	"github.com/MOVEI144/RunnerLoom/internal/testsupport"
)

func TestMain(m *testing.M) { os.Exit(testsupport.Run(m)) }

func code(e error) string {
	if f, ok := e.(*core.Error); ok {
		return f.Code
	}
	return ""
}

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(path, []byte(body), mode); e != nil {
		t.Fatal(e)
	}
}

func TestCollectReadsOnlyDeclaredCredentials(t *testing.T) {
	home := t.TempDir()
	p := Profile{Name: "codex", Run: []string{"codex"}, Files: []FileSpec{{Path: ".codex/auth.json", Optional: true}, {Path: ".codex/other.json", Optional: true}}, Env: []string{"OPENAI_API_KEY"}}
	env := map[string]string{"OPENAI_API_KEY": "k", "UNRELATED_SECRET": "x"}
	getenv := func(k string) string { return env[k] }
	writeFile(t, filepath.Join(home, ".codex/auth.json"), "A", 0600)
	files, vars, found, e := p.Collect(home, getenv)
	if e != nil || len(files) != 1 || string(files[0].Content) != "A" || vars["OPENAI_API_KEY"] != "k" || len(vars) != 1 || len(found.Files) != 1 {
		t.Fatalf("%v %v %+v %v", files, vars, found, e)
	}
	if e = os.Remove(filepath.Join(home, ".codex/auth.json")); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink("/etc/hostname", filepath.Join(home, ".codex/auth.json")); e != nil {
		t.Fatal(e)
	}
	if _, _, _, e = p.Collect(home, getenv); code(e) != "UNSAFE_CREDENTIAL" {
		t.Fatalf("symlinked credential followed: %v", e)
	}
	empty := t.TempDir()
	if _, _, _, e = p.Collect(empty, func(string) string { return "" }); code(e) != "CREDENTIALS_MISSING" {
		t.Fatalf("agent without any credential accepted: %v", e)
	}
	required := Profile{Name: "x", Run: []string{"x"}, Files: []FileSpec{{Path: ".x/key"}}}
	if _, _, _, e = required.Collect(empty, getenv); code(e) != "CREDENTIALS_MISSING" {
		t.Fatalf("missing required file accepted: %v", e)
	}
	writeFile(t, filepath.Join(home, "big"), strings.Repeat("x", core.MaxTaskFile+1), 0600)
	big := Profile{Name: "b", Run: []string{"b"}, Files: []FileSpec{{Path: ".b", From: "~/big"}}}
	if _, _, _, e = big.Collect(home, getenv); code(e) != "CREDENTIAL_TOO_LARGE" {
		t.Fatalf("oversized file accepted: %v", e)
	}
}

func TestProfilesOverlayAndValidation(t *testing.T) {
	dir := t.TempDir()
	for _, p := range Builtins() {
		if e := p.Validate(); e != nil {
			t.Fatalf("builtin %s: %v", p.Name, e)
		}
	}
	writeFile(t, filepath.Join(dir, "agents.json"), `{"agents":[{"name":"grok","run":["grok","-p","{prompt}"],"files":[{"path":".grok/user-settings.json"}],"env":["GROK_API_KEY"]},{"name":"codex","run":["codex","exec","{prompt}"]}]}`, 0600)
	m, e := LoadProfiles(dir)
	if e != nil || m["grok"].Builtin || !m["claude"].Builtin || len(m["codex"].Run) != 3 || m["codex"].Builtin {
		t.Fatalf("%v %v", m, e)
	}
	writeFile(t, filepath.Join(dir, "agents.json"), `{"agents":[{"name":"bad","run":["x"],"files":[{"path":"../.ssh/id_rsa"}]}]}`, 0600)
	if _, e = LoadProfiles(dir); e == nil {
		t.Fatal("profile escaping the home accepted")
	}
	writeFile(t, filepath.Join(dir, "agents.json"), `{"agents":[{"name":"bad","run":["x"],"env":["LD_PRELOAD"]}]}`, 0600)
	if _, e = LoadProfiles(dir); e == nil {
		t.Fatal("reserved env accepted")
	}
	writeFile(t, filepath.Join(dir, "agents.json"), `{"agents":[]}`, 0600)
	if e = os.Chmod(filepath.Join(dir, "agents.json"), 0666); e != nil {
		t.Fatal(e)
	}
	if _, e = LoadProfiles(dir); code(e) != "UNSAFE_AGENTS_FILE" {
		t.Fatalf("world-writable agents.json accepted: %v", e)
	}
}

type fakeAPI struct {
	pools     []core.TaskPool
	submitted []core.TaskSpec
	previous  core.Task
}

func (f *fakeAPI) Pools(context.Context) ([]core.TaskPool, error) { return f.pools, nil }
func (f *fakeAPI) Submit(_ context.Context, s core.TaskSpec) (core.Task, error) {
	f.submitted = append(f.submitted, s)
	return core.Task{ID: core.ID(), Branch: s.Branch}, nil
}
func (f *fakeAPI) Task(context.Context, string) (core.Task, error)   { return f.previous, nil }
func (f *fakeAPI) Tasks(context.Context) ([]core.Task, error)        { return nil, nil }
func (f *fakeAPI) Cancel(context.Context, string) (core.Task, error) { return core.Task{}, nil }

func TestServiceAllowListPoolAndContinuation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "client")
	if _, _, e := Init(dir, "laptop"); e != nil {
		t.Fatal(e)
	}
	if _, _, e := Init(dir, "laptop"); code(e) != "CLIENT_EXISTS" {
		t.Fatal("client key replaced")
	}
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".codex/auth.json"), "A", 0600)
	api := &fakeAPI{pools: []core.TaskPool{{Name: "agent-tasks"}, {Name: "big"}}}
	conf, _ := LoadConfig(dir)
	svc := &Service{Dir: dir, Config: conf, API: api, Profiles: map[string]Profile{"codex": Builtins()[1]}, Home: home, Getenv: func(k string) string { return map[string]string{"GH_TOKEN": "gh"}[k] }}
	ctx := context.Background()
	if _, e := svc.Start(ctx, StartRequest{Agent: "codex", Prompt: "p"}); code(e) != "AGENT_NOT_ALLOWED" {
		t.Fatalf("%v", e)
	}
	var e error
	if svc.Config, e = SetAllow(dir, []string{"codex"}, true); e != nil {
		t.Fatal(e)
	}
	if _, e = svc.Start(ctx, StartRequest{Agent: "codex", Prompt: "p"}); code(e) != "POOL_REQUIRED" {
		t.Fatalf("ambiguous pool accepted: %v", e)
	}
	if _, e = svc.Start(ctx, StartRequest{Agent: "codex", Prompt: "p", Pool: "big", Repository: "o/r"}); e != nil {
		t.Fatal(e)
	}
	s := api.submitted[0]
	if s.GitToken != "" || s.Repository != "https://github.com/o/r.git" || !strings.HasPrefix(s.Branch, "runnerloom/") || s.RequestID == "" {
		t.Fatalf("GitHub token sent without allow github, or defaults wrong: %+v", s)
	}
	if svc.Config, e = SetAllow(dir, []string{"github"}, true); e != nil {
		t.Fatal(e)
	}
	api.previous = core.Task{ID: core.ID(), Repository: "https://github.com/o/r.git", Branch: "runnerloom/x", BaseRef: "main", Agent: "codex", Pool: "big"}
	if _, e = svc.Start(ctx, StartRequest{Prompt: "continue", ContinueFrom: api.previous.ID}); e != nil {
		t.Fatal(e)
	}
	s = api.submitted[1]
	if s.Branch != "runnerloom/x" || s.Pool != "big" || s.Agent.Name != "codex" || s.GitToken != "gh" || s.BaseRef != "main" {
		t.Fatalf("continuation lost its context: %+v", s)
	}
	if svc.Config, e = SetAllow(dir, []string{"codex"}, false); e != nil || core.Contains(svc.Config.Allow, "codex") {
		t.Fatalf("disallow failed: %v", e)
	}
}

func TestNormalizeRepository(t *testing.T) {
	for in, want := range map[string]string{"o/r": "https://github.com/o/r.git", "o/r.git": "https://github.com/o/r.git", "https://gitlab.com/a/b": "https://gitlab.com/a/b", "../x": "../x"} {
		if got := NormalizeRepository(in); got != want {
			t.Fatalf("%s → %s", in, got)
		}
	}
}
