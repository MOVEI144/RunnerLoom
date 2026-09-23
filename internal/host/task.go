package host

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/MOVEI144/RunnerLoom/internal/core"
)

//go:embed task-runner.py
var taskRunnerScript string

// taskDocument is written to the guest only. deadlineUnix is the Controller
// deadline; the guest derives every step's budget from it.
type taskDocument struct {
	ID           string        `json:"id"`
	Title        string        `json:"title"`
	DeadlineUnix int64         `json:"deadlineUnix"`
	Spec         core.TaskSpec `json:"spec"`
}

// MinimumTaskTime refuses to boot a task VM that could not finish its setup.
const MinimumTaskTime = 10 * time.Minute

func taskCloudConfig(a core.Instance, p core.TaskPayload) []byte {
	doc := taskDocument{ID: p.ID, Title: core.TaskTitle(p.Spec.Prompt), DeadlineUnix: a.Deadline.Unix(), Spec: p.Spec}
	b, _ := json.Marshal(doc)
	// Base64 keeps arbitrary prompt bytes (DEL, C1 controls) out of the YAML
	// parser, which would otherwise reject the whole seed.
	b64 := func(v []byte) string { return base64.StdEncoding.EncodeToString(v) }
	sudo := ""
	if p.Spec.Agent.Sudo {
		sudo = "    sudo: ['ALL=(ALL) NOPASSWD:ALL']\n"
	}
	return []byte("#cloud-config\nbootcmd:\n  - [systemctl, mask, --now, serial-getty@ttyS0.service]\n  - [systemctl, mask, --now, ssh.service, ssh.socket]\nssh_pwauth: false\ndisable_root: true\nusers:\n  - name: runner\n    lock_passwd: true\n    shell: /bin/bash\n" + sudo + "write_files:\n  - path: /run/runnerloom-task.json\n    permissions: '0600'\n    encoding: b64\n    content: " + b64(b) + "\n  - path: /usr/local/sbin/runnerloom-task\n    permissions: '0700'\n    encoding: b64\n    content: " + b64([]byte(taskRunnerScript)) + "\nruncmd:\n  - [bash, -c, 'exec /usr/bin/python3 /usr/local/sbin/runnerloom-task >/dev/ttyS0 2>&1']\n")
}

// EnsureTask boots a disposable VM for one agent task. It shares every
// ownership, capacity, image and start-intent rule of a GitHub runner VM.
func (l *Libvirt) EnsureTask(ctx context.Context, a core.Instance, p core.TaskPayload) error {
	if a.Task == "" || p.ID != a.Task || !a.Pool.Tasks {
		return errors.New("task payload does not match its instance")
	}
	if e := p.Spec.Validate(); e != nil {
		return e
	}
	b, _ := json.Marshal(p)
	return l.ensure(ctx, a, job{secret: string(b), kind: "task", userData: func() []byte { return taskCloudConfig(a, p) }})
}

var taskChunk = regexp.MustCompile(`^RUNNERLOOM_TASK_RESULT ([0-9]{1,5}) ([A-Za-z0-9+/=]*)$`)
var taskEnd = regexp.MustCompile(`^RUNNERLOOM_TASK_RESULT_END ([0-9]{1,5}) ([a-f0-9]{64})$`)

// ParseTaskResult extracts the last complete result from a guest serial log.
// The guest is untrusted: a missing, partial or mismatched result is reported
// as such and never changes VM ownership or lifecycle.
func ParseTaskResult(log []byte) (core.TaskResult, error) {
	// Only the region before the last END line can hold the result (at most a
	// few MiB of chunks); splitting the whole 16 MiB log per Step is wasteful.
	if i := bytes.LastIndex(log, []byte("\nRUNNERLOOM_TASK_RESULT_END ")); i >= 0 {
		log = log[max(0, i-(4<<20)):]
	} else if !bytes.HasPrefix(log, []byte("RUNNERLOOM_TASK_RESULT_END ")) {
		return core.TaskResult{}, errors.New("no result in the serial log")
	}
	// The guest prints the result several times because other console
	// writers (systemd status, kernel) can corrupt a copy. The newest intact
	// copy wins. Every such line is the runner's own: agent output is always
	// mirrored behind "| ", so it cannot supply a copy.
	lines := strings.Split(string(log), "\n")
	var last error
	for i := len(lines) - 1; i >= 0; i-- {
		m := taskEnd.FindStringSubmatch(consoleLine(lines[i]))
		if m == nil {
			continue
		}
		r, e := taskResultCopy(lines[:i], m)
		if e == nil {
			return r, nil
		}
		if last == nil {
			last = e
		}
	}
	if last == nil {
		last = errors.New("no result in the serial log")
	}
	return core.TaskResult{}, last
}

