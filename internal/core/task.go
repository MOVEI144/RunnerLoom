package core

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Agent tasks reuse the Instance ledger: an approved client submits a task, the
// Controller places it on a Pool marked "tasks", and the Node boots a disposable
// VM whose guest runs one coding-agent CLI. Credentials in a task travel exactly
// like a JIT configuration: encrypted at rest, delivered only in an ensure
// command to the owning Node, and erased when deletion is confirmed.
const (
	MaxTaskPrompt     = 64 << 10
	MaxTaskFile       = 128 << 10
	MaxTaskFiles      = 256 << 10
	MaxTaskPayload    = 512 << 10
	MaxTaskOutput     = 64 << 10
	MaxTaskPatch      = 384 << 10
	MaxTaskProgress   = 32 << 10
	MaxTaskReport     = 900 << 10
	TaskQueueLifetime = time.Hour
	maxClientTasks    = 64
)

// AgentProfile says how the guest installs and starts one coding-agent CLI.
// "{prompt}" and "{promptFile}" in Run are replaced inside the guest.
type AgentProfile struct {
	Name    string   `json:"name"`
	Binary  string   `json:"binary,omitempty"`
	Runtime string   `json:"runtime,omitempty"`
	Install string   `json:"install,omitempty"`
	Run     []string `json:"run"`
	// Sudo gives the agent user passwordless sudo in the guest. The agent can
	// then read every secret of the task, including the GitHub token.
	Sudo bool `json:"sudo,omitempty"`
}

// TaskFile is placed at the same path relative to the guest runner's home.
type TaskFile struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
}

// TaskSpec is what a client submits. GitToken is used only by the guest's own
// clone/push/pull-request steps and is not placed in the agent's environment.
type TaskSpec struct {
	RequestID   string            `json:"requestID"`
	Pool        string            `json:"pool"`
	Agent       AgentProfile      `json:"agent"`
	Prompt      string            `json:"prompt"`
	Repository  string            `json:"repository,omitempty"`
	BaseRef     string            `json:"baseRef,omitempty"`
	Branch      string            `json:"branch,omitempty"`
	PullRequest bool              `json:"pullRequest,omitempty"`
	Files       []TaskFile        `json:"files,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	GitToken    string            `json:"gitToken,omitempty"`
}

// TaskPayload is the encrypted document delivered to the owning Node.
type TaskPayload struct {
	ID   string   `json:"id"`
	Spec TaskSpec `json:"spec"`
}

// TaskResult is produced by an untrusted guest and bounded before storage.
type TaskResult struct {
	Status         string `json:"status"`
	ExitCode       int    `json:"exitCode"`
	Error          string `json:"error,omitempty"`
	Output         string `json:"output,omitempty"`
	Branch         string `json:"branch,omitempty"`
	Commit         string `json:"commit,omitempty"`
	Changed        bool   `json:"changed"`
	Pushed         bool   `json:"pushed"`
	PullRequestURL string `json:"pullRequestURL,omitempty"`
	DiffStat       string `json:"diffStat,omitempty"`
	Patch          string `json:"patch,omitempty"`
	Truncated      bool   `json:"truncated,omitempty"`
}

// TaskReport is sent by a Node for one of its own task instances.
type TaskReport struct {
	Instance string      `json:"instance"`
	Progress string      `json:"progress,omitempty"`
	Result   *TaskResult `json:"result,omitempty"`
}

// Task is the client-visible record. State is derived from the Instance so
// that tasks never keep a second, divergent lifecycle.
type Task struct {
	ID          string      `json:"id"`
	Client      string      `json:"client"`
	RequestID   string      `json:"requestID"`
	Pool        string      `json:"pool"`
	Agent       string      `json:"agent"`
	Title       string      `json:"title"`
	Repository  string      `json:"repository,omitempty"`
	BaseRef     string      `json:"baseRef,omitempty"`
	Branch      string      `json:"branch,omitempty"`
	PullRequest bool        `json:"pullRequest,omitempty"`
	SpecHash    string      `json:"specHash"`
	Instance    string      `json:"instance,omitempty"`
	Node        string      `json:"node,omitempty"`
	State       string      `json:"state"`
	Cancelled   bool        `json:"cancelled,omitempty"`
	Expired     bool        `json:"expired,omitempty"`
	Created     time.Time   `json:"created"`
	Updated     time.Time   `json:"updated"`
	Deadline    *time.Time  `json:"deadline,omitempty"`
	ProgressAt  *time.Time  `json:"progressAt,omitempty"`
	Progress    string      `json:"progress,omitempty"`
	Result      *TaskResult `json:"result,omitempty"`
}

// Terminal reports whether no further change is expected.
func (t Task) Terminal() bool {
	switch t.State {
	case "Succeeded", "Failed", "Cancelled", "Expired", "Finished":
		return true
	}
	return false
}

var requestPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
var binaryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
var gitRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
var envPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
var commitPattern = regexp.MustCompile(`^[a-f0-9]{0,64}$`)

func ValidGitRef(s string) bool {
	return gitRefPattern.MatchString(s) && !strings.Contains(s, "..") && !strings.Contains(s, "//") && !strings.Contains(s, "/.") && !strings.HasSuffix(s, "/") && !strings.HasSuffix(s, ".") && !strings.HasSuffix(s, ".lock")
}

// ValidHomePath accepts a normalized path below the guest user's home. The
// work tree and RunnerLoom's own directory are reserved so that a credential
// file can never be committed or replace the runner's control files.
func ValidHomePath(p string) bool {
	if p == "" || len(p) > 256 || strings.ContainsAny(p, "\x00\\\r\n") || strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	first := strings.Split(p, "/")[0]
	return first != "work" && first != ".runnerloom"
}

// RepositoryPath returns owner/name for a github.com HTTPS repository URL.
func RepositoryPath(raw string) (string, bool) {
	u, e := url.Parse(raw)
	if e != nil || !strings.EqualFold(u.Host, "github.com") {
		return "", false
	}
	parts := strings.Split(strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	return parts[0] + "/" + parts[1], true
}

func validRepository(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && len(raw) <= 512 && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && len(strings.Trim(u.Path, "/")) > 0 && !strings.ContainsAny(raw, " \t\r\n\\'\"`$")
}

