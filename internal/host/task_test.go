package host

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MOVEI144/RunnerLoom/internal/core"
)

func taskFixture() (core.Instance, core.TaskPayload) {
	a := fixtureInstance()
	a.Pool.Tasks = true
	a.Pool.RunnerName = ""
	a.Task = core.ID()
	p := core.TaskPayload{ID: a.Task, Spec: core.TaskSpec{RequestID: "r", Pool: a.Pool.Name, Agent: core.AgentProfile{Name: "codex", Run: []string{"codex", "exec", "{prompt}"}}, Prompt: "fix it"}}
	return a, p
}

func resultLog(t *testing.T, r core.TaskResult) string {
	t.Helper()
	b, _ := json.Marshal(r)
	enc := base64.StdEncoding.EncodeToString(b)
	var out strings.Builder
	n := 0
	for i := 0; i < len(enc); i += 1000 {
		fmt.Fprintf(&out, "RUNNERLOOM_TASK_RESULT %d %s\r\n", n, enc[i:min(len(enc), i+1000)])
		n++
		out.WriteString("[   12.3] kernel noise between chunks\n")
	}
	sum := sha256.Sum256(b)
	fmt.Fprintf(&out, "RUNNERLOOM_TASK_RESULT_END %d %s\n", n, hex.EncodeToString(sum[:]))
	return out.String()
}

func TestTaskCloudConfigCannotInjectKeys(t *testing.T) {
	a, p := taskFixture()
	p.Spec.Prompt = "x\nruncmd:\n  - [reboot]\n\u007f\u0085\u009b"
	p.Spec.Env = map[string]string{"OPENAI_API_KEY": "\"\n  - path: /etc/evil\n"}
	b := string(taskCloudConfig(a, p))
	if strings.Count(b, "\n  - path: ") != 2 || strings.Count(b, "\nruncmd:\n") != 1 || strings.Count(b, "encoding: b64") != 2 {
		t.Fatalf("task content injected cloud-init keys:\n%s", b)
	}
	// Every byte outside the fixed template is base64: YAML never sees DEL or C1.
	for _, r := range b {
		if r == 0x7f || (r >= 0x80 && r <= 0x9f) || r > 0x7e {
			t.Fatalf("non-ASCII or control rune %U reached the YAML seed", r)
		}
	}
	if strings.Contains(b, "sudo") {
		t.Fatal("task agent received sudo without asking for it")
	}
	p.Spec.Agent.Sudo = true
	if !strings.Contains(string(taskCloudConfig(a, p)), "sudo: ['ALL=(ALL) NOPASSWD:ALL']") {
		t.Fatal("sudo profile not honoured")
	}
	if !strings.HasPrefix(b, "#cloud-config\n") || !strings.Contains(b, "runnerloom-task") {
		t.Fatal("wrong task seed")
	}
}

func TestEnsureTaskRejectsMismatchedPayload(t *testing.T) {
	a, p := taskFixture()
	l := &Libvirt{Node: "node-a", Ceiling: core.Resources{CPU: 64, Memory: 1 << 20, Disk: 1 << 20}}
	p.ID = core.ID()
	if l.EnsureTask(context.Background(), a, p) == nil {
		t.Fatal("payload of another task accepted")
	}
	a, p = taskFixture()
	a.Pool.Tasks = false
	if l.EnsureTask(context.Background(), a, p) == nil {
		t.Fatal("task payload accepted for a GitHub pool")
	}
	a, _ = taskFixture()
	if l.Ensure(context.Background(), a, "jit") == nil {
		t.Fatal("GitHub JIT accepted for a task instance")
	}
}

