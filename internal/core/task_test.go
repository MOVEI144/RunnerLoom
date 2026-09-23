package core

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func taskStore(t *testing.T) (*Store, Config) {
	t.Helper()
	s, e := OpenStore(filepath.Join(t.TempDir(), "state"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = s.Close() })
	c := Example()
	c.Pools = append(c.Pools, Pool{Name: "agent-tasks", Image: "ubuntu-24", VCPU: 2, MemoryMiB: 2048, OverheadMiB: 512, RootGiB: 20, DiskOverheadGiB: 2, MaxRunners: 3, ExecutionMinutes: 240, Enabled: true, Tasks: true})
	c.Nodes[0].AllowedPools = append(c.Nodes[0].AllowedPools, "agent-tasks")
	p, e := s.Plan(ctx, c)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Apply(ctx, p.ID); e != nil {
		t.Fatal(e)
	}
	// Task clients exist before they can submit; payload delivery checks it.
	for _, name := range []string{"laptop", "desktop"} {
		if _, e = s.DB.Exec("INSERT INTO clients(name,certificate_hash,created) VALUES(?,?,0)", name, "x"); e != nil {
			t.Fatal(e)
		}
	}
	return s, c
}

func taskSpec(request string) TaskSpec {
	return TaskSpec{
		RequestID:  request,
		Pool:       "agent-tasks",
		Agent:      AgentProfile{Name: "codex", Binary: "codex", Runtime: "node", Install: "npm install -g @openai/codex", Run: []string{"codex", "exec", "{prompt}"}},
		Prompt:     "Refactor the parser\nand add tests",
		Repository: "https://github.com/example-org/private-app",
		BaseRef:    "main",
		Branch:     "runnerloom/parser",
		Files:      []TaskFile{{Path: ".codex/auth.json", Content: []byte(`{"tokens":"SECRET-CODEX-TOKEN"}`)}},
		Env:        map[string]string{"OPENAI_API_KEY": "SECRET-ENV-KEY"},
		GitToken:   "SECRET-GIT-TOKEN",
	}
}

func TestTaskLifecycleDeliversPayloadOnlyToOwningNodeAndErasesIt(t *testing.T) {
	s, c := taskStore(t)
	observe(t, s, c, 1)
	task, e := s.SubmitTask(ctx, "laptop", taskSpec("req-1"))
	if e != nil {
		t.Fatal(e)
	}
	if task.State != "Starting" || task.Instance == "" || task.Title != "Refactor the parser" {
		t.Fatalf("unexpected placement: %+v", task)
	}
	var secret, payload []byte
	if e = s.DB.QueryRow("SELECT secret,payload FROM tasks WHERE id=?", task.ID).Scan(&secret, &payload); e != nil {
		t.Fatal(e)
	}
	for _, leak := range []string{"SECRET-CODEX-TOKEN", "SECRET-ENV-KEY", "SECRET-GIT-TOKEN", "Refactor the parser\nand"} {
		if bytes.Contains(secret, []byte(leak)) || bytes.Contains(payload, []byte(leak)) {
			t.Fatalf("task secret stored in plaintext: %s", leak)
		}
	}
	r := observe(t, s, c, 2)
	if len(r.Commands) != 1 || r.Commands[0].Action != "ensure" || r.Commands[0].JIT != "" || r.Commands[0].Task == nil {
		t.Fatalf("expected one task ensure: %+v", r.Commands)
	}
	cmd := r.Commands[0]
	if cmd.Task.ID != task.ID || cmd.Instance.Task != task.ID || cmd.Task.Spec.GitToken != "SECRET-GIT-TOKEN" || cmd.Instance.Pool.Name != "agent-tasks" {
		t.Fatalf("payload not bound to its instance: %+v", cmd)
	}
	id := task.Instance
	observe(t, s, c, 3, VMReport{ID: id, State: "Running"})
	if v, _ := s.Task(ctx, "laptop", task.ID, true); v.State != "Running" {
		t.Fatalf("state %s", v.State)
	}
	if e = s.ReportTask(ctx, "node-a", task.ID, TaskReport{Instance: id, Progress: "cloning..."}); e != nil {
		t.Fatal(e)
	}
	result := &TaskResult{Status: "succeeded", Output: "done", Branch: "runnerloom/parser", Commit: strings.Repeat("a", 40), Changed: true, Pushed: true}
	if code(s.ReportTask(ctx, "node-a", task.ID, TaskReport{Instance: id, Result: result})) != "TASK_STILL_RUNNING" {
		t.Fatal("result accepted before host-confirmed stop")
	}
	observe(t, s, c, 4, VMReport{ID: id, State: "Stopped"})
	if v, _ := s.Task(ctx, "laptop", task.ID, true); v.State != "Collecting" || v.Progress != "cloning..." {
		t.Fatalf("state %s progress %q", v.State, v.Progress)
	}
	if e = s.ReportTask(ctx, "node-a", task.ID, TaskReport{Instance: id, Result: result}); e != nil {
		t.Fatal(e)
	}
	replaced := *result
	replaced.Output = "forged later"
	if e = s.ReportTask(ctx, "node-a", task.ID, TaskReport{Instance: id, Result: &replaced}); e != nil {
		t.Fatal(e)
	}
	observe(t, s, c, 5, VMReport{ID: id, State: "Deleted"})
	if e = s.DB.QueryRow("SELECT secret FROM tasks WHERE id=?", task.ID).Scan(&secret); e != nil || secret != nil {
		t.Fatalf("payload not erased after confirmed deletion: %v", e)
	}
	v, e := s.Task(ctx, "laptop", task.ID, true)
	if e != nil || v.State != "Succeeded" || v.Result == nil || v.Result.Output != "done" || !v.Terminal() {
		t.Fatalf("final task %+v %v", v, e)
	}
	list, e := s.Tasks(ctx, "laptop", 10)
	if e != nil || len(list) != 1 || list[0].Result == nil || list[0].Result.Output != "" {
		t.Fatalf("list must summarize without bodies: %+v %v", list, e)
	}
}

