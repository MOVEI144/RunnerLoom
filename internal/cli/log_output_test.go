package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MOVEI144/RunnerLoom/internal/agent"
	"github.com/MOVEI144/RunnerLoom/internal/core"
)

func TestUntrustedLogsCannotControlTerminal(t *testing.T) {
	raw := "日本語のログ\n\x1b]52;c;c2VjcmV0\a\r\b\u009b31m\u202espoof\tend"
	text := terminalSafeLog([]byte(raw))
	for _, control := range []string{"\x1b", "\a", "\r", "\b", "\u009b", "\u202e"} {
		if strings.Contains(text, control) {
			t.Fatalf("unsafe terminal control %q escaped filter", control)
		}
	}
	if !strings.Contains(text, "日本語のログ\n") || !strings.Contains(text, `\x1b]52;`) || !strings.Contains(text, "\tend") {
		t.Fatal("evidence or readable text was discarded", text)
	}
}

func TestVMLogsEscapeTerminalButKeepJSONData(t *testing.T) {
	dir := t.TempDir()
	conf := agent.Config{Node: "node-a", Cluster: "home", Controller: "https://127.0.0.1:8443", StateDir: filepath.Join(dir, "state"), DiskDir: filepath.Join(dir, "disks"), Ceiling: core.Resources{CPU: 4, Memory: 8192, Disk: 80}, CacheGiB: 1}
	path := filepath.Join(conf.StateDir, "agent.json")
	if e := agent.SaveConfig(path, conf); e != nil {
		t.Fatal(e)
	}
	id := core.ID()
	logs := filepath.Join(conf.StateDir, "logs")
	if e := os.Mkdir(logs, 0700); e != nil {
		t.Fatal(e)
	}
	raw := "plain\n\x1b[2J\u202etext\r"
	if e := os.WriteFile(filepath.Join(logs, id+".log"), []byte(raw), 0600); e != nil {
		t.Fatal(e)
	}
	status, out, errout := invoke("vm", "logs", id, "--config", path)
	if status != 0 || out != terminalSafeLog([]byte(raw)) {
		t.Fatal(status, out, errout)
	}
	status, out, errout = invoke("vm", "logs", id, "--config", path, "--json")
	var decoded struct {
		Data struct {
			Log string `json:"log"`
		} `json:"data"`
	}
	if e := json.Unmarshal([]byte(out), &decoded); e != nil || status != 0 || decoded.Data.Log != raw || strings.Contains(out, "\x1b") {
		t.Fatal(status, out, errout, e)
	}
}
