package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MOVEI144/RunnerLoom/internal/core"
)

func TestCIShellScriptsParseIndividually(t *testing.T) {
	for _, name := range []string{"scripts/ci-vm-smoke.sh", "scripts/ci-free-space.sh", "internal/cli/image-build.sh"} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command("bash", "-n", filepath.Join("..", "..", name))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("invalid shell script: %v: %s", err, out)
			}
		})
	}
}

func smokeScript(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../scripts/ci-vm-smoke.sh")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCISmokeDoesNotCopySuppliedImage(t *testing.T) {
	script := smokeScript(t)
	start := strings.Index(script, `if [[ -n "${RUNNERLOOM_SMOKE_IMAGE:-}" ]]`)
	end := strings.Index(script, `sudo python3 - "$STATE"`)
	if start < 0 || end <= start {
		t.Fatal("image-selection block missing")
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "golden image.qcow2")
	data := []byte("a small fixture, not a bootable image")
	if err := os.WriteFile(input, data, 0600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "stage")
	evidence := filepath.Join(dir, "evidence")
	for _, path := range []string{root, evidence} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	// Exercise ONLY image selection. No VM, host cleanup, network or real sudo.
	// The command stub refuses any attempt to copy/chown/chmod the input.
	stub := "#!/bin/sh\n[ \"$1\" = sha256sum ] || exit 91\nshift\nexec sha256sum \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", "set -euo pipefail\n"+script[start:end]+"\nprintf '%s' \"$IMAGE\"\n")
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"),
		"RUNNERLOOM_SMOKE_IMAGE="+input, "ROOT="+root, "EVIDENCE="+evidence)
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != input {
		t.Fatalf("source image not passed directly: %v: %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(root, "base.qcow2")); !os.IsNotExist(err) {
		t.Fatal("redundant image copy was created", err)
	}
	digest, err := os.ReadFile(filepath.Join(evidence, "ubuntu-image.sha256"))
	if err != nil || strings.TrimSpace(string(digest)) != core.Hash(data) {
		t.Fatal("image digest not verified", err)
	}
	st, err := os.Stat(input)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("input permissions changed", err)
	}
	if !strings.Contains(script, `--image "$IMAGE" --digest "sha256:$SHA"`) {
		t.Fatal("smoke-vm no longer consumes the verified original image")
	}
}

func TestCISmokeRejectsNonHostedRunners(t *testing.T) {
	script := smokeScript(t)
	preamble, _, found := strings.Cut(script, "ROOT=")
	if !found {
		t.Fatal("CI guard missing")
	}
	for _, env := range []string{"self-hosted", ""} {
		cmd := exec.Command("bash", "-c", preamble)
		cmd.Env = append(os.Environ(), "GITHUB_ACTIONS=true", "RUNNER_ENVIRONMENT="+env)
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "disposable GitHub-hosted") {
			t.Fatalf("unsafe runner accepted: %q: %v: %s", env, err, out)
		}
	}
}

func TestInvalidInviteOutputDoesNotPersistInvitation(t *testing.T) {
	for _, kind := range []string{"existing-file", "non-private-parent", "relative-path"} {
		t.Run(kind, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "controller")
			status, out, errout := invoke("setup", "--file", configFixture(t), "--apply",
				"--role", "controller", "--non-interactive", "--state", dir, "--json")
			if status != 0 {
				t.Fatal(out, errout)
			}
			output := filepath.Join(t.TempDir(), "invite.json")
			switch kind {
			case "existing-file":
				if err := os.WriteFile(output, []byte("keep me"), 0600); err != nil {
					t.Fatal(err)
				}
			case "non-private-parent":
				if err := os.Chmod(filepath.Dir(output), 0755); err != nil {
					t.Fatal(err)
				}
			case "relative-path":
				output = "must-not-be-created-invite.json"
			}
			status, out, errout = invoke("node", "invite", "--url", "https://controller.example:8443",
				"--out", output, "--state", dir, "--json")
			if status == 0 {
				t.Fatal("invalid output accepted", out, errout)
			}
			store, err := core.OpenStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			var count int
			if err = store.DB.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM invites").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("invalid output persisted %d orphan invitations", count)
			}
			if kind == "existing-file" {
				contents, err := os.ReadFile(output)
				if err != nil || string(contents) != "keep me" {
					t.Fatal("existing invitation overwritten", err)
				}
			}
		})
	}
}

func TestMissingInputUsesDocumentedExitCode(t *testing.T) {
	status, out, _ := invoke("setup", "--non-interactive", "--json", "--state", filepath.Join(t.TempDir(), "state"))
	if status != 2 || !strings.Contains(out, "MISSING_INPUT") {
		t.Fatal(status, out)
	}
}
