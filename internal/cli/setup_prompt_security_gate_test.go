package cli

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestInteractiveReleaseGateSanitizesLegacySetupPrompt(t *testing.T) {
	var out, errOut bytes.Buffer
	a := &App{In: strings.NewReader("\n"), Out: &out, Err: &errOut}
	value, err := a.prompt(bufio.NewReader(a.In), "label\x1b[2J", "/tmp/state\x1b[31m")
	if err != nil {
		t.Fatal(err)
	}
	if value != "/tmp/state\x1b[31m" {
		t.Fatalf("fallback changed: %q", value)
	}
	if strings.ContainsRune(errOut.String(), '\x1b') {
		t.Fatalf("legacy setup prompt emitted a terminal escape: %q", errOut.String())
	}
}
