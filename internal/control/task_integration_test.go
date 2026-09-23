package control_test

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MOVEI144/RunnerLoom/internal/core"
	"github.com/MOVEI144/RunnerLoom/internal/host"
	"github.com/MOVEI144/RunnerLoom/internal/tasks"
)

// fakeTaskProvider is an explicit hypervisor double: no VM boots and no agent
// CLI runs. TLS, handlers, SQLite, the Agent loop and the client are real.
type fakeTaskProvider struct {
	*fakeProvider
	payloads map[string]core.TaskPayload
	progress map[string]string
	results  map[string]*core.TaskResult
	reported map[string]bool
}

func (p *fakeTaskProvider) EnsureTask(_ context.Context, a core.Instance, tp core.TaskPayload) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if tp.ID != a.Task || !a.Pool.Tasks {
		return errors.New("mismatched task payload")
	}
	p.payloads[a.ID] = tp
	if _, ok := p.states[a.ID]; !ok {
		p.created++
		p.states[a.ID] = "Running"
	}
	return nil
}
func (p *fakeTaskProvider) TaskReports(context.Context) ([]host.TaskUpload, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []host.TaskUpload{}
	for id, tp := range p.payloads {
		switch {
		case p.states[id] == "Running" && p.progress[id] != "":
			out = append(out, host.TaskUpload{Task: tp.ID, Report: core.TaskReport{Instance: id, Progress: p.progress[id]}})
		case (p.states[id] == "Stopped" || p.states[id] == "Deleted") && p.results[id] != nil && !p.reported[id]:
			out = append(out, host.TaskUpload{Task: tp.ID, Report: core.TaskReport{Instance: id, Result: p.results[id]}})
		}
	}
	return out, nil
}
func (p *fakeTaskProvider) TaskReported(id string, final bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if final {
		p.reported[id] = true
	} else {
		delete(p.progress, id)
	}
	return nil
}

func (l *lab) addTaskPool(t *testing.T) {
	t.Helper()
	c := l.c
	c.Pools = append(append([]core.Pool{}, c.Pools...), core.Pool{Name: "agent-tasks", Image: "ubuntu-24", VCPU: 2, MemoryMiB: 2048, OverheadMiB: 512, RootGiB: 20, DiskOverheadGiB: 2, MaxRunners: 2, ExecutionMinutes: 120, Enabled: true, Tasks: true})
	c.Nodes = append([]core.Node{}, c.Nodes...)
	c.Nodes[0].AllowedPools = append(append([]string{}, c.Nodes[0].AllowedPools...), "agent-tasks")
	p, e := l.s.Plan(context.Background(), c)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = l.s.Apply(context.Background(), p.ID); e != nil {
		t.Fatal(e)
	}
	l.c = c
}

func (l *lab) taskClient(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(l.dir, "client-"+name)
	csrPath, hash, e := tasks.Init(dir, name)
	if e != nil {
		t.Fatal(e)
	}
	csr, e := os.ReadFile(csrPath)
	if e != nil {
		t.Fatal(e)
	}
	parsed, _ := core.ParseCSR(csr)
	if core.Hash(parsed.Raw) != hash {
		t.Fatal("CSR fingerprint shown to the user differs from the file")
	}
	cert, cluster, e := l.s.ApproveClient(context.Background(), l.ca, name, csr, false)
	if e != nil {
		t.Fatal(e)
	}
	bundle := tasks.Bundle{Name: name, Cluster: cluster, Controller: l.http.URL, CA: l.ca.PEM, Fingerprint: core.Hash(l.ca.Certificate.Raw), Certificate: cert}
	other, _ := core.InitCA(filepath.Join(l.dir, "forged-ca-"+name), "home")
	forged := bundle
	forged.CA, forged.Fingerprint = other.PEM, core.Hash(other.Certificate.Raw)
	if _, e = tasks.Install(dir, forged, core.Hash(l.ca.Certificate.Raw), false); e == nil {
		t.Fatal("a bundle signed by another CA was installed")
	}
	if _, e = tasks.Install(dir, bundle, "sha256:"+core.Hash(l.ca.Certificate.Raw), false); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "client-key.pem"))
	if strings.Contains(string(cert), string(b)) {
		t.Fatal("private key left the client")
	}
	return dir
}

