package core

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Regression tests for the agent-task review findings. Each one failed on the
// first implementation.

func TestRevokedClientPayloadIsNeverDeliveredAndPlacedVMStops(t *testing.T) {
	s, c := taskStore(t)
	observe(t, s, c, 1)
	task, e := s.SubmitTask(ctx, "laptop", taskSpec("placed"))
	if e != nil || task.Instance == "" {
		t.Fatalf("%+v %v", task, e)
	}
	n, e := s.RevokeClient(ctx, "laptop")
	if e != nil || n != 1 {
		t.Fatalf("revoke cancelled %d %v", n, e)
	}
	r := observe(t, s, c, 2)
	for _, cmd := range r.Commands {
		if cmd.Task != nil || cmd.Action == "ensure" {
			t.Fatalf("revoked client's payload delivered: %+v", cmd)
		}
	}
	runs, _ := s.Instances(ctx)
	if runs[0].State != "Stopping" || runs[0].Held.Empty() {
		t.Fatalf("placed VM not stopped, or resources released early: %+v", runs[0])
	}
}

func TestCancelNeverRelabelsAFinishedTask(t *testing.T) {
	s, c := taskStore(t)
	observe(t, s, c, 1)
	task, _ := s.SubmitTask(ctx, "laptop", taskSpec("done"))
	observe(t, s, c, 2, VMReport{ID: task.Instance, State: "Running"})
	observe(t, s, c, 3, VMReport{ID: task.Instance, State: "Stopped"})
	if e := s.CancelTask(ctx, "laptop", task.ID); e != nil {
		t.Fatal(e)
	}
	if e := s.ReportTask(ctx, "node-a", task.ID, TaskReport{Instance: task.Instance, Result: &TaskResult{Status: "succeeded", Pushed: true}}); e != nil {
		t.Fatal(e)
	}
	observe(t, s, c, 4, VMReport{ID: task.Instance, State: "Deleted"})
	if e := s.CancelTask(ctx, "laptop", task.ID); e != nil {
		t.Fatal(e)
	}
	if v, _ := s.Task(ctx, "laptop", task.ID, false); v.State != "Succeeded" || v.Cancelled {
		t.Fatalf("finished task relabelled: %s cancelled=%v", v.State, v.Cancelled)
	}
}

func TestDeletedWithoutResultIsCollectingUntilGraceEnds(t *testing.T) {
	s, c := taskStore(t)
	observe(t, s, c, 1)
	task, _ := s.SubmitTask(ctx, "laptop", taskSpec("late"))
	observe(t, s, c, 2, VMReport{ID: task.Instance, State: "Stopped"})
	observe(t, s, c, 3, VMReport{ID: task.Instance, State: "Deleted"})
	if v, _ := s.Task(ctx, "laptop", task.ID, false); v.State != "Collecting" || v.Terminal() {
		t.Fatalf("result window closed immediately: %s", v.State)
	}
	now := s.Now
	s.Now = func() time.Time { return now().Add(collectingGrace + time.Minute) }
	if v, _ := s.Task(ctx, "laptop", task.ID, false); v.State != "Finished" {
		t.Fatalf("state after grace: %s", v.State)
	}
	if e := s.ReportTask(ctx, "node-a", task.ID, TaskReport{Instance: task.Instance, Result: &TaskResult{Status: "failed"}}); e != nil {
		t.Fatal(e)
	}
}

func TestPastDeadlineShowsStopping(t *testing.T) {
	s, c := taskStore(t)
	observe(t, s, c, 1)
	task, _ := s.SubmitTask(ctx, "laptop", taskSpec("slow"))
	now := s.Now
	s.Now = func() time.Time { return now().Add(48 * time.Hour) }
	if v, _ := s.Task(ctx, "laptop", task.ID, false); v.State != "Stopping" {
		t.Fatalf("state past deadline: %s", v.State)
	}
}

func TestNewSubmissionWaitsBehindOlderQueuedTasks(t *testing.T) {
	s, c := taskStore(t)
	older, _ := s.SubmitTask(ctx, "laptop", taskSpec("older"))
	observe(t, s, c, 1)
	newer, e := s.SubmitTask(ctx, "laptop", taskSpec("newer"))
	if e != nil || newer.State != "Queued" {
		t.Fatalf("newer task overtook the queue: %+v %v", newer, e)
	}
	if _, e = s.DispatchTasks(ctx); e != nil {
		t.Fatal(e)
	}
	if v, _ := s.Task(ctx, "laptop", older.ID, false); v.State != "Starting" {
		t.Fatalf("oldest task not placed first: %s", v.State)
	}
}

