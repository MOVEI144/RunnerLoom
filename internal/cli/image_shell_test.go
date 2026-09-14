package cli

import (
	"os/exec"
	"strings"
	"testing"
)

func TestGoldenImageBuilderQuotesRunnerURL(t *testing.T) {
	if !strings.Contains(imageBuildScript, "shlex.quote(url)") {
		t.Fatal("builder embeds the runner URL without shell quoting")
	}
	if strings.Contains(imageBuildScript, "replace('__URL__',url)") {
		t.Fatal("unquoted URL substitution remains")
	}
}

func TestOfflineImageCleanupShellOptions(t *testing.T) {
	_, body, ok := strings.Cut(imageBuildScript, "<<'CLEAN'\n")
	if !ok {
		t.Fatal("offline cleanup body missing")
	}
	body, _, ok = strings.Cut(body, "\nCLEAN\n")
	if !ok {
		t.Fatal("offline cleanup terminator missing")
	}
	// libguestfs runs --run bodies using sh. Execute ONLY the interpreter/options
	// preamble; never run the destructive cleanup commands on the test host.
	preamble, _, ok := strings.Cut(body, "\nrm -f ")
	if !ok || !strings.HasPrefix(preamble, "#!/bin/") {
		t.Fatal("unexpected cleanup preamble")
	}
	if output, err := exec.Command("/bin/sh", "-c", preamble).CombinedOutput(); err != nil {
		t.Fatalf("libguestfs /bin/sh rejects the cleanup preamble: %v: %s", err, output)
	}
}
