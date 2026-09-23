package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/MOVEI144/RunnerLoom/internal/core"
	"github.com/MOVEI144/RunnerLoom/internal/tasks"
)

// Backend is implemented by *tasks.Service.
type Backend interface {
	Agents(context.Context) ([]tasks.AgentInfo, error)
	Pools(context.Context) ([]core.TaskPool, error)
	Start(context.Context, tasks.StartRequest) (core.Task, error)
	Get(context.Context, string) (core.Task, error)
	List(context.Context) ([]core.Task, error)
	Cancel(context.Context, string) (core.Task, error)
	Wait(context.Context, string, time.Duration) (core.Task, error)
}

// MaxWait stays below ChatGPT's ~60 second tool-call ceiling.
const MaxWait = 50 * time.Second

const instructions = `RunnerLoom runs long coding tasks in disposable VMs on the user's own machines, like a self-hosted cloud agent. Tasks are asynchronous: runnerloom_start_task returns a task ID immediately; check later with runnerloom_get_task (optionally waitSeconds up to 50). Results come back as a pushed branch / draft PR, or as a patch when no push was possible. Starting a task sends the chosen agent's local credentials to the VM; only agents the user allowed on this PC can be used.`

func decodeArgs(raw json.RawMessage, v any) error {
	if e := core.Decode(bytes.NewReader(raw), v); e != nil {
		return core.Fail("INVALID_ARGUMENTS", "ツール引数の項目名・型を確認してください", nil)
	}
	return nil
}

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}
func str(desc string) map[string]any  { return map[string]any{"type": "string", "description": desc} }
func boolean(d string) map[string]any { return map[string]any{"type": "boolean", "description": d} }

// taskView trims bulky fields unless the caller asked for them.
func taskView(t core.Task, includePatch bool) core.Task {
	if t.Result != nil {
		r := *t.Result
		if !includePatch && r.Patch != "" {
			r.Patch = "(" + itoa(len(r.Patch)) + " bytes; call runnerloom_get_task with includePatch=true)"
		}
		t.Result = &r
	}
	return t
}
func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// NewRunnerLoom returns the MCP server for agent tasks.
func NewRunnerLoom(b Backend, version string) *Server {
	idProp := str("Task ID (32 hex characters) returned by runnerloom_start_task")
	return &Server{Name: "runnerloom", Version: version, Instructions: instructions, Tools: []Tool{
		{Name: "runnerloom_list_agents", Title: "List agents", ReadOnly: true,
			Description: "List coding agents that can run in RunnerLoom VMs (claude, codex, opencode, pi, gemini and user-defined ones), whether the user allowed each one on this PC, and which local credential files/env vars were found (names only).",
			InputSchema: obj(map[string]any{}),
			Handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
				v, e := b.Agents(ctx)
				return map[string]any{"agents": v}, e
			}},
		{Name: "runnerloom_list_pools", Title: "List task pools", ReadOnly: true,
			Description: "List VM sizes (task pools) the Controller offers: vCPU, memory, disk and the time limit per task.",
			InputSchema: obj(map[string]any{}),
			Handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
				v, e := b.Pools(ctx)
				return map[string]any{"pools": v}, e
			}},
		{Name: "runnerloom_start_task", Title: "Start a VM task",
			Description: "Start a long-running coding task in a fresh VM and return immediately with its ID. With a repository, the VM clones it, works on `branch` (new from baseRef, or continues it when it already exists), commits and pushes, and can open a draft PR. Without a repository the agent works in an empty directory and the result is returned as a patch. Use continueFrom to continue a previous task's branch.",
			InputSchema: obj(map[string]any{
				"prompt":       str("Complete, self-contained instructions for the agent. It cannot see this conversation."),
				"agent":        str("Agent name from runnerloom_list_agents (for example codex, claude, opencode, pi, gemini). Defaults to the user's default agent."),
				"repository":   str("GitHub repository as owner/name or an https URL. Optional."),
				"baseRef":      str("Branch to start from when creating a new work branch. Defaults to the repository's default branch."),
				"branch":       str("Work branch to push. Defaults to runnerloom/<timestamp>. Never the base branch."),
				"pullRequest":  boolean("Open a draft pull request after pushing (github.com only)."),
				"pool":         str("Task pool name from runnerloom_list_pools. Defaults to the user's default or the only pool."),
				"continueFrom": str("Task ID whose repository/branch/agent to continue."),
				"requestId":    str("Optional idempotency key; resending the same key and arguments returns the same task."),
			}, "prompt"),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var r tasks.StartRequest
				if e := decodeArgs(raw, &r); e != nil {
					return nil, e
				}
				t, e := b.Start(ctx, r)
				if e != nil {
					return nil, e
				}
				return map[string]any{"task": taskView(t, false), "next": "The task runs asynchronously. Call runnerloom_get_task with this id later (waitSeconds up to 50)."}, nil
			}},
		{Name: "runnerloom_get_task", Title: "Get task status", ReadOnly: true,
			Description: "Get a task's state (Queued, Starting, Running, Stopping, Collecting, Succeeded, Failed, Cancelled, Expired, Finished), recent progress output and, when finished, its result: pushed branch/commit, draft PR URL, diff stat, the agent's final output, and optionally the patch.",
			InputSchema: obj(map[string]any{
				"taskId":       idProp,
				"waitSeconds":  map[string]any{"type": "integer", "minimum": 0, "maximum": 50, "description": "Wait up to this many seconds for the task to finish before answering."},
				"includePatch": boolean("Include the full patch when the result was not pushed."),
			}, "taskId"),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var r struct {
					TaskID       string `json:"taskId"`
					WaitSeconds  int    `json:"waitSeconds"`
					IncludePatch bool   `json:"includePatch"`
				}
				if e := decodeArgs(raw, &r); e != nil {
					return nil, e
				}
				wait := min(max(time.Duration(r.WaitSeconds)*time.Second, 0), MaxWait)
				var t core.Task
				var e error
				if wait > 0 {
					t, e = b.Wait(ctx, r.TaskID, wait)
				} else {
					t, e = b.Get(ctx, r.TaskID)
				}
				if e != nil {
					return nil, e
				}
				return map[string]any{"task": taskView(t, r.IncludePatch), "finished": t.Terminal()}, nil
			}},
		{Name: "runnerloom_list_tasks", Title: "List tasks", ReadOnly: true,
			Description: "List this PC's recent tasks (newest first) with their states.",
			InputSchema: obj(map[string]any{}),
			Handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
				v, e := b.List(ctx)
				return map[string]any{"tasks": v}, e
			}},
		{Name: "runnerloom_cancel_task", Title: "Cancel a task", Destructive: true,
			Description: "Cancel a task. A queued task is dropped; a running VM is stopped (its work that was not pushed is lost).",
			InputSchema: obj(map[string]any{"taskId": idProp}, "taskId"),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var r struct {
					TaskID string `json:"taskId"`
				}
				if e := decodeArgs(raw, &r); e != nil {
					return nil, e
				}
				t, e := b.Cancel(ctx, r.TaskID)
				return map[string]any{"task": taskView(t, false)}, e
			}},
	}}
}

