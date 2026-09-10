# Contributing to RunnerLoom

Thank you for your interest in contributing to RunnerLoom! We welcome bug reports, feature requests, and pull requests.

## Prerequisites

- **Go version**: Ensure you are using the Go version declared in `go.mod`.
- **Python version**: Packaging requires Python 3.12 or newer (uses `python3.12` by default; you can override this with the `PYTHON` variable for `make package`). The pinned Ubuntu 24.04 CI image provides Python 3.12, which the packaging and release steps invoke explicitly.

## Development & Testing

Please ensure your changes are well-tested.

### Running Tests

Run the following command before opening a pull request to ensure all checks pass:

```bash
make check
```

- Preserve exact dependency versions and always commit `go.sum` changes.
- **New Mutations**: Any new resource mutations require restart/replay tests.
- **Capacity Changes**: Changes to capacity require concurrent tests.
- **Destructive Operations**: Destructive host operations need ownership-negative tests.

### Test Suites

- **Integration Tests**: The `control` integration suite uses real TLS, HTTP, and SQLite with explicitly mocked GitHub/hypervisor providers. **Do not** relabel these tests as real VM tests.
- **VM Tests**: The dedicated CI jobs boot actual VMs on GitHub-owned ephemeral hosts. Never run `network apply`, `service install`, or real VM tests on someone else's host without explicit permission.

The CI wrapper will refuse non-CI execution. If you need to perform explicit operator-facing commands, use `smoke-vm`.

## Submitting Pull Requests

- **No Secrets**: Never add production GitHub credentials or invitation secrets to test fixtures or CI logs.
- **Honest Feedback**: Documentation and CLI help must identify unverified/unsupported operations, rather than reporting a successful setup merely because a JSON file was written. User-facing errors should clearly state what was checked and which changes were not made.
- **Security Reports**: Security-sensitive issues do not belong in public pull requests or issues. Please report them in the private channel as described in [SECURITY.md](SECURITY.md).
