# Contributing

Use the Go version declared by the repository. Run `make check` before opening
a pull request. Preserve exact dependency versions and commit `go.sum` changes.
New resource mutations need restart/replay tests, capacity changes need
concurrent tests, and destructive host operations need ownership-negative tests.

The `control` integration suite uses real TLS, HTTP and SQLite with explicitly
mocked GitHub/hypervisor providers. Do not relabel those tests as real VM tests.
The dedicated CI jobs boot actual VMs on GitHub-owned ephemeral hosts.

Do not run `network apply`, `service install` or real VM tests on someone else's
host without explicit permission. The CI wrapper refuses non-CI execution;
`smoke-vm` is the explicit operator-facing command. Never add production GitHub
credentials or invitation secrets to test fixtures or CI logs.

Documentation and CLI help must identify unverified/unsupported operations
rather than report a successful setup merely because a JSON file was written.
User-facing errors should say what was checked and which changes were not made.
Security-sensitive reports belong in the private channel described in SECURITY.md.