// consoleLine removes the trailing CR of a serial line and a leading run of
// CRs and spaces: systemd erases its transient status line that way on the
// same console. A mirrored agent line always starts with "| ", so this never
// turns agent output into a runner line.
func consoleLine(s string) string {
	s = strings.TrimRight(s, "\r")
	if strings.HasPrefix(s, "\r") {
		s = strings.TrimLeft(s, "\r ")
	}
	return s
}

// taskResultCopy decodes the chunks printed just before one END line.
func taskResultCopy(lines []string, m []string) (core.TaskResult, error) {
	n, _ := strconv.Atoi(m[1])
	if n < 1 || n > 16384 {
		return core.TaskResult{}, errors.New("invalid result chunk count")
	}
	chunks := make([]string, n)
	found := 0
	for j := len(lines) - 1; j >= 0 && found < n; j-- {
		line := consoleLine(lines[j])
		c := taskChunk.FindStringSubmatch(line)
		if c == nil {
			if taskEnd.MatchString(line) {
				break
			}
			continue
		}
		k, _ := strconv.Atoi(c[1])
		if k >= n || chunks[k] != "" || len(c[2]) == 0 || len(c[2]) > 1024 {
			continue
		}
		chunks[k] = c[2]
		found++
	}
	if found != n {
		return core.TaskResult{}, errors.New("result chunks are incomplete")
	}
	data, e := base64.StdEncoding.DecodeString(strings.Join(chunks, ""))
	if e != nil {
		return core.TaskResult{}, errors.New("result encoding is corrupt")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != m[2] {
		return core.TaskResult{}, errors.New("result digest mismatch")
	}
	var r core.TaskResult
	d := json.NewDecoder(bytes.NewReader(data))
	if e = d.Decode(&r); e != nil {
		return core.TaskResult{}, errors.New("result document is invalid")
	}
	return boundResult(r), nil
}

func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[len(s)-n:], "")
}

// boundResult coerces a guest document into the Controller's limits.
func boundResult(r core.TaskResult) core.TaskResult {
	if r.Status != "succeeded" && r.Status != "failed" {
		r.Status = "failed"
	}
	r.Error = tailString(r.Error, 4096)
	if len(r.Output) > core.MaxTaskOutput {
		r.Output, r.Truncated = tailString(r.Output, core.MaxTaskOutput), true
	}
	r.DiffStat = tailString(r.DiffStat, 16<<10)
	if len(r.Patch) > core.MaxTaskPatch {
		r.Patch, r.Truncated = "", true
	}
	if r.Branch != "" && !core.ValidGitRef(r.Branch) {
		r.Branch = ""
	}
	if r.Validate() != nil {
		r.Commit, r.PullRequestURL = "", ""
	}
	for {
		b, _ := json.Marshal(core.TaskReport{Instance: strings.Repeat("0", 32), Result: &r})
		if len(b) <= core.MaxTaskReport {
			break
		}
		if r.Patch != "" {
			r.Patch, r.Truncated = "", true
			continue
		}
		r.Output, r.Truncated = tailString(r.Output, len(r.Output)/2), true
	}
	return r
}

// TaskUpload is one report the Agent sends for a local task VM.
type TaskUpload struct {
	Task   string
	Report core.TaskReport
}

func (l *Libvirt) taskMarker(id string) string {
	return filepath.Join(l.StateDir, "task-reports", id+".sent")
}

