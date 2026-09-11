package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MOVEI144/RunnerLoom/internal/core"
)

func TestControllerCachePruneIsDryRunFirst(t *testing.T) {
	state := filepath.Join(t.TempDir(), "controller")
	code, out, errout := invoke("setup", "--file", configFixture(t), "--apply", "--role", "controller", "--non-interactive", "--state", state, "--json")
	if code != 0 {
		t.Fatal(code, out, errout)
	}
	cache := filepath.Join(state, "images")
	if err := os.Mkdir(cache, 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte("obsolete-controller-image")
	digest := core.Hash(data)
	path := filepath.Join(cache, digest+".qcow2")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	code, out, errout = invoke("cache", "prune", "--state", state, "--older-than", "168h", "--json")
	if code != 0 || !strings.Contains(out, `"applied": false`) || !strings.Contains(out, `"reclaimable": true`) {
		t.Fatal(code, out, errout)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("dry run changed cache", err)
	}

	code, out, errout = invoke("cache", "prune", "--state", state, "--older-than", "168h", "--apply", "--json")
	if code != 0 || !strings.Contains(out, `"applied": true`) {
		t.Fatal(code, out, errout)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("applied prune left obsolete image", err)
	}
}
