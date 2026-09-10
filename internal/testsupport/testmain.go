// Package testsupport contains process-wide test harness helpers.
package testsupport

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Run executes a package's tests. On macOS, it first moves TMPDIR below the
// canonical /private/var path so t.TempDir does not accidentally exercise
// RunnerLoom's production symlink rejection through the /var -> /private/var
// compatibility symlink. Production path validation is deliberately unchanged.
func Run(m *testing.M) int {
	if runtime.GOOS != "darwin" {
		return m.Run()
	}

	original, hadOriginal := os.LookupEnv("TMPDIR")
	base, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		fmt.Fprintf(os.Stderr, "runnerloom test harness: resolve TMPDIR: %v\n", err)
		return 2
	}
	if !filepath.IsAbs(base) || filepath.Clean(base) != base {
		fmt.Fprintf(os.Stderr, "runnerloom test harness: resolved TMPDIR is not canonical: %q\n", base)
		return 2
	}
	root, err := os.MkdirTemp(base, "runnerloom-go-test-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "runnerloom test harness: create canonical TMPDIR: %v\n", err)
		return 2
	}
	if err = os.Chmod(root, 0o700); err != nil {
		_ = os.RemoveAll(root)
		fmt.Fprintf(os.Stderr, "runnerloom test harness: protect canonical TMPDIR: %v\n", err)
		return 2
	}
	if err = os.Setenv("TMPDIR", root); err != nil {
		_ = os.RemoveAll(root)
		fmt.Fprintf(os.Stderr, "runnerloom test harness: set TMPDIR: %v\n", err)
		return 2
	}

	code := m.Run()
	if hadOriginal {
		_ = os.Setenv("TMPDIR", original)
	} else {
		_ = os.Unsetenv("TMPDIR")
	}
	if err = os.RemoveAll(root); err != nil && code == 0 {
		fmt.Fprintf(os.Stderr, "runnerloom test harness: remove canonical TMPDIR: %v\n", err)
		return 1
	}
	return code
}
