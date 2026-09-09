package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MOVEI144/RunnerLoom/internal/core"
)

func TestWizardRejectsInvalidBudgetWithoutPanic(t *testing.T) {
	for _, budget := range []string{"0\n8192\n100\n", "-1\n8192\n100\n", "4\n512\n100\n", "4\n8192\n1\n", "65537\n8192\n100\n"} {
		t.Run(strings.ReplaceAll(budget, "\n", "-"), func(t *testing.T) {
			a := &App{State: filepath.Join(t.TempDir(), "state"), Err: io.Discard}
			prefix := "home\nhttps://github.com/MOVEI144\nMOVEI144/RunnerLoom\n1\n/var/lib/runnerloom/key.json\nsha256:" + strings.Repeat("1", 64) + "\nnode-a\nyes\n"
			_, e := a.wizard(bufio.NewReader(strings.NewReader(prefix + budget)))
			if e == nil {
				t.Fatal("invalid budget accepted")
			}
		})
	}
}
func TestInvalidNodeSetupDoesNotApplyControllerConfig(t *testing.T) {
	for _, args := range [][]string{{"--node", "missing-node"}, {"--node-state", "relative/path"}, {"--node-state", "/tmp/overlap/node", "--disk-dir", "/tmp/overlap"}} {
		dir := filepath.Join(t.TempDir(), "controller")
		base := []string{"setup", "--apply", "--file", configFixture(t), "--non-interactive", "--state", dir, "--json"}
		status, out, errout := invoke(append(base, args...)...)
		if status == 0 {
			t.Fatal(out, errout)
		}
		if _, e := os.Stat(filepath.Join(dir, "controller.db")); !os.IsNotExist(e) {
			t.Fatal("invalid node input saved controller configuration", e)
		}
	}
}
func TestSystemdDoesNotExpandEnvironmentInPaths(t *testing.T) {
	for _, s := range []string{"/var/lib/$HOME/state", "${HOME}", "$MAINPID"} {
		if _, e := systemdArg(s); e == nil {
			t.Fatal("environment expansion permitted", s)
		}
	}
}
func TestHumanOutputAndStableJSONAreSeparate(t *testing.T) {
	v := []core.Check{{Name: "check", Status: "pass", Detail: "normal\x1b]52;c;evil\a"}}
	var human, machine bytes.Buffer
	a := &App{Out: &human}
	if e := a.output(v); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(human.String(), "項目") || strings.Contains(human.String(), "\x1b") {
		t.Fatal("human table unsafe", human.String())
	}
	a.JSON = true
	a.Out = &machine
	if e := a.output(v); e != nil {
		t.Fatal(e)
	}
	var got struct {
		OK   bool         `json:"ok"`
		Data []core.Check `json:"data"`
	}
	if e := json.Unmarshal(machine.Bytes(), &got); e != nil || !got.OK || got.Data[0].Detail != v[0].Detail {
		t.Fatal("JSON contract changed", e)
	}
}
func TestSchemaCommandIsValidJSON(t *testing.T) {
	status, out, errout := invoke("config", "schema")
	var v map[string]any
	if e := json.Unmarshal([]byte(out), &v); e != nil || status != 0 || v["$schema"] == nil {
		t.Fatal(status, out, errout, e)
	}
	if !strings.Contains(out, `"additionalProperties": false`) {
		t.Fatal("schema is not strict")
	}
}
func TestImageBuilderPreservesBootAndRequiresSuccess(t *testing.T) {
	if strings.Contains(imageBuildScript, "virt-resize --") {
		t.Fatal("builder renumbers boot partitions")
	}
	for _, needle := range []string{"qemu-img convert -f qcow2", "qemu-img resize -f qcow2", "'growpart':", "RUNNERLOOM_BUILD_EXIT=0", "virt-customize --no-network", "--max-time 900"} {
		if !strings.Contains(imageBuildScript, needle) {
			t.Fatal("missing image build guard", needle)
		}
	}
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(imageBuildScript)
	if b, e := cmd.CombinedOutput(); e != nil {
		t.Fatal(e, string(b))
	}
}