// LazyBackend opens the client on first use so that `mcp serve` can start and
// explain setup problems through tool results instead of failing to launch.
// A successful open is cached; a failed one is retried on the next call.
type LazyBackend struct {
	Open   func() (Backend, error)
	mu     sync.Mutex
	opened Backend
}

func (l *LazyBackend) get() (Backend, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.opened != nil {
		return l.opened, nil
	}
	b, e := l.Open()
	if e != nil {
		return nil, e
	}
	l.opened = b
	return b, nil
}
func (l *LazyBackend) Agents(ctx context.Context) ([]tasks.AgentInfo, error) {
	b, e := l.get()
	if e != nil {
		return nil, e
	}
	return b.Agents(ctx)
}
func (l *LazyBackend) Pools(ctx context.Context) ([]core.TaskPool, error) {
	b, e := l.get()
	if e != nil {
		return nil, e
	}
	return b.Pools(ctx)
}
func (l *LazyBackend) Start(ctx context.Context, r tasks.StartRequest) (core.Task, error) {
	b, e := l.get()
	if e != nil {
		return core.Task{}, e
	}
	return b.Start(ctx, r)
}
func (l *LazyBackend) Get(ctx context.Context, id string) (core.Task, error) {
	b, e := l.get()
	if e != nil {
		return core.Task{}, e
	}
	return b.Get(ctx, id)
}
func (l *LazyBackend) List(ctx context.Context) ([]core.Task, error) {
	b, e := l.get()
	if e != nil {
		return nil, e
	}
	return b.List(ctx)
}
func (l *LazyBackend) Cancel(ctx context.Context, id string) (core.Task, error) {
	b, e := l.get()
	if e != nil {
		return core.Task{}, e
	}
	return b.Cancel(ctx, id)
}
func (l *LazyBackend) Wait(ctx context.Context, id string, d time.Duration) (core.Task, error) {
	b, e := l.get()
	if e != nil {
		return core.Task{}, e
	}
	return b.Wait(ctx, id, d)
}