func reservedEnv(k string) bool {
	switch strings.ToUpper(k) {
	case "HOME", "USER", "LOGNAME", "PATH", "SHELL", "PWD", "TERM", "LANG":
		return true
	}
	u := strings.ToUpper(k)
	return strings.HasPrefix(u, "LD_") || strings.HasPrefix(u, "RUNNERLOOM_")
}

func (p AgentProfile) Validate() error {
	bad := map[string]string{}
	check := func(ok bool, key, msg string) {
		if !ok {
			bad[key] = msg
		}
	}
	check(ValidName(p.Name), "agent.name", "英小文字・数字・ハイフンで指定してください")
	check(p.Binary == "" || binaryPattern.MatchString(p.Binary), "agent.binary", "コマンド名だけを指定してください")
	check(p.Runtime == "" || p.Runtime == "node", "agent.runtime", "runtimeは空またはnodeです")
	check(len(p.Install) <= 16<<10 && utf8.ValidString(p.Install) && !strings.ContainsRune(p.Install, 0), "agent.install", "導入手順が長すぎるか不正です")
	check(len(p.Run) >= 1 && len(p.Run) <= 64, "agent.run", "実行コマンドを1〜64要素で指定してください")
	for _, v := range p.Run {
		check(v != "" && len(v) <= 4096 && utf8.ValidString(v) && !strings.ContainsRune(v, 0), "agent.run", "実行コマンドの要素が不正です")
	}
	if len(bad) > 0 {
		return Fail("INVALID_TASK", "エージェント定義を修正してください", bad)
	}
	return nil
}

func (t TaskSpec) Validate() error {
	if e := t.Agent.Validate(); e != nil {
		return e
	}
	bad := map[string]string{}
	check := func(ok bool, key, msg string) {
		if !ok {
			bad[key] = msg
		}
	}
	check(requestPattern.MatchString(t.RequestID), "requestID", "要求IDが不正です")
	check(ValidName(t.Pool), "pool", "Pool名が不正です")
	check(strings.TrimSpace(t.Prompt) != "" && len(t.Prompt) <= MaxTaskPrompt && utf8.ValidString(t.Prompt) && !strings.ContainsRune(t.Prompt, 0), "prompt", "指示は1文字以上64KiB以下のUTF-8です")
	if t.Repository == "" {
		check(t.BaseRef == "" && t.Branch == "" && !t.PullRequest, "repository", "ブランチやPRにはRepositoryが必要です")
	} else {
		check(validRepository(t.Repository), "repository", "https://のRepository URLを指定してください（認証情報をURLに含めない）")
		check(t.BaseRef == "" || ValidGitRef(t.BaseRef), "baseRef", "基点ブランチ名が不正です")
		check(ValidGitRef(t.Branch), "branch", "作業ブランチ名が不正です")
		check(t.Branch != t.BaseRef, "branch", "作業ブランチは基点と別にしてください")
		if t.PullRequest {
			_, ok := RepositoryPath(t.Repository)
			check(ok && t.GitToken != "", "pullRequest", "PR作成はgithub.comのRepositoryとGitHubトークンが必要です")
		}
	}
	check(len(t.GitToken) <= 1024 && !strings.ContainsFunc(t.GitToken, func(r rune) bool { return r <= ' ' || r > '~' }), "gitToken", "GitHubトークンの形式が不正です")
	if t.GitToken != "" {
		// The guest credential helper answers only this host; refusing other
		// hosts here keeps a caller from sending the token anywhere else.
		_, onGitHub := RepositoryPath(t.Repository)
		check(onGitHub, "gitToken", "GitHubトークンは github.com のRepositoryにだけ送れます")
	}
	check(len(t.Files) <= 32, "files", "認証ファイルは32個までです")
	total := 0
	seen := map[string]bool{}
	for _, f := range t.Files {
		check(ValidHomePath(f.Path) && !seen[f.Path], "files."+f.Path, "ホーム配下の正規化した相対パスで、重複なく指定してください")
		seen[f.Path] = true
		check(len(f.Content) <= MaxTaskFile, "files."+f.Path, "ファイルが128KiBを超えています")
		total += len(f.Content)
	}
	check(total <= MaxTaskFiles, "files", "認証ファイルの合計が256KiBを超えています")
	check(len(t.Env) <= 32, "env", "環境変数は32個までです")
	for k, v := range t.Env {
		check(envPattern.MatchString(k) && !reservedEnv(k), "env."+k, "この環境変数名は使用できません")
		check(len(v) <= 16<<10 && utf8.ValidString(v) && !strings.ContainsRune(v, 0), "env."+k, "値が長すぎるか不正です")
	}
	if len(bad) > 0 {
		return Fail("INVALID_TASK", "タスクを修正してください", bad)
	}
	b, _ := json.Marshal(TaskPayload{ID: strings.Repeat("0", 32), Spec: t})
	if len(b) > MaxTaskPayload {
		return Fail("TASK_TOO_LARGE", "タスク全体が512KiBを超えています", nil)
	}
	return nil
}

