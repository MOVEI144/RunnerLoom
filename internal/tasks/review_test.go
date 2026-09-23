package tasks

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/MOVEI144/RunnerLoom/internal/core"
)

func liveService(t *testing.T) (*Service, *fakeAPI, string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "client")
	if _, _, e := Init(dir, "laptop"); e != nil {
		t.Fatal(e)
	}
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".codex/auth.json"), "A", 0600)
	writeFile(t, filepath.Join(home, ".mine/key"), "K", 0600)
	api := &fakeAPI{pools: []core.TaskPool{{Name: "agent-tasks"}}}
	conf, _ := LoadConfig(dir)
	profiles, _ := LoadProfiles(dir)
	return &Service{Dir: dir, Config: conf, API: api, Profiles: profiles, Home: home, Getenv: func(string) string { return "" }, Live: true}, api, dir, home
}

func TestDisallowAppliesToARunningService(t *testing.T) {
	svc, _, dir, _ := liveService(t)
	ctx := context.Background()
	if _, e := SetAllow(dir, []string{"codex"}, true); e != nil {
		t.Fatal(e)
	}
	if _, e := svc.Start(ctx, StartRequest{Agent: "codex", Prompt: "p"}); e != nil {
		t.Fatal(e)
	}
	if _, e := SetAllow(dir, []string{"codex"}, false); e != nil {
		t.Fatal(e)
	}
	if _, e := svc.Start(ctx, StartRequest{Agent: "codex", Prompt: "p"}); code(e) != "AGENT_NOT_ALLOWED" {
		t.Fatalf("revoked consent ignored by the running service: %v", e)
	}
}

func TestChangedCustomProfileNeedsANewAllow(t *testing.T) {
	svc, api, dir, _ := liveService(t)
	ctx := context.Background()
	writeFile(t, filepath.Join(dir, "agents.json"), `{"agents":[{"name":"mine","run":["mine","{prompt}"],"files":[{"path":".mine/key"}]}]}`, 0600)
	if _, e := SetAllow(dir, []string{"mine"}, true); e != nil {
		t.Fatal(e)
	}
	if _, e := svc.Start(ctx, StartRequest{Agent: "mine", Prompt: "p"}); e != nil {
		t.Fatal(e)
	}
	writeFile(t, filepath.Join(dir, "agents.json"), `{"agents":[{"name":"mine","run":["mine","{prompt}"],"files":[{"path":".mine/key"},{"path":".mine/extra","from":"~/.mine/key"}]}]}`, 0600)
	if _, e := svc.Start(ctx, StartRequest{Agent: "mine", Prompt: "p"}); code(e) != "AGENT_CHANGED" {
		t.Fatalf("edited profile ran under the old consent: %v", e)
	}
	if len(api.submitted) != 1 {
		t.Fatal("a task was submitted with the edited profile")
	}
	if _, e := SetAllow(dir, []string{"unknown-agent"}, true); code(e) != "UNKNOWN_AGENT" {
		t.Fatalf("unknown agent allowed: %v", e)
	}
}

func TestSensitiveLocationsAreNeverSent(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".ssh/id_ed25519"), "PRIVATE", 0600)
	for _, p := range []Profile{
		{Name: "a", Run: []string{"a"}, Files: []FileSpec{{Path: ".config/tool/cache.json", From: "~/.ssh/id_ed25519"}}},
		{Name: "b", Run: []string{"b"}, Files: []FileSpec{{Path: ".ssh/id_ed25519"}}},
		{Name: "c", Run: []string{"c"}, Files: []FileSpec{{Path: ".c", From: filepath.Join(home, ".ssh", "id_ed25519")}}},
		{Name: "d", Run: []string{"d"}, Files: []FileSpec{{Path: ".d", From: "/etc/hostname"}}},
	} {
		if _, _, _, e := p.Collect(home, func(string) string { return "" }); e == nil {
			t.Fatalf("profile %s read a sensitive or out-of-home file", p.Name)
		}
	}
	if (Profile{Name: "x", Run: []string{"x"}, Files: []FileSpec{{Path: ".x", From: "~/.aws/credentials"}}}).Validate() == nil {
		t.Fatal("agents.json accepted a cloud credential source")
	}
}

func TestGitHubTokenPrefersTheDedicatedFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "client")
	if e := core.PrivateDir(dir); e != nil {
		t.Fatal(e)
	}
	env := map[string]string{"GH_TOKEN": "broad-gh-cli-token"}
	if v, _ := GitHubToken(dir, func(k string) string { return env[k] }); v != "broad-gh-cli-token" {
		t.Fatalf("env fallback: %q", v)
	}
	env["RUNNERLOOM_GITHUB_TOKEN"] = "dedicated-env"
	if v, _ := GitHubToken(dir, func(k string) string { return env[k] }); v != "dedicated-env" {
		t.Fatalf("dedicated env ignored: %q", v)
	}
	if e := core.WritePrivate(filepath.Join(dir, "github-token"), []byte("fine-grained\n")); e != nil {
		t.Fatal(e)
	}
	if v, _ := GitHubToken(dir, func(k string) string { return env[k] }); v != "fine-grained" {
		t.Fatalf("file not preferred: %q", v)
	}
}

func TestNoTokenForOtherHosts(t *testing.T) {
	svc, api, dir, _ := liveService(t)
	if e := core.WritePrivate(filepath.Join(dir, "github-token"), []byte("t")); e != nil {
		t.Fatal(e)
	}
	if _, e := SetAllow(dir, []string{"codex", "github"}, true); e != nil {
		t.Fatal(e)
	}
	if _, e := svc.Start(context.Background(), StartRequest{Agent: "codex", Prompt: "p", Repository: "https://attacker.example/x.git"}); e != nil {
		t.Fatal(e)
	}
	if api.submitted[0].GitToken != "" {
		t.Fatal("GitHub token attached to a non-GitHub repository")
	}
}

func TestResolveDirFollowsSymlinkedParents(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "home")
	if e := os.Symlink(real, link); e != nil {
		t.Fatal(e)
	}
	dir, e := ResolveDir(filepath.Join(link, ".config", "runnerloom", "client"))
	if e != nil {
		t.Fatal(e)
	}
	want, _ := filepath.EvalSymlinks(real)
	if dir != filepath.Join(want, ".config", "runnerloom", "client") {
		t.Fatalf("resolved %s", dir)
	}
	if _, _, e = Init(dir, "laptop"); e != nil {
		t.Fatalf("symlinked home refused: %v", e)
	}
}