func TestNodeEnrollmentDoesNotOptIntoTaskPools(t *testing.T) {
	s, _ := taskStore(t)
	ca, e := InitCA(filepath.Join(t.TempDir(), "ca"), "home")
	if e != nil {
		t.Fatal(e)
	}
	inv, e := s.Invite(ctx, ca, "https://controller.example:8443", 10*time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	_, csr, _ := NewKeyCSR()
	enrollment, e := s.Join(ctx, JoinRequest{ID: inv.ID, Secret: inv.Secret, Name: "node-b", CSR: csr, Ceiling: Resources{16, 32768, 400}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Approve(ctx, enrollment.ID, ca); e != nil {
		t.Fatal(e)
	}
	conf, _, _ := s.Config(ctx)
	n, _ := conf.Node("node-b")
	if Contains(n.AllowedPools, "agent-tasks") || !Contains(n.AllowedPools, "linux-lite") {
		t.Fatalf("new node pools: %v", n.AllowedPools)
	}
}

func TestTasksFlagCannotChangeOnAPoolInUse(t *testing.T) {
	s, c := taskStore(t)
	observe(t, s, c, 1)
	if _, e := s.SubmitTask(ctx, "laptop", taskSpec("busy")); e != nil {
		t.Fatal(e)
	}
	conf, _, _ := s.Config(ctx)
	for i := range conf.Pools {
		if conf.Pools[i].Name == "agent-tasks" {
			conf.Pools[i].Tasks = false
			conf.Pools[i].RunnerName = "flipped"
		}
	}
	plan, e := s.Plan(ctx, conf)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Apply(ctx, plan.ID); code(e) != "POOL_IN_USE" {
		t.Fatalf("pool kind changed under a live task VM: %v", e)
	}
}

func TestGitTokenOnlyForGitHub(t *testing.T) {
	spec := taskSpec("r")
	spec.Repository = "https://attacker.example/x.git"
	spec.Branch = "runnerloom/x"
	spec.BaseRef = ""
	if spec.Validate() == nil {
		t.Fatal("token accepted for a non-GitHub host")
	}
	spec.GitToken = ""
	if e := spec.Validate(); e != nil {
		t.Fatalf("tokenless clone of another host rejected: %v", e)
	}
}

func TestGuestResultIsBoundToTheTask(t *testing.T) {
	task := Task{Repository: "https://github.com/Example-Org/private-app.git", Branch: "runnerloom/x"}
	r := bindResult(task, TaskResult{Status: "succeeded", Branch: "main", PullRequestURL: "https://github.com/example-org/private-app/pull/12"})
	if r.Branch != "runnerloom/x" || r.PullRequestURL == "" {
		t.Fatalf("%+v", r)
	}
	for _, bad := range []string{"https://evil.example/login", "https://github.com/other/repo/pull/1", "https://github.com/example-org/private-app/pull/1/../../x", "https://github.com/example-org/private-app/pulls"} {
		if bindResult(task, TaskResult{PullRequestURL: bad}).PullRequestURL != "" {
			t.Fatalf("spoofed pull request URL kept: %s", bad)
		}
	}
}

func TestClientRenewalKeepsPreviousCertificateForAGracePeriod(t *testing.T) {
	s, _ := taskStore(t)
	ca, _ := InitCA(filepath.Join(t.TempDir(), "ca"), "home")
	_, csr, _ := NewKeyCSR()
	old, _, e := s.ApproveClient(ctx, ca, "phone", csr, false)
	if e != nil {
		t.Fatal(e)
	}
	renewed, _ := ca.SignClient(csr, "home", "phone")
	if e = s.UpdateClientCertificate(ctx, "phone", renewed); e != nil {
		t.Fatal(e)
	}
	if s.AuthorizeClient(ctx, "phone", old) != nil || s.AuthorizeClient(ctx, "phone", renewed) != nil {
		t.Fatal("a process still holding the previous certificate was locked out")
	}
	now := s.Now
	s.Now = func() time.Time { return now().Add(clientRenewalGrace + time.Hour) }
	if s.AuthorizeClient(ctx, "phone", old) == nil {
		t.Fatal("previous certificate valid forever")
	}
	if _, e = s.RevokeClient(ctx, "phone"); e != nil {
		t.Fatal(e)
	}
	if _, _, e = s.ApproveClient(ctx, ca, "phone", csr, true); code(e) != "CLIENT_REVOKED" {
		t.Fatalf("revoked client name revived: %v", e)
	}
}

func TestTasksOnlyClusterNeedsNoGitHub(t *testing.T) {
	c := Example()
	c.GitHub = GitHub{}
	c.Reservations = nil
	for i := range c.Pools {
		c.Pools[i].Tasks = true
		c.Pools[i].RunnerName = ""
	}
	if e := c.Validate(); e != nil || c.UsesGitHub() {
		t.Fatalf("tasks-only cluster rejected: %v", e)
	}
	c.Pools[0].Tasks = false
	c.Pools[0].RunnerName = "gh"
	if e := c.Validate(); e == nil || !strings.Contains(e.Error(), "INVALID_CONFIG") {
		t.Fatal("GitHub pool accepted without a github block")
	}
}

func TestTaskPoolNeedsTimeForTheGuestMargins(t *testing.T) {
	c := Example()
	p := c.Pools[0]
	p.Name, p.RunnerName, p.WarmIdle, p.Tasks, p.ExecutionMinutes = "agent-tasks", "", 0, true, 29
	c.Pools = append(c.Pools, p)
	c.Nodes[0].AllowedPools = append(c.Nodes[0].AllowedPools, p.Name)
	if e := c.Validate(); e == nil || !strings.Contains(fmt.Sprint(e.(*Error).Details), "agent-tasks.executionMinutes") {
		t.Fatalf("a task pool too short to ever start a task was accepted: %v", e)
	}
	c.Pools[len(c.Pools)-1].ExecutionMinutes = 30
	if e := c.Validate(); e != nil {
		t.Fatal(e.(*Error).Details)
	}
}
