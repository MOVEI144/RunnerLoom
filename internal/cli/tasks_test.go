package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientInitAllowAndMCPStdio(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "client")
	code, out, errout := invoke("client", "init", "--name", "laptop", "--client-dir", dir, "--json")
	if code != 0 || !strings.Contains(out, "csrHash") || strings.Contains(out, "PRIVATE KEY") {
		t.Fatal(code, out, errout)
	}
	code, out, errout = invoke("client", "allow", "codex", "github", "--client-dir", dir, "--json")
	if code != 0 || !strings.Contains(out, `"codex"`) || !strings.Contains(out, `"github"`) {
		t.Fatal(code, out, errout)
	}
	// Before approval, the server still starts and explains setup through a
	// tool error; stdout carries protocol messages only.
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"runnerloom_list_tasks","arguments":{}}}`,
	}, "\n") + "\n"
	var stdout, stderr bytes.Buffer
	if rc := Run(context.Background(), []string{"mcp", "serve", "--client-dir", dir}, strings.NewReader(in), &stdout, &stderr); rc != 0 {
		t.Fatal(rc, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("unexpected stdout: %q", stdout.String())
	}
	for _, line := range lines {
		var v map[string]any
		if e := json.Unmarshal([]byte(line), &v); e != nil || v["jsonrpc"] != "2.0" {
			t.Fatalf("non-protocol stdout: %q", line)
		}
	}
	if !strings.Contains(stdout.String(), "CLIENT_NOT_APPROVED") || !strings.Contains(stdout.String(), `\"isError\"`) && !strings.Contains(stdout.String(), `"isError":true`) {
		t.Fatalf("setup problem not reported as a tool error: %s", stdout.String())
	}
}

func TestHelpIncludesAgentTasks(t *testing.T) {
	code, out, _ := invoke("--help", "--json")
	if code != 0 {
		t.Fatal(code)
	}
	for _, want := range []string{"client init", "client approve", "client install", "client allow", "client revoke", "task start", "task show", "mcp serve"} {
		if !strings.Contains(out, want) {
			t.Fatal("help does not expose", want)
		}
	}
}