func (r TaskResult) Validate() error {
	ok := (r.Status == "succeeded" || r.Status == "failed" || r.Status == "no-result") &&
		len(r.Error) <= 4096 && len(r.Output) <= MaxTaskOutput && len(r.DiffStat) <= 16<<10 && len(r.Patch) <= MaxTaskPatch &&
		(r.Branch == "" || ValidGitRef(r.Branch)) && commitPattern.MatchString(r.Commit) && len(r.PullRequestURL) <= 512
	if ok && r.PullRequestURL != "" {
		u, e := url.Parse(r.PullRequestURL)
		ok = e == nil && u.Scheme == "https" && u.Host != "" && u.User == nil
	}
	if !ok {
		return Fail("TASK_RESULT", "タスク結果が不正または上限超過です", nil)
	}
	return nil
}

// TaskTitle derives a short, terminal-safe label from the prompt.
func TaskTitle(prompt string) string {
	line := ""
	for _, l := range strings.Split(prompt, "\n") {
		if strings.TrimSpace(l) != "" {
			line = strings.TrimSpace(l)
			break
		}
	}
	line = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, line)
	if utf8.RuneCountInString(line) > 120 {
		line = string([]rune(line)[:117]) + "..."
	}
	return line
}

func (s *Store) seal(id, purpose string, plain []byte) ([]byte, error) {
	nonce := make([]byte, s.box.NonceSize())
	if _, e := rand.Read(nonce); e != nil {
		return nil, e
	}
	return s.box.Seal(nonce, nonce, plain, []byte(id+":"+purpose)), nil
}
func (s *Store) open(id, purpose string, b []byte) ([]byte, error) {
	n := s.box.NonceSize()
	if len(b) < n {
		return nil, errors.New("missing encrypted task data")
	}
	return s.box.Open(nil, b[:n], b[n:], []byte(id+":"+purpose))
}

// placeTx selects a Node under the caller's writer lock and returns an
// unsaved Instance. Allocation and task placement share this exact ledger.
func (s *Store) placeTx(ctx context.Context, tx *sql.Tx, c Config, runs []Instance, request string, p Pool) (Instance, error) {
	obs, e := readNodes(ctx, tx)
	if e != nil {
		return Instance{}, e
	}
	rows := candidates(c, p, runs, obs, s.Now())
	if len(rows) == 0 || len(rows[0].Reasons) > 0 {
		return Instance{}, Fail("NO_CAPACITY", "現在配置できるNodeがありません", rows)
	}
	im, _ := c.Image(p.Image)
	now := s.Now().UTC()
	return Instance{ID: ID(), RequestID: request, Node: rows[0].Node, Pool: p, Image: im, State: "Reserved", Reservation: rows[0].Reservation, Held: p.Charge(), Created: now, Updated: now, Deadline: now.Add(time.Duration(p.ExecutionMinutes+10) * time.Minute)}, nil
}

func readTaskRow(ctx context.Context, q rowReader, query string, args ...any) (Task, []byte, []byte, []byte, error) {
	var t Task
	var payload, result, progress, secret []byte
	e := q.QueryRowContext(ctx, query, args...).Scan(&payload, &result, &progress, &secret)
	if errors.Is(e, sql.ErrNoRows) {
		return t, nil, nil, nil, Fail("TASK_NOT_FOUND", "タスクがありません", nil)
	}
	if e != nil {
		return t, nil, nil, nil, e
	}
	if e = json.Unmarshal(payload, &t); e != nil {
		return t, nil, nil, nil, e
	}
	return t, result, progress, secret, nil
}
func saveTask(ctx context.Context, tx *sql.Tx, t Task) error {
	t.State, t.Node, t.Progress, t.Result, t.Deadline = "", "", "", nil, nil
	b, _ := json.Marshal(t)
	_, e := tx.ExecContext(ctx, "UPDATE tasks SET payload=? WHERE id=?", b, t.ID)
	return e
}