func TestTaskQueueWaitsForCapacityAndExpires(t *testing.T) {
	s, c := taskStore(t)
	queued, e := s.SubmitTask(ctx, "laptop", taskSpec("queued"))
	if e != nil || queued.State != "Queued" || queued.Instance != "" {
		t.Fatalf("unready node must queue: %+v %v", queued, e)
	}
	stale, e := s.SubmitTask(ctx, "laptop", taskSpec("stale"))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec("UPDATE tasks SET created=0 WHERE id=?", stale.ID); e != nil {
		t.Fatal(e)
	}
	var b []byte
	if e = s.DB.QueryRow("SELECT payload FROM tasks WHERE id=?", stale.ID).Scan(&b); e != nil {
		t.Fatal(e)
	}
	old := bytes.Replace(b, []byte(stale.Created.Format(time.RFC3339Nano)), []byte(stale.Created.Add(-2*TaskQueueLifetime).Format(time.RFC3339Nano)), 1)
	if _, e = s.DB.Exec("UPDATE tasks SET payload=? WHERE id=?", old, stale.ID); e != nil {
		t.Fatal(e)
	}
	observe(t, s, c, 1)
	placed, e := s.DispatchTasks(ctx)
	if e != nil || placed != 1 {
		t.Fatalf("placed %d %v", placed, e)
	}
	if v, _ := s.Task(ctx, "laptop", queued.ID, false); v.State != "Starting" {
		t.Fatalf("queued task not placed: %s", v.State)
	}
	v, _ := s.Task(ctx, "laptop", stale.ID, false)
	var secret []byte
	_ = s.DB.QueryRow("SELECT secret FROM tasks WHERE id=?", stale.ID).Scan(&secret)
	if v.State != "Expired" || secret != nil {
		t.Fatalf("expired task retained payload: %s", v.State)
	}
}

