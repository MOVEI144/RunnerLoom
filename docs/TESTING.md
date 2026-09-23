# Testing

This page is what each test proves. Recorded run IDs are in
[VERIFICATION.md](VERIFICATION.md). Remaining v1 evidence is in
[V1_ACCEPTANCE.md](V1_ACCEPTANCE.md).

How to open a pull request is in [CONTRIBUTING.md](../CONTRIBUTING.md).

## Start here

| Question | Use | What it tells you |
|---|---|---|
| Did this change break an existing contract? | `make check` | Formatting, vet, and race-enabled repository tests |
| Will the GitHub PR run the same checks? | RunnerLoom CI `checks` | The above, plus fuzz, `.deb` packaging, and `govulncheck` |
| Does a real Ubuntu VM start, work, and get deleted? | CI `real-vm`, or `runnerloom smoke-vm` | One disposable lifecycle on a GitHub-hosted runner |
| Can a signed Golden Image be built and booted? | CI `golden-image` | Canonical signatures, runner digest, masked services, runner binary after boot |
| Do Controller and Agent sync over real TLS? | `internal/control` integration tests | Real TLS / HTTP / SQLite. GitHub and the hypervisor are explicit fakes |
| Do two real VMs enroll and clean up with ownership checks? | `e2e` qualification (`main` only) | Real libvirt. GitHub demand is a fixture. Not a home cluster |
| Are doc links and headings intact? | `python3.12 scripts/check-docs.py` | Offline local check |
| Did a live GitHub job complete once? | The live workflow in [V1_ACCEPTANCE.md](V1_ACCEPTANCE.md) | Evidence for that deployment. A green CI job is not a substitute |

## Local

```bash
make check
```

That runs:

```text
gofmt
go vet ./...
go test -race -count=1 ./...
bash -n   (image-build / ci-vm-smoke / ci-free-space)
```

Packaging only:

```bash
make package
```

Create, run, and delete a VM on your own machine only:

```bash
runnerloom smoke-vm
```

`internal/e2e` is `//go:build linux`. That package does not build on other OSes.

## CI jobs

Product CI is the table below. A GitHub Release is created only by
`release.yml` after a successful **push** of RunnerLoom CI on `main` (not a
pull request or `workflow_dispatch`), when the three qualification jobs below
all succeeded on that same run.

| Workflow | Job | When | Proves | Does not prove |
|---|---|---|---|---|
| RunnerLoom CI | Go, race, integration and CLI (`checks`) | Push to `main`, pull requests, manual | Units, concurrency, mTLS integration, fuzz, Debian package, shipped-binary vuln scan | Real KVM, a live GitHub job; `make check`'s `bash -n` |
| RunnerLoom CI | Real Ubuntu VM lifecycle | After `checks`, on same-repo PRs and `main` | Signed base, boot, guest work, host-probe rejection, poweroff, owned disk deletion; the agent-task guest runner (`smoke-vm --task`) | Official-runner Golden Image, multiple nodes, real agent CLIs |
| RunnerLoom CI | Build verified runner image and boot it | After `checks`, on same-repo PRs and `main` | Unregistered Golden Image build, service masks, runner binary, smoke on that image | Home-host capacity or LAN isolation campaigns |
| RunnerLoom runtime qualification | Build image, enroll Node, run two REAL VMs… | Push to `main` and manual | Two real libvirt VMs, enroll, cleanup | Live GitHub API, a home node |
| RunnerLoom runtime qualification | Live GitHub prerequisites ONLY | Same as above | Whether dedicated secrets exist | Running a job |
| RunnerLoom documentation | Reader paths and local links | Documentation changes | Procedure links and headings | Design correctness |
| RunnerLoom v1 live acceptance | Manual | `workflow_dispatch` | A real job on an allowed private repository | A substitute for CI |
| Publish verified release | After RunnerLoom CI succeeds on a `main` **push** | Same-repo CI artifact | Tagged GitHub Release of that `VERSION` | PR CI, dispatch re-runs, e2e, live acceptance |

Current CI runners and the qualified product host are Ubuntu 24.04 x86_64.
macOS and Windows hosts are not qualified yet and may be added later. CI does
not use macOS or Windows runners today.

## What each package guards

| Package | Guards |
|---|---|
| `internal/core` | Config, resource ledger, reservations, placement reasons, SQLite, JIT encryption, CA / invitations |
| `internal/control` | Node protocol only. No admin API. Real TLS plus fake GitHub/hypervisor |
| `internal/agent` | Outbound mTLS, ensure/stop/delete, image fetch |
| `internal/github` | Official `actions/scaleset`, persist-before-ACK, no redirects |
| `internal/host` | libvirt ownership, dedicated network, fail-closed cache prune, resumable download |
| `internal/cli` | Human CLI and `--json`, interactive mode on a TTY only, no secrets in output |
| `internal/discovery` | Avahi records are untrusted hints |
| `internal/e2e` | Real libvirt. GitHub side is a diagnostic fixture |
| `internal/tasks` | Task-client identity files, allow list, credential collection (no symlinks, size limits) |
| `internal/mcp` | MCP JSON-RPC over stdio and Streamable HTTP, tool errors, secret and Origin checks |

Agent tasks are covered by `internal/core` (placement, encryption, erasure, revocation, state derivation), `internal/control` (client → Controller → Agent over real mTLS with a fake hypervisor), `internal/mcp` (JSON-RPC and HTTP transport) and `internal/host`. The `internal/host` tests run the real guest runner script against a local git server without a VM. The CI `real-vm` job also runs `smoke-vm --task`: the real guest runner in a real VM with a fixed shell agent, checking the user switch, no sudo, that the agent cannot read the token, a public github.com clone, a push refused for an invalid token, the returned patch and VM deletion. No test runs a real agent CLI or logs in to a real agent service.

Do not call the `control` integration tests a real-VM test. Real VMs are
`real-vm`, `golden-image`, `e2e`, and `smoke-vm`.

## Still unproven when CI is green

A green CI run does not prove:

- Two physical LAN nodes
- Jobs from public forks
- GPU, a qualified Windows/macOS host, or controller HA
- A complete backup/restore and fencing of the old controller
- Storage-pressure or peer-VM isolation campaigns on real hardware

Those remain deployment evidence in [V1_ACCEPTANCE.md](V1_ACCEPTANCE.md).