// dispatchTx places one queued task. NO_CAPACITY leaves it queued.
func (s *Store) dispatchTx(ctx context.Context, tx *sql.Tx, c Config, runs []Instance, t *Task) (*Instance, error) {
	p, ok := c.Pool(t.Pool)
	if !ok || !p.Tasks || !p.Enabled {
		return nil, nil
	}
	inst, e := s.placeTx(ctx, tx, c, runs, "task:"+t.ID, p)
	var fault *Error
	if errors.As(e, &fault) && fault.Code == "NO_CAPACITY" {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	inst.Task = t.ID
	inst.State = "Provisioning"
	inst.JITReady = true
	b, _ := json.Marshal(inst)
	if _, e = tx.ExecContext(ctx, "INSERT INTO instances(id,request,payload) VALUES(?,?,?)", inst.ID, inst.RequestID, b); e != nil {
		return nil, e
	}
	t.Instance = inst.ID
	t.Updated = s.Now().UTC()
	if e = saveTask(ctx, tx, *t); e != nil {
		return nil, e
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO audit(at,event,target) VALUES(?,?,?)", s.Now().Unix(), "task.place", t.ID+"/"+inst.ID)
	return &inst, e
}

// SubmitTask stores an encrypted task and places it immediately when capacity
// exists. The same client and request ID are idempotent for the same content.
func (s *Store) SubmitTask(ctx context.Context, client string, spec TaskSpec) (Task, error) {
	if !ValidName(client) {
		return Task{}, Fail("CLIENT_UNAUTHORIZED", "クライアントが不正です", nil)
	}
	if e := spec.Validate(); e != nil {
		return Task{}, e
	}
	raw, _ := json.Marshal(spec)
	hash := Hash(raw)
	var id string
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		existing, _, _, _, e := readTaskRow(ctx, tx, "SELECT payload,result,progress,secret FROM tasks WHERE client=? AND request=?", client, spec.RequestID)
		if e == nil {
			if existing.SpecHash != hash {
				return Fail("IDEMPOTENCY_CONFLICT", "同じ要求IDで異なるタスクは登録できません", nil)
			}
			id = existing.ID
			return nil
		}
		var fault *Error
		if !errors.As(e, &fault) || fault.Code != "TASK_NOT_FOUND" {
			return e
		}
		c, rev, e := readConfig(ctx, tx)
		if e != nil {
			return e
		}
		if rev == 0 {
			return Fail("NOT_CONFIGURED", "Controllerが未設定です", nil)
		}
		p, ok := c.Pool(spec.Pool)
		if !ok || !p.Tasks {
			return Fail("TASK_POOL", "タスク用Pool（tasks: true）を指定してください", spec.Pool)
		}
		if !p.Enabled {
			return Fail("POOL_DISABLED", "Poolが無効です", spec.Pool)
		}
		var open int
		if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks WHERE client=? AND secret IS NOT NULL", client).Scan(&open); e != nil {
			return e
		}
		if open >= maxClientTasks {
			return Fail("TASK_LIMIT", "未完了タスクが多すぎます", maxClientTasks)
		}
		now := s.Now().UTC()
		t := Task{ID: ID(), Client: client, RequestID: spec.RequestID, Pool: spec.Pool, Agent: spec.Agent.Name, Title: TaskTitle(spec.Prompt), Repository: spec.Repository, BaseRef: spec.BaseRef, Branch: spec.Branch, PullRequest: spec.PullRequest, SpecHash: hash, Created: now, Updated: now}
		payload, _ := json.Marshal(TaskPayload{ID: t.ID, Spec: spec})
		sealed, e := s.seal(t.ID, "payload", payload)
		if e != nil {
			return e
		}
		meta, _ := json.Marshal(t)
		if _, e = tx.ExecContext(ctx, "INSERT INTO tasks(id,client,request,created,payload,secret) VALUES(?,?,?,?,?,?)", t.ID, client, spec.RequestID, now.UnixNano(), meta, sealed); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO audit(at,event,target) VALUES(?,?,?)", now.Unix(), "task.submit", client+"/"+t.ID); e != nil {
			return e
		}
		id = t.ID
		queued, e := queuedTasks(ctx, tx)
		if e != nil {
			return e
		}
		for _, older := range queued {
			if older.Pool == t.Pool && older.ID != t.ID {
				return nil // wait behind older tasks of this Pool
			}
		}
		runs, e := readInstances(ctx, tx)
		if e != nil {
			return e
		}
		_, e = s.dispatchTx(ctx, tx, c, runs, &t)
		return e
	})
	if err != nil {
		return Task{}, err
	}
	return s.Task(ctx, client, id, false)
}

// queuedTasks returns unplaced, live tasks in submission order.
func queuedTasks(ctx context.Context, q queryReader) ([]Task, error) {
	rows, e := q.QueryContext(ctx, "SELECT payload FROM tasks WHERE secret IS NOT NULL ORDER BY created,id")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	queued := []Task{}
	for rows.Next() {
		var b []byte
		var t Task
		if e = rows.Scan(&b); e == nil {
			e = json.Unmarshal(b, &t)
		}
		if e != nil {
			return nil, e
		}
		if t.Instance == "" && !t.Cancelled && !t.Expired {
			queued = append(queued, t)
		}
	}
	return queued, rows.Err()
}