func TestTaskIdempotencyOwnershipAndPoolKind(t *testing.T) {
	s, c := taskStore(t)
	observe(t, s, c, 1)
	first, e := s.SubmitTask(ctx, "laptop", taskSpec("same"))
	if e != nil {
		t.Fatal(e)
	}
	again, e := s.SubmitTask(ctx, "laptop", taskSpec("same"))
	if e != nil || again.ID != first.ID {
		t.Fatal("replayed submission created a second task")
	}
	changed := taskSpec("same")
	changed.Prompt = "something else"
	if code(func() error { _, e := s.SubmitTask(ctx, "laptop", changed); return e }()) != "IDEMPOTENCY_CONFLICT" {
		t.Fatal("conflicting replay accepted")
	}
	if _, e = s.Task(ctx, "desktop", first.ID, true); code(e) != "TASK_NOT_FOUND" {
		t.Fatal("another client can read the task")
	}
	if code(s.CancelTask(ctx, "desktop", first.ID)) != "TASK_NOT_FOUND" {
		t.Fatal("another client can cancel the task")
	}
	if code(s.ReportTask(ctx, "node-b", first.ID, TaskReport{Instance: first.Instance, Progress: "x"})) != "TASK_NOT_OWNED" {
		t.Fatal("another node can report the task")
	}
	if _, e = s.Allocate(ctx, ID(), "agent-tasks"); code(e) != "TASK_POOL" {
		t.Fatal("GitHub allocation used a task pool")
	}
	github := taskSpec("github-pool")
	github.Pool = "linux-lite"
	if _, e = s.SubmitTask(ctx, "laptop", github); code(e) != "TASK_POOL" {
		t.Fatal("task used a GitHub pool")
	}
	if e = s.CancelTask(ctx, "laptop", first.ID); e != nil {
		t.Fatal(e)
	}
	runs, _ := s.Instances(ctx)
	for _, a := range runs {
		if a.ID == first.Instance && a.State != "Stopping" {
			t.Fatalf("cancel did not request stop: %s", a.State)
		}
	}
	if a := runs[0]; a.Held.Empty() {
		t.Fatal("cancel released resources before host confirmation")
	}
}

func TestTaskSyncBudgetDefersLargePayloads(t *testing.T) {
	s, c := taskStore(t)
	observe(t, s, c, 1)
	for i := 0; i < 3; i++ {
		spec := taskSpec(ID())
		spec.Files = []TaskFile{{Path: "a", Content: bytes.Repeat([]byte("x"), MaxTaskFile)}, {Path: "b", Content: bytes.Repeat([]byte("y"), MaxTaskFile)}}
		if _, e := s.SubmitTask(ctx, "laptop", spec); e != nil {
			t.Fatal(e)
		}
	}
	r := observe(t, s, c, 2)
	if len(r.Commands) != 2 {
		t.Fatalf("expected the reply to stop at the byte budget, got %d commands", len(r.Commands))
	}
}

func TestTaskSpecValidation(t *testing.T) {
	cases := map[string]func(*TaskSpec){
		"parent-path":     func(s *TaskSpec) { s.Files[0].Path = "../.ssh/id_rsa" },
		"absolute-path":   func(s *TaskSpec) { s.Files[0].Path = "/etc/passwd" },
		"work-tree":       func(s *TaskSpec) { s.Files[0].Path = "work/.env" },
		"control-dir":     func(s *TaskSpec) { s.Files[0].Path = ".runnerloom/prompt" },
		"unclean-path":    func(s *TaskSpec) { s.Files[0].Path = ".codex//auth.json" },
		"path-env":        func(s *TaskSpec) { s.Env["PATH"] = "/tmp" },
		"preload-env":     func(s *TaskSpec) { s.Env["LD_PRELOAD"] = "/tmp/x.so" },
		"internal-env":    func(s *TaskSpec) { s.Env["RUNNERLOOM_TASK_USER"] = "" },
		"http-repo":       func(s *TaskSpec) { s.Repository = "http://github.com/a/b" },
		"userinfo-repo":   func(s *TaskSpec) { s.Repository = "https://token@github.com/a/b" },
		"same-branch":     func(s *TaskSpec) { s.Branch = "main" },
		"dotdot-branch":   func(s *TaskSpec) { s.Branch = "a..b" },
		"option-branch":   func(s *TaskSpec) { s.Branch = "-f" },
		"pr-no-token":     func(s *TaskSpec) { s.PullRequest = true; s.GitToken = "" },
		"pr-not-github":   func(s *TaskSpec) { s.PullRequest = true; s.Repository = "https://gitlab.com/a/b" },
		"branch-no-repo":  func(s *TaskSpec) { s.Repository = "" },
		"empty-prompt":    func(s *TaskSpec) { s.Prompt = " \n" },
		"huge-prompt":     func(s *TaskSpec) { s.Prompt = strings.Repeat("x", MaxTaskPrompt+1) },
		"token-space":     func(s *TaskSpec) { s.GitToken = "a b" },
		"no-run":          func(s *TaskSpec) { s.Agent.Run = nil },
		"binary-path":     func(s *TaskSpec) { s.Agent.Binary = "/usr/bin/codex" },
		"runtime":         func(s *TaskSpec) { s.Agent.Runtime = "python" },
		"request":         func(s *TaskSpec) { s.RequestID = "../x" },
		"duplicate-files": func(s *TaskSpec) { s.Files = append(s.Files, s.Files[0]) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s := taskSpec("r")
			change(&s)
			if s.Validate() == nil {
				t.Fatal("accepted invalid task")
			}
		})
	}
	if e := taskSpec("r").Validate(); e != nil {
		t.Fatal(e)
	}
	plain := TaskSpec{RequestID: "r", Pool: "agent-tasks", Agent: AgentProfile{Name: "claude", Run: []string{"claude", "-p", "{prompt}"}}, Prompt: "research only"}
	if e := plain.Validate(); e != nil {
		t.Fatalf("repository-less task rejected: %v", e)
	}
}

