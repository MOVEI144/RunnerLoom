package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MOVEI144/RunnerLoom/internal/core"
	"github.com/MOVEI144/RunnerLoom/internal/tasks"
)

type fakeBackend struct {
	started []tasks.StartRequest
	waited  time.Duration
}

func (f *fakeBackend) Agents(context.Context) ([]tasks.AgentInfo, error) {
	return []tasks.AgentInfo{{Name: "codex", Allowed: true}}, nil
}
func (f *fakeBackend) Pools(context.Context) ([]core.TaskPool, error) {
	return []core.TaskPool{{Name: "agent-tasks"}}, nil
}
func (f *fakeBackend) Start(_ context.Context, r tasks.StartRequest) (core.Task, error) {
	if r.Agent == "denied" {
		return core.Task{}, core.Fail("AGENT_NOT_ALLOWED", "not allowed", r.Agent)
	}
	f.started = append(f.started, r)
	return core.Task{ID: strings.Repeat("a", 32), State: "Queued"}, nil
}
func (f *fakeBackend) Get(context.Context, string) (core.Task, error) {
	return core.Task{ID: strings.Repeat("a", 32), State: "Succeeded", Result: &core.TaskResult{Status: "succeeded", Patch: "diff --git a b"}}, nil
}
func (f *fakeBackend) List(context.Context) ([]core.Task, error) { return []core.Task{}, nil }
func (f *fakeBackend) Cancel(context.Context, string) (core.Task, error) {
	return core.Task{State: "Cancelled"}, nil
}
func (f *fakeBackend) Wait(ctx context.Context, id string, d time.Duration) (core.Task, error) {
	f.waited = d
	return f.Get(ctx, id)
}

func rpc(t *testing.T, s *Server, id int, method string, params any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	var out map[string]any
	if e := json.Unmarshal(s.Handle(context.Background(), b), &out); e != nil {
		t.Fatal(e)
	}
	return out
}

func toolResult(t *testing.T, reply map[string]any) (string, bool) {
	t.Helper()
	r, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", reply)
	}
	content := r["content"].([]any)[0].(map[string]any)
	return content["text"].(string), r["isError"].(bool)
}

func TestInitializeNegotiatesAndListsAnnotatedTools(t *testing.T) {
	s := NewRunnerLoom(&fakeBackend{}, "test")
	init := rpc(t, s, 1, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t", "version": "1"}})
	result := init["result"].(map[string]any)
	if result["protocolVersion"] != "2025-06-18" || result["instructions"] == "" {
		t.Fatalf("%v", result)
	}
	if v := rpc(t, s, 2, "initialize", map[string]any{"protocolVersion": "1999-01-01"})["result"].(map[string]any)["protocolVersion"]; v != Versions[0] {
		t.Fatalf("unknown version must fall back to latest, got %v", v)
	}
	if len(s.Instructions) > 512 && !strings.Contains(s.Instructions[:512], "runnerloom_get_task") {
		t.Fatal("the first 512 characters of instructions must be self-contained")
	}
	tools := rpc(t, s, 3, "tools/list", nil)["result"].(map[string]any)["tools"].([]any)
	readOnly := map[string]bool{}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		readOnly[tool["name"].(string)] = tool["annotations"].(map[string]any)["readOnlyHint"].(bool)
	}
	if len(readOnly) != 6 || !readOnly["runnerloom_get_task"] || readOnly["runnerloom_start_task"] || readOnly["runnerloom_cancel_task"] {
		t.Fatalf("tool annotations: %v", readOnly)
	}
	if rpc(t, s, 4, "no/such", nil)["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Fatal("unknown method accepted")
	}
	if s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)) != nil {
		t.Fatal("notification answered")
	}
}

func TestToolCallsAndErrors(t *testing.T) {
	f := &fakeBackend{}
	s := NewRunnerLoom(f, "test")
	text, isErr := toolResult(t, rpc(t, s, 1, "tools/call", map[string]any{"name": "runnerloom_start_task", "arguments": map[string]any{"prompt": "do it", "agent": "codex", "repository": "o/r", "pullRequest": true}}))
	if isErr || len(f.started) != 1 || f.started[0].Repository != "o/r" || !f.started[0].PullRequest || !strings.Contains(text, "runnerloom_get_task") {
		t.Fatalf("%s %v %+v", text, isErr, f.started)
	}
	text, isErr = toolResult(t, rpc(t, s, 2, "tools/call", map[string]any{"name": "runnerloom_start_task", "arguments": map[string]any{"prompt": "x", "agent": "denied"}}))
	if !isErr || !strings.Contains(text, "AGENT_NOT_ALLOWED") {
		t.Fatalf("backend failure must be a tool error: %s", text)
	}
	text, isErr = toolResult(t, rpc(t, s, 3, "tools/call", map[string]any{"name": "runnerloom_start_task", "arguments": map[string]any{"prompt": "x", "Prompt": "y"}}))
	if !isErr || !strings.Contains(text, "INVALID_ARGUMENTS") {
		t.Fatalf("ambiguous arguments accepted: %s", text)
	}
	text, _ = toolResult(t, rpc(t, s, 4, "tools/call", map[string]any{"name": "runnerloom_get_task", "arguments": map[string]any{"taskId": strings.Repeat("a", 32), "waitSeconds": 600}}))
	if f.waited != MaxWait || strings.Contains(text, "diff --git") {
		t.Fatalf("wait not capped (%s) or patch not trimmed: %s", f.waited, text)
	}
	text, _ = toolResult(t, rpc(t, s, 5, "tools/call", map[string]any{"name": "runnerloom_get_task", "arguments": map[string]any{"taskId": strings.Repeat("a", 32), "includePatch": true}}))
	if !strings.Contains(text, "diff --git") {
		t.Fatal("patch not returned on request")
	}
	if rpc(t, s, 6, "tools/call", map[string]any{"name": "nope"})["error"] == nil {
		t.Fatal("unknown tool accepted")
	}
}

