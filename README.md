# RunnerLoom

[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/github/go-mod/go-version/your-org/runnerloom)](go.mod)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](CONTRIBUTING.md)

> **Ephemeral GitHub Actions runners on your own machines.**

RunnerLoom provisions a fresh Ubuntu VM for each GitHub Actions job, places it on an eligible LAN server, and securely removes the VM after completion. You can run both the controller and worker on a single machine, or easily scale by adding more worker nodes that connect outbound over mutually authenticated TLS (mTLS).

[**Install & Upgrade**](docs/INSTALL.md) · [**日本語クイックスタート**](docs/QUICKSTART.ja.md) · [**Architecture**](docs/ARCHITECTURE.md) · [**Security**](SECURITY.md) · [**Verification**](docs/VERIFICATION.md)

---

## Architecture Overview

```text
GitHub Actions — outbound HTTPS — Controller
                                     │
                       outbound mTLS from each Node
                          ┌──────────┴──────────┐
                       Node A                 Node B
                      libvirt/KVM            libvirt/KVM
                     fresh VM → delete     fresh VM → delete
```

## Key Features

- **JIT Configuration**: Official `actions/scaleset` integration ensures secure, JIT configurations for each job.
- **True Isolation**: Uses real libvirt VMs, qcow2 overlays, and cloud-init for isolated, ephemeral environments. Dedicated NAT and firewall rules block guest access to private networks and other VMs.
- **Secure by Default**: Requires approved node enrollment, pinned TLS 1.3, and per-request revocation checks. Base images are built from signature-verified Ubuntu images and digest-verified official runners.
- **Resource Management**: Uses SQLite for durable, transactional tracking of CPU, RAM, and disk quotas. Support for hard reservations and protection against operation replays.
- **Operational Simplicity**: A single Go binary that provides the CLI, controller, and agent.

Pools provide well-defined execution environments rather than arbitrary resource requests from workflow code:

```yaml
jobs:
  build:
    runs-on: home-linux-lite
    steps:
      - run: python3 --version
```

## Getting Started

### Installation

Download the latest versioned `linux-amd64.tar.gz` or `.deb` from the Releases page and verify the `SHA256SUMS`. Go is not required for the pre-packaged binary.

*Note: Installation simply places the binary; it does not start services or modify your network.*

```bash
runnerloom version --json
runnerloom doctor
runnerloom config schema > cluster.schema.json
```

For detailed instructions, see [Installation & Upgrades](docs/INSTALL.md).

### Quick Setup

We highly recommend following the comprehensive [Japanese Quickstart Guide (日本語セットアップガイド)](docs/QUICKSTART.ja.md) for the complete setup sequence, required host packages, and GitHub App authorization.

A typical initialization looks like this:

```bash
runnerloom doctor
runnerloom config sample > cluster.json
runnerloom config validate --file cluster.json --json
runnerloom setup --file cluster.json --role controller-node --apply
runnerloom --help --json
```

*Don't forget to replace the sample image digest and machine budgets with your real configuration before applying.*

## Development & Testing

RunnerLoom is built with Go. To compile and run the test suite locally:

```bash
go test -race -count=1 ./...
go vet ./...
go build -trimpath -o runnerloom ./cmd/runnerloom
```

Our CI pipeline provisions real disposable Ubuntu VMs to ensure comprehensive testing.

## Support & Limitations

RunnerLoom is currently in **Release Candidate** status.

- **Target Environment**: Ubuntu 24.04 x86_64, managed by trusted administrators.
- **Allowed Repositories**: Explicitly permitted private repositories only.
- **Current Limitations**: GPU passthrough, Windows/macOS hosts, automatic controller HA, and public fork jobs are **not** supported yet.

Please read the [Security Model](SECURITY.md) carefully before connecting your repositories or granting host privileges to the agent.

## License

RunnerLoom is licensed under the [MIT License](LICENSE).

Third-party components retain their own licenses. The image builder downloads official upstream distributions dynamically rather than redistributing them within this repository.