// DispatchTasks places queued tasks first-in first-out per Pool and expires
// ones that waited longer than TaskQueueLifetime. When the oldest task of a
// Pool cannot be placed, younger tasks of that Pool wait behind it.
func (s *Store) DispatchTasks(ctx context.Context) (placed int, err error) {
	// A read-only look first: GitHub-only installations never take the
	// writer lock for tasks.
	pending, err := queuedTasks(ctx, s.DB)
	if err != nil || len(pending) == 0 {
		return 0, err
	}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		c, rev, e := readConfig(ctx, tx)
		if e != nil || rev == 0 {
			return e
		}
		queued, e := queuedTasks(ctx, tx)
		if e != nil {
			return e
		}
		runs, e := readInstances(ctx, tx)
		if e != nil {
			return e
		}
		blocked := map[string]bool{}
		for i := range queued {
			t := &queued[i]
			if s.Now().Sub(t.Created) > TaskQueueLifetime {
				t.Expired = true
				t.Updated = s.Now().UTC()
				if e = saveTask(ctx, tx, *t); e != nil {
					return e
				}
				if _, e = tx.ExecContext(ctx, "UPDATE tasks SET secret=NULL WHERE id=?", t.ID); e != nil {
					return e
				}
				if _, e = tx.ExecContext(ctx, "INSERT INTO audit(at,event,target) VALUES(?,?,?)", s.Now().Unix(), "task.expire", t.ID); e != nil {
					return e
				}
				continue
			}
			if blocked[t.Pool] {
				continue
			}
			inst, e := s.dispatchTx(ctx, tx, c, runs, t)
			if e != nil {
				return e
			}
			if inst == nil {
				blocked[t.Pool] = true
				continue
			}
			runs = append(runs, *inst)
			placed++
		}
		return nil
	})
	return
}

// collectingGrace keeps a Deleted task without a result in Collecting for a
// while: the Node reports the result right after deletion, sometimes a Step
// later. After that, Finished means that no result will arrive.
const collectingGrace = 2 * time.Minute

func deriveTaskState(t Task, inst *Instance, result *TaskResult, now time.Time) string {
	if t.Expired {
		return "Expired"
	}
	finished := inst == nil || inst.State == "Deleting" || inst.State == "Deleted"
	if result != nil && finished {
		switch {
		case result.Status == "succeeded":
			return "Succeeded"
		case t.Cancelled:
			return "Cancelled"
		default:
			return "Failed"
		}
	}
	if inst == nil {
		switch {
		case t.Cancelled:
			return "Cancelled"
		case t.Instance == "":
			return "Queued"
		default:
			return "Finished"
		}
	}
	switch inst.State {
	case "Reserved", "Provisioning", "Idle", "Busy":
		if t.Cancelled || !now.Before(inst.Deadline) {
			return "Stopping"
		}
		if inst.State == "Idle" || inst.State == "Busy" {
			return "Running"
		}
		return "Starting"
	case "Stopping":
		return "Stopping"
	case "Deleted":
		if t.Cancelled {
			return "Cancelled"
		}
		if now.Sub(inst.Updated) > collectingGrace {
			return "Finished"
		}
	}
	return "Collecting"
}

func (s *Store) viewTask(t Task, inst *Instance, result, progress []byte, detail bool) (Task, error) {
	if len(result) > 0 {
		plain, e := s.open(t.ID, "result", result)
		if e != nil {
			return t, e
		}
		var r TaskResult
		if e = json.Unmarshal(plain, &r); e != nil {
			return t, e
		}
		if !detail {
			r.Output, r.Patch, r.DiffStat = "", "", ""
		}
		t.Result = &r
	}
	if detail && len(progress) > 0 {
		plain, e := s.open(t.ID, "progress", progress)
		if e != nil {
			return t, e
		}
		t.Progress = string(plain)
	}
	if inst != nil {
		t.Node = inst.Node
		d := inst.Deadline
		t.Deadline = &d
	}
	t.State = deriveTaskState(t, inst, t.Result, s.Now())
	return t, nil
}

// Task returns one task of this client. client "" is the local administrator.
func (s *Store) Task(ctx context.Context, client, id string, detail bool) (Task, error) {
	if !ValidID(id) {
		return Task{}, Fail("TASK_NOT_FOUND", "タスクがありません", nil)
	}
	t, result, progress, _, e := readTaskRow(ctx, s.DB, "SELECT payload,result,progress,secret FROM tasks WHERE id=?", id)
	if e != nil {
		return Task{}, e
	}
	if client != "" && t.Client != client {
		return Task{}, Fail("TASK_NOT_FOUND", "タスクがありません", nil)
	}
	var inst *Instance
	if t.Instance != "" {
		if inst, e = readInstance(ctx, s.DB, t.Instance); e != nil {
			return Task{}, e
		}
	}
	return s.viewTask(t, inst, result, progress, detail)
}

