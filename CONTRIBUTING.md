# Contributing

Use the Go version declared by the repository. Run `make check` before opening
a pull request. Packaging requires Python 3.12 or newer (`python3.12` by default;
override `PYTHON` for `make package`). The pinned Ubuntu 24.04 CI image provides
Python 3.12, which the packaging and release steps invoke explicitly.
Preserve exact dependency versions and commit `go.sum` changes.
New resource mutations need restart/replay tests, capacity changes need
concurrent tests, and destructive host operations need ownership-negative tests.

The repository-wide `go test ./...` command is supported directly on macOS. The
test-only `TestMain` harness canonicalizes macOS's temporary path before
`t.TempDir()` is used; do not weaken `core.PrivateDir` or production symlink
rejection to make a test pass.

The `control` integration suite uses real TLS, HTTP and SQLite with explicitly
mocked GitHub/hypervisor providers. Do not relabel those tests as real VM tests.
The dedicated CI jobs boot actual VMs on GitHub-owned ephemeral hosts.

Do not run `network apply`, `service install` or real VM tests on someone else's
host without explicit permission. The CI wrapper refuses non-CI execution;
`smoke-vm` is the explicit operator-facing command. Never add production GitHub
credentials or invitation secrets to test fixtures or CI logs.

Controller compaction must be previewed with `runnerloom maintenance compact`
and applied only while the Controller service is stopped. Never delete instance,
inbox, enrollment, image or unknown host state merely because it is old.

Documentation and CLI help must identify unverified/unsupported operations
rather than report a successful setup merely because a JSON file was written.
User-facing errors should say what was checked and which changes were not made.
Security-sensitive reports belong in the private channel described in SECURITY.md.
