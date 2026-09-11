package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

func TestRootWithoutTTYPrintsHelpInsteadOfPrompting(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run(context.Background(), nil, strings.NewReader("1\n"), &out, &errOut)
	if code != 0 {
		t.Fatalf("unexpected exit code %d: %s", code, errOut.String())
	}
	combined := out.String() + errOut.String()
	if strings.Contains(combined, "メインメニュー") || strings.Contains(combined, "選択:") {
		t.Fatalf("non-TTY invocation entered interactive mode: %q", combined)
	}
	if !strings.Contains(combined, "Usage:") && !strings.Contains(combined, "使い方") {
		t.Fatalf("root help was not printed: %q", combined)
	}
}

func TestJSONRootNeverPrompts(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"--json"}, strings.NewReader("1\n"), &out, &errOut)
	if code != 0 {
		t.Fatalf("unexpected exit code %d: %s", code, errOut.String())
	}
	combined := out.String() + errOut.String()
	if strings.Contains(combined, "メインメニュー") || strings.Contains(combined, "選択:") {
		t.Fatalf("JSON invocation entered interactive mode: %q", combined)
	}
	if !strings.Contains(out.String(), `"commands"`) {
		t.Fatalf("machine-readable help missing: %q", out.String())
	}
}

func TestNonInteractiveRootNeverPrompts(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"--non-interactive"}, strings.NewReader("1\n"), &out, &errOut)
	if code != 0 {
		t.Fatalf("unexpected exit code %d: %s", code, errOut.String())
	}
	combined := out.String() + errOut.String()
	if strings.Contains(combined, "メインメニュー") || strings.Contains(combined, "選択:") {
		t.Fatalf("non-interactive invocation entered menu: %q", combined)
	}
}

func TestExplicitInteractiveRequiresTTY(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"interactive"}, strings.NewReader("0\n"), &out, &errOut)
	if code == 0 {
		t.Fatal("interactive command accepted non-TTY streams")
	}
	if !strings.Contains(errOut.String(), "TTY_REQUIRED") && !strings.Contains(errOut.String(), "端末") {
		t.Fatalf("missing TTY error: %q", errOut.String())
	}
}

func TestInteractiveMenuCanRunDoctorAndExit(t *testing.T) {
	var out, errOut bytes.Buffer
	a := &App{
		State: t.TempDir(),
		In:    strings.NewReader("2\n1\n0\n0\n"),
		Out:   &out,
		Err:   &errOut,
	}
	if err := a.runInteractive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut.String(), "メインメニュー") || !strings.Contains(errOut.String(), "状態・診断") {
		t.Fatalf("interactive navigation missing: %q", errOut.String())
	}
	if !strings.Contains(out.String(), "項目") {
		t.Fatalf("doctor output was not rendered: %q", out.String())
	}
}

func TestInteractiveEOFExitsCleanly(t *testing.T) {
	var out, errOut bytes.Buffer
	a := &App{State: t.TempDir(), In: strings.NewReader(""), Out: &out, Err: &errOut}
	if err := a.runInteractive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut.String(), "入力が終了") {
		t.Fatalf("EOF exit was not explained: %q", errOut.String())
	}
}

func TestInteractiveConfirmationRequiresExactPhrase(t *testing.T) {
	var out, errOut bytes.Buffer
	a := &App{In: strings.NewReader("apply\nAPPLY\n"), Out: &out, Err: &errOut}
	ok, err := a.interactiveConfirm("networkを変更", "APPLY")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("case-mismatched confirmation was accepted")
	}
	ok, err = a.interactiveConfirm("networkを変更", "APPLY")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("exact confirmation was rejected")
	}
}

func TestReadInteractiveLineBoundsInput(t *testing.T) {
	input := strings.Repeat("x", interactiveInputLimit+1) + "\n"
	if _, err := readInteractiveLine(strings.NewReader(input)); err == nil {
		t.Fatal("oversized interactive input was accepted")
	}
}

func TestReadInteractiveLineReturnsFinalLineAtEOF(t *testing.T) {
	value, err := readInteractiveLine(strings.NewReader("node-a"))
	if err != nil {
		t.Fatal(err)
	}
	if value != "node-a" {
		t.Fatalf("got %q", value)
	}
}

func TestReadInteractiveLinePropagatesReaderFailure(t *testing.T) {
	_, err := readInteractiveLine(io.MultiReader(strings.NewReader(""), failingReader{}))
	if err == nil || err.Error() != "boom" {
		t.Fatalf("unexpected error: %v", err)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestContainsStateFlag(t *testing.T) {
	for _, args := range [][]string{{"--state", "/tmp/a", "status"}, {"status", "--state=/tmp/a"}} {
		if !containsStateFlag(args) {
			t.Fatalf("state flag not detected in %#v", args)
		}
	}
	if containsStateFlag([]string{"status"}) {
		t.Fatal("state flag falsely detected")
	}
}