// Tasks lists the newest tasks of one client (or all for client "").
func (s *Store) Tasks(ctx context.Context, client string, limit int) ([]Task, error) {
	if limit < 1 || limit > 500 {
		limit = 50
	}
	query := "SELECT payload,result FROM tasks ORDER BY created DESC,id LIMIT ?"
	args := []any{limit}
	if client != "" {
		query = "SELECT payload,result FROM tasks WHERE client=? ORDER BY created DESC,id LIMIT ?"
		args = []any{client, limit}
	}
	rows, e := s.DB.QueryContext(ctx, query, args...)
	if e != nil {
		return nil, e
	}
	type row struct {
		t      Task
		result []byte
	}
	list := []row{}
	for rows.Next() {
		var b, r []byte
		var t Task
		if e = rows.Scan(&b, &r); e == nil {
			e = json.Unmarshal(b, &t)
		}
		if e != nil {
			rows.Close()
			return nil, e
		}
		list = append(list, row{t, r})
	}
	rows.Close()
	if e = rows.Err(); e != nil {
		return nil, e
	}
	byID := map[string]*Instance{}
	for _, r := range list {
		if r.t.Instance == "" {
			continue
		}
		inst, e := readInstance(ctx, s.DB, r.t.Instance)
		if e != nil {
			return nil, e
		}
		byID[r.t.Instance] = inst
	}
	out := []Task{}
	for _, r := range list {
		t, e := s.viewTask(r.t, byID[r.t.Instance], r.result, nil, false)
		if e != nil {
			return nil, e
		}
		out = append(out, t)
	}
	return out, nil
}

// CancelTask erases a queued task's payload or asks the Node to stop its VM.
// A running VM's resources are released only by host-confirmed shutdown.
func (s *Store) CancelTask(ctx context.Context, client, id string) error {
	if !ValidID(id) {
		return Fail("TASK_NOT_FOUND", "タスクがありません", nil)
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		t, result, _, _, e := readTaskRow(ctx, tx, "SELECT payload,result,progress,secret FROM tasks WHERE id=?", id)
		if e != nil {
			return e
		}
		if client != "" && t.Client != client {
			return Fail("TASK_NOT_FOUND", "タスクがありません", nil)
		}
		if t.Cancelled || t.Expired || len(result) > 0 {
			return nil
		}
		if t.Instance != "" {
			inst, e := readInstance(ctx, tx, t.Instance)
			if e != nil {
				return e
			}
			// A finished VM keeps its outcome; cancelling it would only
			// relabel a result that already exists.
			if inst == nil || inst.State == "Deleting" || inst.State == "Deleted" {
				return nil
			}
		}
		t.Cancelled = true
		t.Updated = s.Now().UTC()
		if e = saveTask(ctx, tx, t); e != nil {
			return e
		}
		if t.Instance == "" {
			_, e = tx.ExecContext(ctx, "UPDATE tasks SET secret=NULL WHERE id=?", id)
			return e
		}
		runs, e := readInstances(ctx, tx)
		if e != nil {
			return e
		}
		for _, a := range runs {
			if a.ID == t.Instance && a.State != "Deleted" && a.State != "Deleting" {
				a.State = "Stopping"
				a.Updated = s.Now().UTC()
				if e = saveInstance(ctx, tx, a); e != nil {
					return e
				}
			}
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO audit(at,event,target) VALUES(?,?,?)", s.Now().Unix(), "task.cancel", id)
		return e
	})
}

// ReportTask accepts progress while the owning Node's VM runs and one result
// after the host confirmed shutdown. Guest output is data, never proof of
// deletion; it cannot change the Instance lifecycle.
func (s *Store) ReportTask(ctx context.Context, node, id string, r TaskReport) error {
	if !ValidName(node) || !ValidID(id) || !ValidID(r.Instance) || len(r.Progress) > MaxTaskProgress || (r.Progress == "" && r.Result == nil) {
		return Fail("TASK_REPORT", "タスク報告が不正です", nil)
	}
	if r.Result != nil {
		if e := r.Result.Validate(); e != nil {
			return e
		}
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		t, result, _, _, e := readTaskRow(ctx, tx, "SELECT payload,result,progress,secret FROM tasks WHERE id=?", id)
		if e != nil {
			return e
		}
		inst, e := readInstance(ctx, tx, r.Instance)
		if e != nil {
			return e
		}
		if inst == nil || inst.Task != id || t.Instance != inst.ID || inst.Node != node {
			return Fail("TASK_NOT_OWNED", "このNodeのタスクではありません", nil)
		}
		if r.Progress != "" && (inst.State == "Provisioning" || inst.State == "Idle" || inst.State == "Busy" || inst.State == "Stopping") {
			sealed, e := s.seal(id, "progress", []byte(r.Progress))
			if e != nil {
				return e
			}
			now := s.Now().UTC()
			t.ProgressAt = &now
			if e = saveTask(ctx, tx, t); e != nil {
				return e
			}
			if _, e = tx.ExecContext(ctx, "UPDATE tasks SET progress=? WHERE id=?", sealed, id); e != nil {
				return e
			}
		}
		if r.Result == nil {
			return nil
		}
		if inst.State != "Deleting" && inst.State != "Deleted" {
			return Fail("TASK_STILL_RUNNING", "停止確認前の結果は受け付けません", nil)
		}
		if len(result) > 0 {
			return nil
		}
		bound := bindResult(t, *r.Result)
		plain, _ := json.Marshal(bound)
		sealed, e := s.seal(id, "result", plain)
		if e != nil {
			return e
		}
		t.Updated = s.Now().UTC()
		if e = saveTask(ctx, tx, t); e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, "UPDATE tasks SET result=? WHERE id=?", sealed, id)
		return e
	})
}