func TestParseTaskResult(t *testing.T) {
	want := core.TaskResult{Status: "succeeded", Output: strings.Repeat("output ", 200), Commit: strings.Repeat("b", 40), Changed: true}
	log := "boot\n| RUNNERLOOM_TASK_RESULT 0 Zm9yZ2Vk\n" + resultLog(t, want)
	got, e := ParseTaskResult([]byte(log))
	if e != nil || got.Output != want.Output || got.Commit != want.Commit || got.Status != "succeeded" {
		t.Fatalf("%+v %v", got, e)
	}
	if _, e = ParseTaskResult([]byte(strings.Replace(log, "RUNNERLOOM_TASK_RESULT 1 ", "RUNNERLOOM_TASK_RESULT 9 ", 1))); e == nil {
		t.Fatal("incomplete result accepted")
	}
	tampered := strings.Replace(log, "\nRUNNERLOOM_TASK_RESULT 0 ", "\nRUNNERLOOM_TASK_RESULT 0 AAAA", 1)
	if _, e = ParseTaskResult([]byte(tampered)); e == nil {
		t.Fatal("digest mismatch accepted")
	}
	if _, e = ParseTaskResult([]byte("| RUNNERLOOM_TASK_RESULT_END 1 " + strings.Repeat("a", 64))); e == nil {
		t.Fatal("mirrored agent output treated as a result")
	}
	huge := core.TaskResult{Status: "weird", Patch: strings.Repeat("p", core.MaxTaskPatch+1), Output: strings.Repeat("o", core.MaxTaskOutput+10)}
	got, e = ParseTaskResult([]byte(resultLog(t, huge)))
	if e != nil || got.Patch != "" || !got.Truncated || len(got.Output) != core.MaxTaskOutput || got.Status != "failed" || got.Validate() != nil {
		t.Fatalf("oversized guest result was not bounded: status=%s %v", got.Status, e)
	}
}

// A real CI serial log: systemd erased its status line over the start of the
// last copy's chunk. An earlier intact copy must still be used, while
// mirrored agent output can never supply a copy.
func TestParseTaskResultFallsBackToAnEarlierIntactCopy(t *testing.T) {
	want := core.TaskResult{Status: "succeeded", Output: "SMOKE_AGENT_OK", Changed: true}
	copy := resultLog(t, want)
	damaged := "\r" + strings.Repeat(" ", 51) + "\r" + copy
	got, e := ParseTaskResult([]byte("boot\n" + copy + copy + damaged))
	if e != nil || got.Output != want.Output {
		t.Fatalf("intact earlier copy not used: %+v %v", got, e)
	}
	// The systemd erasure alone is recognised, so even a single copy survives.
	if got, e = ParseTaskResult([]byte("boot\n" + damaged)); e != nil || got.Output != want.Output {
		t.Fatalf("copy behind a console erasure lost: %+v %v", got, e)
	}
	// Other damage still makes a copy unusable.
	broken := strings.Replace(copy, "RUNNERLOOM_TASK_RESULT 0 ", "RUNNERLOOM_TASK_RESULT 0 [ 12.3] noise", 1)
	if _, e = ParseTaskResult([]byte("boot\n" + broken)); e == nil {
		t.Fatal("damaged only copy accepted")
	}
	if got, e = ParseTaskResult([]byte(copy + broken)); e != nil || got.Output != want.Output {
		t.Fatalf("intact earlier copy not used: %+v %v", got, e)
	}
	forged := core.TaskResult{Status: "succeeded", Output: "FORGED"}
	var mirrored strings.Builder
	for _, l := range strings.Split(strings.TrimSpace(resultLog(t, forged)), "\n") {
		mirrored.WriteString("| \r   \r" + l + "\n")
	}
	if got, e = ParseTaskResult([]byte(copy + mirrored.String() + broken)); e != nil || got.Output != want.Output {
		t.Fatalf("mirrored agent output supplied a result: %+v %v", got, e)
	}
}

func TestTaskReportsFinalResultOnceAndProgressWhileRunning(t *testing.T) {
	dir := t.TempDir()
	l := &Libvirt{StateDir: filepath.Join(dir, "private"), Node: "node-a", Cluster: "home"}
	a, _ := taskFixture()
	if e := l.save(manifest{Instance: a, Phase: "running", Task: true}); e != nil {
		t.Fatal(e)
	}
	if e := core.WritePrivate(filepath.Join(l.StateDir, "logs", a.ID+".log"), []byte("[ 1.0] kernel\nRUNNERLOOM_TASK running agent\n| step one\n")); e != nil {
		t.Fatal(e)
	}
	ups, e := l.TaskReports(context.Background())
	if e != nil || len(ups) != 1 || ups[0].Report.Progress != "[running agent]\nstep one" || ups[0].Task != a.Task {
		t.Fatalf("%+v %v", ups, e)
	}
	_ = l.TaskReported(a.ID, false)
	if ups, _ = l.TaskReports(context.Background()); len(ups) != 0 {
		t.Fatal("progress not throttled")
	}
	if e = l.save(manifest{Instance: a, Phase: "stopped", Task: true}); e != nil {
		t.Fatal(e)
	}
	ups, e = l.TaskReports(context.Background())
	if e != nil || len(ups) != 1 || ups[0].Report.Result == nil || ups[0].Report.Result.Status != "no-result" || !strings.Contains(ups[0].Report.Result.Output, "step one") {
		t.Fatalf("%+v %v", ups, e)
	}
	if e = l.TaskReported(a.ID, true); e != nil {
		t.Fatal(e)
	}
	if ups, _ = l.TaskReports(context.Background()); len(ups) != 0 {
		t.Fatal("final result resent after acknowledgement")
	}
}