func TestStdioServesConcurrentRequests(t *testing.T) {
	s := NewRunnerLoom(&fakeBackend{}, "test")
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`not json`,
		`{"jsonrpc":"2.0","id":"two","method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if e := s.ServeStdio(context.Background(), strings.NewReader(in), &out); e != nil {
		t.Fatal(e)
	}
	ids := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var v map[string]any
		if e := json.Unmarshal([]byte(line), &v); e != nil {
			t.Fatalf("stdout carried a non-JSON line: %q", line)
		}
		b, _ := json.Marshal(v["id"])
		ids[string(b)] = true
	}
	for _, want := range []string{`1`, `"two"`, `3`, `null`} {
		if !ids[want] {
			t.Fatalf("missing reply %s in %s", want, out.String())
		}
	}
}

func TestHTTPTransportAuthenticationAndShape(t *testing.T) {
	secret := strings.Repeat("s", 40)
	h := NewRunnerLoom(&fakeBackend{}, "test").HTTPHandler(secret, []string{"https://chatgpt.com"})
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	do := func(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := do("POST", "/mcp", body, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request: %d", w.Code)
	}
	if w := do("POST", "/mcp", body, map[string]string{"Authorization": "Bearer wrong"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", w.Code)
	}
	if w := do("POST", "/mcp", body, map[string]string{"Authorization": "Bearer " + secret}); w.Code != 200 || w.Header().Get("Content-Type") != "application/json" || !strings.Contains(w.Body.String(), "runnerloom_start_task") {
		t.Fatalf("bearer request: %d %s", w.Code, w.Body.String())
	}
	if w := do("POST", "/mcp/"+secret, body, nil); w.Code != 200 {
		t.Fatalf("path secret request: %d", w.Code)
	}
	if w := do("POST", "/mcp/"+secret, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil); w.Code != http.StatusAccepted || w.Body.Len() != 0 {
		t.Fatalf("notification: %d", w.Code)
	}
	if w := do("GET", "/mcp/"+secret, "", nil); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", w.Code)
	}
	if w := do("POST", "/mcp/"+secret, body, map[string]string{"Origin": "https://evil.example"}); w.Code != http.StatusForbidden {
		t.Fatalf("foreign origin: %d", w.Code)
	}
	if w := do("POST", "/mcp/"+secret, body, map[string]string{"Origin": "https://chatgpt.com"}); w.Code != 200 || w.Header().Get("Access-Control-Allow-Origin") != "https://chatgpt.com" {
		t.Fatalf("allowed origin: %d", w.Code)
	}
	if w := do("POST", "/mcp/"+secret, body, map[string]string{"MCP-Protocol-Version": "1999-01-01"}); w.Code != http.StatusBadRequest {
		t.Fatalf("bad protocol version: %d", w.Code)
	}
	if w := do("GET", "/.well-known/oauth-protected-resource", "", nil); w.Code != http.StatusNotFound {
		t.Fatalf("discovery probe must be a clean 404: %d", w.Code)
	}
}

func TestJSONRPCEdgeCases(t *testing.T) {
	s := NewRunnerLoom(&fakeBackend{}, "test")
	var v map[string]any
	_ = json.Unmarshal(s.Handle(context.Background(), []byte(`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`)), &v)
	if v["error"].(map[string]any)["code"].(float64) != -32600 {
		t.Fatal("batch not rejected as an invalid request")
	}
	_ = json.Unmarshal(s.Handle(context.Background(), []byte(`{"id":7,"method":"ping"}`)), &v)
	if v["error"].(map[string]any)["code"].(float64) != -32600 || v["id"].(float64) != 7 {
		t.Fatalf("missing jsonrpc version: %v", v)
	}
	if s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":3,"result":{}}`)) != nil {
		t.Fatal("client response answered")
	}
}