func TestAgentTaskOverMutualTLS(t *testing.T) {
	l := newLab(t)
	l.addTaskPool(t)
	a, base := l.node(t, "node-a", l.c.Nodes[0].Budget)
	p := &fakeTaskProvider{fakeProvider: base, payloads: map[string]core.TaskPayload{}, progress: map[string]string{}, results: map[string]*core.TaskResult{}, reported: map[string]bool{}}
	a.Provider = p
	step(t, a)
	step(t, a)

	dir := l.taskClient(t, "laptop")
	home := t.TempDir()
	if e := os.MkdirAll(filepath.Join(home, ".fake"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(home, ".fake", "auth.json"), []byte("LOCAL-AGENT-SECRET"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(dir, "agents.json"), []byte(`{"agents":[{"name":"fake","run":["fake","{prompt}"],"files":[{"path":".fake/auth.json"}]}]}`), 0600); e != nil {
		t.Fatal(e)
	}
	svc, e := tasks.NewService(dir)
	if e != nil {
		t.Fatal(e)
	}
	svc.Home = home
	svc.Getenv = func(k string) string { return map[string]string{"GH_TOKEN": "LOCAL-GH-TOKEN"}[k] }
	ctx := context.Background()
	req := tasks.StartRequest{Agent: "fake", Prompt: "long refactor", Repository: "example-org/private-app", PullRequest: true}
	if _, e = svc.Start(ctx, req); e == nil || !strings.Contains(e.Error(), "AGENT_NOT_ALLOWED") {
		t.Fatalf("credentials left the PC without an explicit allow: %v", e)
	}
	if _, e = tasks.SetAllow(dir, []string{"fake"}, true); e != nil {
		t.Fatal(e)
	}
	if _, e = svc.Start(ctx, req); e == nil || !strings.Contains(e.Error(), "GITHUB_TOKEN_REQUIRED") {
		t.Fatalf("GitHub token sent without allow github: %v", e)
	}
	if _, e = tasks.SetAllow(dir, []string{"github"}, true); e != nil {
		t.Fatal(e)
	}
	task, e := svc.Start(ctx, req)
	if e != nil {
		t.Fatal(e)
	}
	if task.State != "Starting" || task.Pool != "agent-tasks" || !strings.HasPrefix(task.Branch, "runnerloom/") || task.Repository != "https://github.com/example-org/private-app.git" {
		t.Fatalf("unexpected task %+v", task)
	}
	step(t, a)
	var inst string
	for id, tp := range p.payloads {
		inst = id
		if tp.Spec.GitToken != "LOCAL-GH-TOKEN" || len(tp.Spec.Files) != 1 || string(tp.Spec.Files[0].Content) != "LOCAL-AGENT-SECRET" {
			t.Fatalf("payload did not reach the owning node intact: %+v", tp.Spec)
		}
	}
	if inst == "" || p.created != 1 {
		t.Fatal("task VM not requested")
	}
	p.mu.Lock()
	p.progress[inst] = "[running agent]\nhalfway"
	p.mu.Unlock()
	step(t, a)
	got, e := svc.Get(ctx, task.ID)
	if e != nil || got.State != "Running" || got.Progress != "[running agent]\nhalfway" || got.Node != "node-a" {
		t.Fatalf("%+v %v", got, e)
	}
	p.mu.Lock()
	p.results[inst] = &core.TaskResult{Status: "succeeded", Output: "all done", Branch: task.Branch, Commit: strings.Repeat("c", 40), Changed: true, Pushed: true, PullRequestURL: "https://github.com/example-org/private-app/pull/7"}
	p.states[inst] = "Stopped"
	p.mu.Unlock()
	step(t, a)
	step(t, a)
	got, e = svc.Get(ctx, task.ID)
	if e != nil || got.State != "Succeeded" || got.Result == nil || got.Result.PullRequestURL == "" || got.Result.Output != "all done" {
		t.Fatalf("%+v %v", got, e)
	}
	runs, _ := l.s.Instances(ctx)
	for _, r := range runs {
		if r.ID == inst && (r.State != "Deleted" || !r.Held.Empty()) {
			t.Fatalf("task VM resources not released through host confirmation: %+v", r)
		}
	}

	// Role separation: a Node certificate cannot use the task API, and a
	// client certificate cannot synchronize VMs.
	var out map[string]any
	if e = a.Client.Post(ctx, "/v1/tasks", core.TaskSpec{}, &out); e == nil || !strings.Contains(e.Error(), "CLIENT_UNAUTHORIZED") {
		t.Fatalf("node certificate used the task API: %v", e)
	}
	certPEM, _ := os.ReadFile(filepath.Join(dir, "client.pem"))
	keyPEM, _ := os.ReadFile(filepath.Join(dir, "client-key.pem"))
	pair, e := tls.X509KeyPair(certPEM, keyPEM)
	if e != nil {
		t.Fatal(e)
	}
	cfg, e := core.ClientTLS(l.ca.PEM, core.Hash(l.ca.Certificate.Raw), "127.0.0.1", &pair)
	if e != nil {
		t.Fatal(e)
	}
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
	resp, e := hc.Post(l.http.URL+"/v1/sync", "application/json", strings.NewReader(`{"node":"node-a"}`))
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("client certificate reached node sync: %d", resp.StatusCode)
	}

	// Another client cannot see the task; a revoked client is refused at once.
	other := l.taskClient(t, "desktop")
	otherSvc, e := tasks.NewService(other)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = otherSvc.Get(ctx, task.ID); e == nil || !strings.Contains(e.Error(), "TASK_NOT_FOUND") {
		t.Fatalf("another client read the task: %v", e)
	}
	if _, e = l.s.RevokeClient(ctx, "laptop"); e != nil {
		t.Fatal(e)
	}
	if _, e = svc.Get(ctx, task.ID); e == nil || !strings.Contains(e.Error(), "CLIENT_UNAUTHORIZED") {
		t.Fatalf("revoked client still served: %v", e)
	}
}
