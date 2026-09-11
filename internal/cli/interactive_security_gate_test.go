package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestInteractiveReleaseGateSanitizesDynamicConfirmation(t *testing.T) {
	var out, errOut bytes.Buffer
	a := &App{In: strings.NewReader("cancel\n"), Out: &out, Err: &errOut}
	ok, err := a.interactiveConfirm("unsafe\x1b[2J-summary", "APPROVE \x1b[31m")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("mismatched confirmation was accepted")
	}
	if strings.ContainsRune(errOut.String(), '\x1b') {
		t.Fatalf("unsanitized terminal escape reached output: %q", errOut.String())
	}
}

func TestInteractiveReleaseGateValidatesEnrollmentID(t *testing.T) {
	const valid = "abcdef0123456789abcdef0123456789"
	var out, errOut bytes.Buffer
	a := &App{In: strings.NewReader("not-valid\n" + valid + "\n"), Out: &out, Err: &errOut}
	got, err := a.interactiveID("申請ID")
	if err != nil {
		t.Fatal(err)
	}
	if got != valid {
		t.Fatalf("got %q want %q", got, valid)
	}
}
