package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MOVEI144/RunnerLoom/internal/core"
)

func TestMaintenanceCompactIsDryRunByDefault(t *testing.T) {
	state := filepath.Join(t.TempDir(), "controller")
	code, out, errout := invoke("setup", "--file", configFixture(t), "--apply", "--role", "controller", "--non-interactive", "--state", state, "--json")
	if code != 0 {
		t.Fatal(code, out, errout)
	}
	store, err := core.OpenStore(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB.Exec("INSERT INTO plans(id,base,expires,payload) VALUES('expired-fixture',0,?,X'7B7D')", time.Now().Add(-72*time.Hour).Unix()); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}

	code, out, errout = invoke("maintenance", "compact", "--older-than", "48h", "--keep-audit", "1", "--state", state, "--json")
	if code != 0 || !strings.Contains(out, `"applied": false`) || !strings.Contains(out, `"expiredPlans": 1`) {
		t.Fatal(code, out, errout)
	}
	store, err = core.OpenStore(state)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err = store.DB.QueryRow("SELECT COUNT(*) FROM plans WHERE id='expired-fixture'").Scan(&count); err != nil {
		store.Close()
		t.Fatal(err)
	}
	store.Close()
	if count != 1 {
		t.Fatal("dry run deleted controller state")
	}

	code, out, errout = invoke("maintenance", "compact", "--older-than", "48h", "--keep-audit", "1", "--apply", "--state", state, "--json")
	if code != 0 || !strings.Contains(out, `"applied": true`) || !strings.Contains(out, `"vacuumComplete": true`) {
		t.Fatal(code, out, errout)
	}
	store, err = core.OpenStore(state)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.DB.QueryRow("SELECT COUNT(*) FROM plans WHERE id='expired-fixture'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("applied maintenance did not remove expired plan")
	}
}
