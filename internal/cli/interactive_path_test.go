package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestInteractiveAbsolutePathPreservesArgumentBoundary(t *testing.T) {
	const path = "/tmp/runner loom;not-a-shell-command"
	var out, errOut bytes.Buffer
	a := &App{In: strings.NewReader(path + "\n"), Out: &out, Err: &errOut}
	got, err := a.interactiveAbsolutePath("path", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Fatalf("path boundary changed: got %q want %q", got, path)
	}
}
