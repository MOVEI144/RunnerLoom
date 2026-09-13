# Contributing

Run `make check` from the repository root before opening a pull request.
What each test layer proves, and what a green check does not prove, is in
[docs/TESTING.md](docs/TESTING.md).

## Requirements

- The Go version in `go.mod`
- Python 3.12 only for packaging (`make package`; override with `PYTHON`)
- Commit `go.sum` changes

```bash
make check
```

`make check` runs `gofmt`, `go vet`, `go test -race -count=1 ./...`, and
syntax checks on related shell scripts. It does not build a real VM, a
Golden Image, or a `.deb`.

## Self-review before you ask for review

After tests pass, read your own diff before opening the PR. The author is
the first reviewer.

Check that:

- The diff has no unsolicited files or formatting-only noise
- Secrets, invitations, JIT configs, and host-specific paths or keys are absent
- A failing test actually pins the contract you changed
- Docs, `--help`, and JSON do not report unfinished work as success
- You can describe what was seen with mocks versus what was not seen on a
  real VM or live GitHub

## Required tests by change type

| If you change | Minimum required |
|---|---|
| Config, JSON, or CLI output | Existing CLI / decode tests. Flag names come from `runnerloom --help --json`; config keys come from `runnerloom config schema` |
| Resource accounting, reservations, or placement | Replay and concurrent tests |
| Stop, delete, network, or cache prune | Negative tests that refuse to destroy unowned resources |
| libvirt, images, or isolation | The CI real-VM jobs, or `smoke-vm`. Do not call the mocked `control` integration tests a real-VM pass |
| Documentation | `python3.12 scripts/check-docs.py` |

The current product host and CI runners are Ubuntu 24.04 x86_64. macOS and
Windows are not qualified hosts yet and may be added later. Do not weaken
production path checks, symlink rejection, or OS assumptions to make a
laptop test pass.

`go test ./...` is still expected to run on a macOS laptop. The test-only
`TestMain` harness canonicalizes the `/var` compatibility symlink. That
harness is not a production change.

## Do not

- Run `network apply`, `service install`, or real VM tests on someone else's
  host without explicit permission
- Use the CI-only wrappers as a local real-VM path; operators use `smoke-vm`
- Put production GitHub credentials, invitation secrets, or JIT configs in
  tests or CI logs
- Delete instance, inbox, enrollment, image, or unknown host state merely
  because it is old. `maintenance compact` is dry-run by default and must be
  applied only while the Controller is stopped
- Describe setup as complete because a JSON file was written. Say what was
  not checked
- File secrets in a public vulnerability issue. Use [SECURITY.md](SECURITY.md)

## Documents

Operator procedures start at [docs/README.md](docs/README.md).
Design is [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md). Recorded evidence is
[docs/VERIFICATION.md](docs/VERIFICATION.md).