func TestTaskConfigRules(t *testing.T) {
	c := Example()
	c.Pools = append(c.Pools, Pool{Name: "agent-tasks", Image: "ubuntu-24", VCPU: 2, MemoryMiB: 2048, OverheadMiB: 512, RootGiB: 20, DiskOverheadGiB: 2, MaxRunners: 1, ExecutionMinutes: 60, Enabled: true, Tasks: true})
	c.Nodes[0].AllowedPools = append(c.Nodes[0].AllowedPools, "agent-tasks")
	if e := c.Validate(); e != nil {
		t.Fatalf("task pool without runnerName rejected: %v", e)
	}
	c.Pools[2].WarmIdle = 1
	if c.Validate() == nil {
		t.Fatal("task pool with warm idle VMs accepted")
	}
	c.Pools[2].WarmIdle = 0
	c.Pools[0].RunnerName = ""
	if c.Validate() == nil {
		t.Fatal("GitHub pool without runnerName accepted")
	}
	before := Fingerprint(Example().Pools[0])
	b, _ := json.Marshal(Example().Pools[0])
	if strings.Contains(string(b), "tasks") || before != Fingerprint(Example().Pools[0]) {
		t.Fatal("tasks field must not change existing Pool fingerprints")
	}
}

func TestClientIdentityIsSeparateFromNodes(t *testing.T) {
	s, _ := taskStore(t)
	ca, e := InitCA(filepath.Join(t.TempDir(), "ca"), "home")
	if e != nil {
		t.Fatal(e)
	}
	_, csr, e := NewKeyCSR()
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB.Exec("DELETE FROM clients WHERE name='laptop'"); e != nil {
		t.Fatal(e)
	}
	cert, cluster, e := s.ApproveClient(ctx, ca, "laptop", csr, false)
	if e != nil || cluster != "home" {
		t.Fatal(e)
	}
	if e = s.AuthorizeClient(ctx, "laptop", cert); e != nil {
		t.Fatal(e)
	}
	if _, _, e = s.ApproveClient(ctx, ca, "laptop", csr, false); code(e) != "CLIENT_EXISTS" {
		t.Fatal("client silently replaced")
	}
	p, _ := pem.Decode(cert)
	leaf, e := x509.ParseCertificate(p.Bytes)
	if e != nil {
		t.Fatal(e)
	}
	if name, e := ClientIdentity(leaf, "home"); e != nil || name != "laptop" {
		t.Fatalf("client identity %q %v", name, e)
	}
	if _, e = NodeIdentity(leaf, "home"); e == nil {
		t.Fatal("client certificate accepted as a node")
	}
	if _, e = ClientIdentity(leaf, "other"); e == nil {
		t.Fatal("client certificate accepted by another cluster")
	}
	queued, e := s.SubmitTask(ctx, "laptop", taskSpec("before-revoke"))
	if e != nil || queued.State != "Queued" {
		t.Fatalf("%+v %v", queued, e)
	}
	n, e := s.RevokeClient(ctx, "laptop")
	if e != nil || n != 1 {
		t.Fatalf("revoke cancelled %d %v", n, e)
	}
	if code(s.AuthorizeClient(ctx, "laptop", cert)) != "CLIENT_UNAUTHORIZED" {
		t.Fatal("revoked client authorized")
	}
	if v, _ := s.Task(ctx, "laptop", queued.ID, false); v.State != "Cancelled" {
		t.Fatalf("queued task of revoked client: %s", v.State)
	}
}