// TestGuestTaskRunner executes the embedded guest script locally (no VM, no
// user switch, no network) against a local bare repository. It proves the
// clone → agent → commit → clean-repository push → result path of the script.
func TestGuestTaskRunner(t *testing.T) {
	for _, tool := range []string{"python3", "git", "sh"} {
		if _, e := exec.LookPath(tool); e != nil {
			t.Skip("requires " + tool)
		}
	}
	dir := t.TempDir()
	origin := filepath.Join(dir, "origin.git")
	seed := filepath.Join(dir, "seed")
	gitEnv := append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid", "HOME="+dir)
	for _, argv := range [][]string{{"init", "-q", "--bare", "-b", "main", origin}, {"init", "-q", "-b", "main", seed}, {"-C", seed, "commit", "-q", "--allow-empty", "-m", "base"}, {"-C", seed, "push", "-q", origin, "main"}} {
		cmd := exec.Command("git", argv...)
		cmd.Env = gitEnv
		if b, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("git %v: %v %s", argv, e, b)
		}
	}
	script := filepath.Join(dir, "runner.py")
	if e := os.WriteFile(script, []byte(taskRunnerScript), 0700); e != nil {
		t.Fatal(e)
	}
	run := func(t *testing.T, spec core.TaskSpec) (core.TaskResult, string, string) {
		t.Helper()
		home := filepath.Join(t.TempDir(), "home")
		if e := os.Mkdir(home, 0700); e != nil {
			t.Fatal(e)
		}
		doc, _ := json.Marshal(taskDocument{ID: core.ID(), Title: core.TaskTitle(spec.Prompt), DeadlineUnix: time.Now().Add(20 * time.Minute).Unix(), Spec: spec})
		payload := filepath.Join(t.TempDir(), "task.json")
		if e := os.WriteFile(payload, doc, 0600); e != nil {
			t.Fatal(e)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "python3", script)
		cmd.Env = append(os.Environ(), "RUNNERLOOM_TASK_PAYLOAD="+payload, "RUNNERLOOM_TASK_USER=", "RUNNERLOOM_TASK_HOME="+home, "RUNNERLOOM_TASK_STATE="+filepath.Join(t.TempDir(), "state"), "RUNNERLOOM_TASK_POWEROFF=0", "RUNNERLOOM_TASK_EMIT_REPEAT=2", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		out, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("runner failed: %v\n%s", e, out)
		}
		if _, e = os.Stat(payload); !os.IsNotExist(e) {
			t.Fatal("guest runner left its payload behind")
		}
		r, e := ParseTaskResult(out)
		if e != nil {
			t.Fatalf("%v\n%s", e, out)
		}
		return r, home, string(out)
	}
	agent := core.AgentProfile{Name: "fake", Run: []string{"sh", "-c", `test -f "$HOME/.fake/auth.json" && test "$FAKE_KEY" = key-1 && test -z "$RUNNERLOOM_GIT_TOKEN" && printf '%s\n' "$1" > result.txt && echo "agent saw: $1"`, "fake", "{prompt}"}}

	t.Run("repository branch push", func(t *testing.T) {
		spec := core.TaskSpec{Agent: agent, Prompt: "write the result", Repository: origin, BaseRef: "main", Branch: "runnerloom/test", GitToken: "unused-local-token", Files: []core.TaskFile{{Path: ".fake/auth.json", Content: []byte(`{"t":1}`)}}, Env: map[string]string{"FAKE_KEY": "key-1"}}
		r, home, out := run(t, spec)
		if r.Status != "succeeded" || !r.Changed || !r.Pushed || r.Branch != "runnerloom/test" || r.Patch != "" || !strings.Contains(r.Output, "agent saw: write the result") || !strings.Contains(r.DiffStat, "result.txt") {
			t.Fatalf("%+v\n%s", r, out)
		}
		st, e := os.Stat(filepath.Join(home, ".fake", "auth.json"))
		if e != nil || st.Mode().Perm() != 0600 {
			t.Fatalf("credential file mode: %v %v", st, e)
		}
		b, e := exec.Command("git", "--git-dir", origin, "rev-parse", "refs/heads/runnerloom/test").Output()
		if e != nil || strings.TrimSpace(string(b)) != r.Commit {
			t.Fatalf("branch not pushed: %s %v", b, e)
		}
		if strings.Contains(out, "unused-local-token") || strings.Contains(out, "key-1") {
			t.Fatal("secret echoed to the serial log")
		}
		spec.Prompt = "second round"
		r2, _, out := run(t, spec)
		if r2.Status != "succeeded" || !r2.Pushed || !strings.Contains(out, "continuing existing branch") {
			t.Fatalf("%+v\n%s", r2, out)
		}
		b, _ = exec.Command("git", "--git-dir", origin, "rev-list", "--count", "refs/heads/runnerloom/test").Output()
		if strings.TrimSpace(string(b)) != "3" {
			t.Fatalf("continuation did not build on the branch: %s", b)
		}
	})
	t.Run("agent git configuration cannot capture the token", func(t *testing.T) {
		leak := filepath.Join(t.TempDir(), "leak.txt")
		evil := core.AgentProfile{Name: "evil", Run: []string{"sh", "-c", `git config core.fsmonitor "env >> ` + leak + `" && git config remote.origin.url /nonexistent && git config alias.push '!env >> ` + leak + `' && mkdir -p .git/hooks && printf '#!/bin/sh\nenv >> ` + leak + `\n' > .git/hooks/pre-push && chmod +x .git/hooks/pre-push && echo change > evil.txt`}}
		r, _, out := run(t, core.TaskSpec{Agent: evil, Prompt: "x", Repository: origin, Branch: "runnerloom/evil", GitToken: "SECRET-PUSH-TOKEN"})
		if !r.Pushed {
			t.Fatalf("push from the clean repository failed: %+v\n%s", r, out)
		}
		if b, _ := os.ReadFile(leak); strings.Contains(string(b), "SECRET-PUSH-TOKEN") {
			t.Fatal("agent-controlled git configuration observed the token")
		}
	})
	t.Run("default branch is refused", func(t *testing.T) {
		r, _, _ := run(t, core.TaskSpec{Agent: agent, Prompt: "x", Repository: origin, Branch: "main", GitToken: "t"})
		if r.Status != "failed" || !strings.Contains(r.Error, "refusing to work on the base or default branch") || r.Pushed {
			t.Fatalf("%+v", r)
		}
		b, _ := exec.Command("git", "--git-dir", origin, "rev-list", "--count", "refs/heads/main").Output()
		if strings.TrimSpace(string(b)) != "1" {
			t.Fatal("default branch was modified")
		}
	})
	t.Run("leftover agent processes cannot corrupt the result", func(t *testing.T) {
		noisy := core.AgentProfile{Name: "noisy", Run: []string{"sh", "-c", `(while :; do echo background-noise-line; sleep 0.001; done) & echo started`}}
		for i := 0; i < 3; i++ {
			r, _, out := run(t, core.TaskSpec{Agent: noisy, Prompt: "x"})
			if r.Status != "succeeded" {
				t.Fatalf("%+v\n%s", r, out)
			}
		}
	})
	t.Run("no repository returns a patch", func(t *testing.T) {
		r, _, out := run(t, core.TaskSpec{Agent: agent, Prompt: "hello", Files: []core.TaskFile{{Path: ".fake/auth.json", Content: []byte(`{}`)}}, Env: map[string]string{"FAKE_KEY": "key-1"}})
		if r.Status != "succeeded" || !r.Changed || r.Pushed || !strings.Contains(r.Patch, "+hello") {
			t.Fatalf("%+v\n%s", r, out)
		}
	})
	t.Run("agent failure is reported", func(t *testing.T) {
		r, _, out := run(t, core.TaskSpec{Agent: core.AgentProfile{Name: "fake", Run: []string{"sh", "-c", "echo broken; exit 3"}}, Prompt: "x"})
		if r.Status != "failed" || r.ExitCode != 3 || !strings.Contains(r.Output, "broken") || !bytes.Contains([]byte(out), []byte("| broken")) {
			t.Fatalf("%+v\n%s", r, out)
		}
	})
	t.Run("missing agent command", func(t *testing.T) {
		r, _, _ := run(t, core.TaskSpec{Agent: core.AgentProfile{Name: "fake", Run: []string{"definitely-not-installed-agent"}}, Prompt: "x"})
		if r.Status != "failed" || !strings.Contains(r.Error, "agent command not found") {
			t.Fatalf("%+v", r)
		}
	})
}