// readLog opens only the owner-only serial log of an owned VM, refusing
// symlinks, and returns at most the last limit bytes.
func (l *Libvirt) readLog(id string, limit int64) ([]byte, error) {
	if !core.ValidID(id) {
		return nil, errors.New("invalid VM ID")
	}
	path := filepath.Join(l.StateDir, "logs", id+".log")
	fd, e := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if e != nil {
		if errors.Is(e, syscall.ENOENT) {
			return nil, nil
		}
		return nil, e
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() {
		return nil, errors.New("unsafe serial log")
	}
	if st.Size() > limit {
		if _, e = f.Seek(st.Size()-limit, io.SeekStart); e != nil {
			return nil, e
		}
	}
	return io.ReadAll(io.LimitReader(f, limit))
}

// progressText keeps the agent's mirrored output and the runner's status
// lines, dropping kernel noise and result chunks.
func progressText(log []byte) string {
	out := []string{}
	for _, line := range strings.Split(string(log), "\n") {
		line = consoleLine(line)
		switch {
		case strings.HasPrefix(line, "| "):
			out = append(out, line[2:])
		case strings.HasPrefix(line, "RUNNERLOOM_TASK ") && !strings.HasPrefix(line, "RUNNERLOOM_TASK_RESULT"):
			out = append(out, "["+strings.TrimPrefix(line, "RUNNERLOOM_TASK ")+"]")
		}
	}
	return tailString(strings.ToValidUTF8(strings.Join(out, "\n"), ""), core.MaxTaskProgress)
}

// TaskReports returns the final result of every stopped task VM that has not
// been acknowledged, and throttled progress for running ones.
func (l *Libvirt) TaskReports(ctx context.Context) ([]TaskUpload, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries, e := os.ReadDir(filepath.Join(l.StateDir, "instances"))
	if os.IsNotExist(e) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	finals, progress := []TaskUpload{}, []TaskUpload{}
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || !core.ValidID(id) {
			continue
		}
		m, e := l.load(id)
		if e != nil {
			return nil, e
		}
		if !m.Task || m.Instance.Task == "" {
			continue
		}
		if _, e = os.Lstat(l.taskMarker(id)); e == nil {
			// The result is acknowledged; once the VM is deleted its serial
			// log (agent output, patch) has no further use on this Node.
			if m.Phase == "deleted" {
				if e = os.Remove(filepath.Join(l.StateDir, "logs", id+".log")); e != nil && !os.IsNotExist(e) {
					return nil, e
				}
			}
			continue
		} else if !os.IsNotExist(e) {
			return nil, e
		}
		switch m.Phase {
		case "stopped", "deleted":
			log, e := l.readLog(id, 16<<20)
			if e != nil {
				return nil, e
			}
			r, e := ParseTaskResult(log)
			if e != nil {
				r = core.TaskResult{Status: "no-result", ExitCode: -1, Error: "VMが結果を出さずに停止しました（時間切れ・停止指示・異常終了）: " + e.Error(), Output: progressText(log)}
				r = boundResult(r)
				r.Status = "no-result"
			}
			finals = append(finals, TaskUpload{Task: m.Instance.Task, Report: core.TaskReport{Instance: id, Result: &r}})
		case "running", "start-issued":
			if l.progress == nil {
				l.progress = map[string]time.Time{}
			}
			if time.Since(l.progress[id]) < 30*time.Second {
				continue
			}
			log, e := l.readLog(id, 256<<10)
			if e != nil {
				return nil, e
			}
			if text := progressText(log); text != "" {
				progress = append(progress, TaskUpload{Task: m.Instance.Task, Report: core.TaskReport{Instance: id, Progress: text}})
			}
		}
	}
	// Final results first: progress must never crowd them out of a Step.
	return append(finals, progress...), nil
}

// TaskReported records a delivered report. A final result is marked on disk
// so that an Agent restart does not resend it.
func (l *Libvirt) TaskReported(id string, final bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !core.ValidID(id) {
		return errors.New("invalid VM ID")
	}
	if !final {
		if l.progress == nil {
			l.progress = map[string]time.Time{}
		}
		l.progress[id] = time.Now()
		return nil
	}
	delete(l.progress, id)
	return core.WritePrivate(l.taskMarker(id), []byte("sent\n"))
}