// taskPayloadTx decrypts a task payload for the ensure command of its Node.
// It returns nil (no delivery) for cancelled or expired tasks and for tasks of
// a revoked client: credentials never leave the Controller after revocation.
func (s *Store) taskPayloadTx(ctx context.Context, tx *sql.Tx, a Instance) (*TaskPayload, error) {
	t, _, _, secret, e := readTaskRow(ctx, tx, "SELECT payload,result,progress,secret FROM tasks WHERE id=?", a.Task)
	if e != nil {
		return nil, e
	}
	if len(secret) == 0 || t.Cancelled || t.Expired {
		return nil, nil
	}
	var revoked bool
	if e = tx.QueryRowContext(ctx, "SELECT revoked FROM clients WHERE name=?", t.Client).Scan(&revoked); errors.Is(e, sql.ErrNoRows) || revoked {
		return nil, nil
	} else if e != nil {
		return nil, e
	}
	plain, e := s.open(a.Task, "payload", secret)
	if e != nil {
		return nil, e
	}
	var p TaskPayload
	if e = json.Unmarshal(plain, &p); e != nil {
		return nil, e
	}
	if p.ID != a.Task {
		return nil, errors.New("task payload identity mismatch")
	}
	return &p, nil
}

// readInstance loads one Instance by ID, or nil when it does not exist.
func readInstance(ctx context.Context, q rowReader, id string) (*Instance, error) {
	var b []byte
	e := q.QueryRowContext(ctx, "SELECT payload FROM instances WHERE id=?", id).Scan(&b)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var a Instance
	if e = json.Unmarshal(b, &a); e != nil {
		return nil, e
	}
	return &a, nil
}

var pullNumber = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)

// bindResult ties guest-reported locations to what the task asked for: the
// branch is the task's branch, and a pull request URL must point into the
// task's own github.com repository. Anything else is dropped.
func bindResult(t Task, r TaskResult) TaskResult {
	r.Branch = t.Branch
	if r.PullRequestURL != "" {
		repo, ok := RepositoryPath(t.Repository)
		prefix := "https://github.com/" + strings.ToLower(repo) + "/pull/"
		if !ok || !strings.HasPrefix(strings.ToLower(r.PullRequestURL), prefix) || !pullNumber.MatchString(r.PullRequestURL[len(prefix):]) {
			r.PullRequestURL = ""
		}
	}
	return r
}

type ClientRecord struct {
	Name    string    `json:"name"`
	Revoked bool      `json:"revoked"`
	Created time.Time `json:"created"`
}

// ApproveClient signs a task-client CSR that the administrator received out of
// band. The private key never leaves the client machine.
func (s *Store) ApproveClient(ctx context.Context, ca CA, name string, csr []byte, replace bool) (cert []byte, cluster string, err error) {
	if !ValidName(name) {
		return nil, "", Fail("CLIENT_INPUT", "クライアント名は英小文字・数字・ハイフンです", nil)
	}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		c, rev, e := readConfig(ctx, tx)
		if e != nil {
			return e
		}
		if rev == 0 {
			return Fail("NOT_CONFIGURED", "先にClusterを設定してください", nil)
		}
		var revoked bool
		e = tx.QueryRowContext(ctx, "SELECT revoked FROM clients WHERE name=?", name).Scan(&revoked)
		exists := e == nil
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if exists && revoked {
			// A new key must not inherit a revoked client's tasks and results.
			return Fail("CLIENT_REVOKED", "失効したクライアント名は再利用できません。別の名前で登録してください", name)
		}
		if exists && !replace {
			return Fail("CLIENT_EXISTS", "同名のクライアントがあります。置き換えるなら --replace を指定してください", name)
		}
		cert, e = ca.SignClient(csr, c.Name, name)
		if e != nil {
			return e
		}
		cluster = c.Name
		if _, e = tx.ExecContext(ctx, "INSERT INTO clients(name,certificate_hash,revoked,created) VALUES(?,?,0,?) ON CONFLICT(name) DO UPDATE SET certificate_hash=excluded.certificate_hash,previous_hash='',previous_until=0", name, Hash(cert), s.Now().Unix()); e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO audit(at,event,target) VALUES(?,?,?)", s.Now().Unix(), "client.approve", name)
		return e
	})
	return
}

