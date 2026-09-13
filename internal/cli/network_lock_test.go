package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MOVEI144/RunnerLoom/internal/agent"
	"github.com/MOVEI144/RunnerLoom/internal/core"
)

func TestNetworkApplyRefusesWhileAgentLockHeld(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	disks := filepath.Join(dir, "disks")
	if e := os.MkdirAll(disks, 0700); e != nil {
		t.Fatal(e)
	}
	cfg := agent.Config{
		Node:        "node-a",
		Cluster:     "home",
		Controller:  "https://127.0.0.1:8443",
		StateDir:    state,
		DiskDir:     disks,
		NetworkCIDR: "172.30.240.0/24",
		Ceiling:     core.Resources{CPU: 4, Memory: 8192, Disk: 80},
		CacheGiB:    1,
	}
	path := filepath.Join(state, "agent.json")
	if e := agent.SaveConfig(path, cfg); e != nil {
		t.Fatal(e)
	}
	lock, e := core.AcquireLock(state, "agent")
	if e != nil {
		t.Fatal(e)
	}
	defer lock.Close()
	code, out, err := invoke("network", "apply", "--config", path, "--json")
	if code != 5 || !strings.Contains(out+err, "ALREADY_RUNNING") {
		t.Fatalf("apply ignored agent lock: code=%d out=%s err=%s", code, out, err)
	}
}
