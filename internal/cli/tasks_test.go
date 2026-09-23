package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MOVEI144/RunnerLoom/internal/core"
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

func TestClientApproveAndInstallVerifyFingerprintsBothWays(t *testing.T) {
	state := filepath.Join(t.TempDir(), "controller")
	s, e := core.OpenStore(state)
	if e != nil {
		t.Fatal(e)
	}
	plan, e := s.Plan(context.Background(), core.Example())
	if e == nil {
		_, e = s.Apply(context.Background(), plan.ID)
	}
	s.Close()
	if e != nil {
		t.Fatal(e)
	}
	if _, e = core.InitCA(state, "home"); e != nil {
		t.Fatal(e)
	}
	dir := filepath.Join(t.TempDir(), "client")
	code, out, errout := invoke("client", "init", "--name", "laptop", "--client-dir", dir, "--json")
	if code != 0 {
		t.Fatal(out, errout)
	}
	var initOut struct {
		Data struct{ CSR, CSRHash string } `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &initOut)
	bundle := filepath.Join(t.TempDir(), "laptop-bundle.json")
	args := []string{"client", "approve", "--state", state, "--csr", initOut.Data.CSR, "--name", "laptop", "--url", "https://127.0.0.1:9", "--out", bundle, "--json"}
	if code, out, _ = invoke(append(args, "--csr-hash", strings.Repeat("0", 64), "--skip-url-check")...); code == 0 || !strings.Contains(out, "CSR_MISMATCH") {
		t.Fatalf("CSR signed without matching the PC's hash: %s", out)
	}
	if code, out, _ = invoke(append(args, "--csr-hash", initOut.Data.CSRHash)...); code == 0 || !strings.Contains(out, "CONTROLLER_URL") {
		t.Fatalf("unreachable Controller URL accepted: %s", out)
	}
	code, out, errout = invoke(append(args, "--csr-hash", initOut.Data.CSRHash, "--skip-url-check")...)
	if code != 0 {
		t.Fatal(out, errout)
	}
	var approved struct {
		Data struct{ CAFingerprint string } `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &approved)
	if code, out, _ = invoke("client", "install", "--client-dir", dir, "--bundle", bundle, "--ca-fingerprint", strings.Repeat("1", 64), "--json"); code == 0 || !strings.Contains(out, "BUNDLE_MISMATCH") {
		t.Fatalf("bundle installed without the out-of-band fingerprint: %s", out)
	}
	if code, out, errout = invoke("client", "install", "--client-dir", dir, "--bundle", bundle, "--ca-fingerprint", approved.Data.CAFingerprint, "--json"); code != 0 {
		t.Fatal(out, errout)
	}
}

// Claude Code updates an installed plugin only when its version changes, so
// the plugin must carry the release version it is shipped with.
func TestPluginVersionMatchesRelease(t *testing.T) {
	version, e := os.ReadFile(filepath.Join("..", "..", "VERSION"))
	if e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(filepath.Join("..", "..", "plugins", "runnerloom", ".claude-plugin", "plugin.json"))
	if e != nil {
		t.Fatal(e)
	}
	var plugin struct {
		Version string `json:"version"`
	}
	if e = json.Unmarshal(b, &plugin); e != nil || plugin.Version != strings.TrimSpace(string(version)) {
		t.Fatalf("plugin version %q differs from VERSION %q", plugin.Version, strings.TrimSpace(string(version)))
	}
}