// clientRenewalGrace keeps the previous certificate valid after a renewal, so
// other processes of the same client (several MCP servers, a CLI call) keep
// working until they reload the renewed certificate.
const clientRenewalGrace = 7 * 24 * time.Hour

func (s *Store) AuthorizeClient(ctx context.Context, name string, certPEM []byte) error {
	var revoked bool
	var hash, previous string
	var until int64
	e := s.DB.QueryRowContext(ctx, "SELECT revoked,certificate_hash,previous_hash,previous_until FROM clients WHERE name=?", name).Scan(&revoked, &hash, &previous, &until)
	got := Hash(certPEM)
	if e != nil || revoked || (hash != got && (previous == "" || previous != got || s.Now().Unix() >= until)) {
		return Fail("CLIENT_UNAUTHORIZED", "クライアントは未承認または失効済みです", nil)
	}
	return nil
}

func (s *Store) UpdateClientCertificate(ctx context.Context, name string, certPEM []byte) error {
	r, e := s.DB.ExecContext(ctx, "UPDATE clients SET previous_hash=certificate_hash,previous_until=?,certificate_hash=? WHERE name=? AND revoked=0", s.Now().Add(clientRenewalGrace).Unix(), Hash(certPEM), name)
	if e != nil {
		return e
	}
	if n, e := r.RowsAffected(); e != nil || n == 0 {
		return Fail("CLIENT_UNAUTHORIZED", "クライアントは未承認または失効済みです", nil)
	}
	return nil
}

// RevokeClient rejects the client's future requests, erases its queued
// payloads and asks the Nodes to stop its placed VMs. Resources stay held
// until each host confirms shutdown; payload delivery stops at once.
func (s *Store) RevokeClient(ctx context.Context, name string) (cancelled int, err error) {
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		r, e := tx.ExecContext(ctx, "UPDATE clients SET revoked=1 WHERE name=?", name)
		if e != nil {
			return e
		}
		if n, e := r.RowsAffected(); e != nil || n == 0 {
			return Fail("CLIENT_NOT_FOUND", "クライアントがありません", name)
		}
		rows, e := tx.QueryContext(ctx, "SELECT payload FROM tasks WHERE client=? AND secret IS NOT NULL", name)
		if e != nil {
			return e
		}
		live := []Task{}
		for rows.Next() {
			var b []byte
			var t Task
			if e = rows.Scan(&b); e == nil {
				e = json.Unmarshal(b, &t)
			}
			if e != nil {
				rows.Close()
				return e
			}
			if !t.Cancelled && !t.Expired {
				live = append(live, t)
			}
		}
		rows.Close()
		if e = rows.Err(); e != nil {
			return e
		}
		for _, t := range live {
			if t.Instance != "" {
				inst, e := readInstance(ctx, tx, t.Instance)
				if e != nil {
					return e
				}
				if inst == nil || inst.State == "Deleting" || inst.State == "Deleted" {
					continue
				}
				inst.State = "Stopping"
				inst.Updated = s.Now().UTC()
				if e = saveInstance(ctx, tx, *inst); e != nil {
					return e
				}
			} else if _, e = tx.ExecContext(ctx, "UPDATE tasks SET secret=NULL WHERE id=?", t.ID); e != nil {
				return e
			}
			t.Cancelled = true
			t.Updated = s.Now().UTC()
			if e = saveTask(ctx, tx, t); e != nil {
				return e
			}
			cancelled++
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO audit(at,event,target) VALUES(?,?,?)", s.Now().Unix(), "client.revoke", name)
		return e
	})
	return
}

func (s *Store) Clients(ctx context.Context) ([]ClientRecord, error) {
	rows, e := s.DB.QueryContext(ctx, "SELECT name,revoked,created FROM clients ORDER BY name")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []ClientRecord{}
	for rows.Next() {
		var r ClientRecord
		var created int64
		if e = rows.Scan(&r.Name, &r.Revoked, &created); e != nil {
			return nil, e
		}
		r.Created = time.Unix(created, 0).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// TaskPools lists enabled Pools that accept tasks, without Node details.
func (s *Store) TaskPools(ctx context.Context) ([]Pool, error) {
	c, _, e := s.Config(ctx)
	if e != nil {
		return nil, e
	}
	out := []Pool{}
	for _, p := range c.Pools {
		if p.Tasks && p.Enabled {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// TaskPool is the client-visible menu entry for a task Pool.
type TaskPool struct {
	Name             string `json:"name"`
	VCPU             int64  `json:"vcpu"`
	MemoryMiB        int64  `json:"memoryMiB"`
	DiskGiB          int64  `json:"diskGiB"`
	ExecutionMinutes int64  `json:"executionMinutes"`
	MaxRunners       int64  `json:"maxRunners"`
}

func TaskPoolView(p Pool) TaskPool {
	return TaskPool{Name: p.Name, VCPU: p.VCPU, MemoryMiB: p.MemoryMiB, DiskGiB: p.RootGiB + p.ScratchGiB, ExecutionMinutes: p.ExecutionMinutes, MaxRunners: p.MaxRunners}
}
