// Package mcp is a small Model Context Protocol server (JSON-RPC 2.0) with a
// stdio transport and a stateless Streamable HTTP transport. It implements the
// subset needed for tools: initialize, ping, tools/list and tools/call.
package mcp

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/MOVEI144/RunnerLoom/internal/core"
)

// Versions are the protocol revisions this server speaks, newest first.
var Versions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type Tool struct {
	Name        string
	Title       string
	Description string
	InputSchema map[string]any
	ReadOnly    bool
	Destructive bool
	Handler     func(context.Context, json.RawMessage) (any, error)
}

type Server struct {
	Name         string
	Version      string
	Instructions string
	Tools        []Tool
	Log          *slog.Logger
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (s *Server) tool(name string) (Tool, bool) {
	for _, t := range s.Tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

func negotiate(requested string) string {
	for _, v := range Versions {
		if v == requested {
			return v
		}
	}
	return Versions[0]
}

func (s *Server) listTools() map[string]any {
	tools := []map[string]any{}
	for _, t := range s.Tools {
		tools = append(tools, map[string]any{
			"name": t.Name, "title": t.Title, "description": t.Description, "inputSchema": t.InputSchema,
			"annotations": map[string]any{"title": t.Title, "readOnlyHint": t.ReadOnly, "destructiveHint": t.Destructive, "idempotentHint": t.ReadOnly, "openWorldHint": false},
		})
	}
	return map[string]any{"tools": tools}
}

func toolText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

// callTool reports tool failures inside the result (isError) so the model can
// read and react to them, as the MCP specification recommends.
func (s *Server) callTool(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if e := json.Unmarshal(params, &p); e != nil {
		return nil, &rpcError{-32602, "invalid tools/call parameters"}
	}
	t, ok := s.tool(p.Name)
	if !ok {
		return nil, &rpcError{-32602, "unknown tool: " + p.Name}
	}
	if len(p.Arguments) == 0 || string(p.Arguments) == "null" {
		p.Arguments = json.RawMessage("{}")
	}
	v, e := t.Handler(ctx, p.Arguments)
	if e != nil {
		var fault *core.Error
		text := e.Error()
		if errors.As(e, &fault) {
			b, _ := json.MarshalIndent(map[string]any{"error": fault}, "", "  ")
			text = string(b)
		}
		return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": true}, nil
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": toolText(v)}}, "isError": false}, nil
}

// Handle processes one JSON-RPC message and returns the encoded response, or
// nil for notifications and client responses.
func (s *Server) Handle(ctx context.Context, raw []byte) []byte {
	var q request
	if e := json.Unmarshal(raw, &q); e != nil || q.JSONRPC != "2.0" {
		b, _ := json.Marshal(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
		return b
	}
	if len(q.ID) == 0 || string(q.ID) == "null" {
		return nil // notification (initialized, cancelled, ...) or a response
	}
	out := response{JSONRPC: "2.0", ID: q.ID}
	switch q.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(q.Params, &p)
		out.Result = map[string]any{"protocolVersion": negotiate(p.ProtocolVersion), "capabilities": map[string]any{"tools": map[string]any{"listChanged": false}}, "serverInfo": map[string]any{"name": s.Name, "version": s.Version}, "instructions": s.Instructions}
	case "ping":
		out.Result = map[string]any{}
	case "tools/list":
		out.Result = s.listTools()
	case "tools/call":
		out.Result, out.Error = s.callTool(ctx, q.Params)
	default:
		out.Error = &rpcError{-32601, "method not found: " + q.Method}
	}
	b, _ := json.Marshal(out)
	return b
}

// ServeStdio reads newline-delimited messages. Requests run concurrently so a
// waiting tool call never blocks ping or cancellation.
func (s *Server) ServeStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var write sync.Mutex
	var wg sync.WaitGroup
	var calls sync.Map
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var peek struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				RequestID json.RawMessage `json:"requestId"`
			} `json:"params"`
		}
		_ = json.Unmarshal(line, &peek)
		if peek.Method == "notifications/cancelled" {
			if c, ok := calls.Load(string(peek.Params.RequestID)); ok {
				c.(context.CancelFunc)()
			}
			continue
		}
		callCtx, callCancel := context.WithCancel(ctx)
		key := string(peek.ID)
		if key != "" {
			calls.Store(key, callCancel)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer callCancel()
			defer calls.Delete(key)
			if reply := s.Handle(callCtx, line); reply != nil && callCtx.Err() == nil {
				write.Lock()
				_, _ = out.Write(append(reply, '\n'))
				write.Unlock()
			}
		}()
	}
	// Finish in-flight calls (bounded by MaxWait) before returning on EOF.
	wg.Wait()
	return scanner.Err()
}

// HTTPHandler serves stateless Streamable HTTP on /mcp. When secret is set,
// a request must present it either as a bearer token or as the path suffix
// /mcp/<secret> (for connectors that cannot send headers). Browser origins
// other than the allowed list are refused to prevent DNS rebinding.
func (s *Server) HTTPHandler(secret string, allowedOrigins []string) http.Handler {
	authorized := func(r *http.Request) bool {
		if secret == "" {
			return true
		}
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") && subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(h, "Bearer ")), []byte(secret)) == 1 {
			return true
		}
		return subtle.ConstantTimeCompare([]byte(r.URL.Path), []byte("/mcp/"+secret)) == 1
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path != "/mcp" && !strings.HasPrefix(r.URL.Path, "/mcp/") {
			http.NotFound(w, r)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			if !core.Contains(allowedOrigins, origin) {
				http.Error(w, "origin not allowed", http.StatusForbidden)
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "content-type, authorization, mcp-protocol-version, mcp-session-id")
			w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !authorized(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST, OPTIONS")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !core.Contains(Versions, v) {
			http.Error(w, "unsupported MCP-Protocol-Version", http.StatusBadRequest)
			return
		}
		body, e := io.ReadAll(io.LimitReader(r.Body, 4<<20+1))
		if e != nil || len(body) > 4<<20 {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		reply := s.Handle(r.Context(), body)
		if reply == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(reply)
	})
}
