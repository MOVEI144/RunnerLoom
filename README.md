# RunnerLoom

**Ephemeral GitHub Actions runners on your own machines.**

RunnerLoom provisions a fresh Ubuntu VM for each GitHub Actions job, places it on an eligible LAN server, and removes the VM after the host confirms completion. One machine can be both controller and worker; additional workers use the same CLI and connect outbound over mutually authenticated TLS.

[Install / upgrade](docs/INSTALL.md) · [日本語セットアップガイド](docs/QUICKSTART.ja.md) · [Architecture](docs/ARCHITECTURE.md) · [Security](SECURITY.md) · [Verification and limitations](docs/VERIFICATION.md)

```text
GitHub Actions — outbound HTTPS — Controller
                                     │
                       outbound mTLS from each Node
                          ┌──────────┴──────────┐
                       Node A                 Node B
                      libvirt/KVM            libvirt/KVM
                     fresh VM → delete     fresh VM → delete
```

## What is implemented

- An operational Go CLI: interactive or JSON setup, configuration plans, enrollment, services, diagnostics and VM recovery.
- Official `actions/scaleset` integration: scale-set sessions, demand, durable message processing before acknowledgment, and one-job JIT configurations.
- SQLite transactions for shared CPU/RAM/disk accounting, hard reservations, operation replay protection and restart recovery.
- Approved node enrollment, pinned TLS 1.3, certificate renewal and per-request revocation checks. No remote administrative API for node credentials.
- Real libvirt VM creation, qcow2 overlays, cloud-init provisioning, bounded serial diagnostics, resource limits and owned-resource cleanup.
- Golden Image construction from a signature-verified Ubuntu image and a digest-verified official GitHub runner. No runner registration or controller key is baked into images.
- Dedicated NAT and firewall rules blocking guest access to private networks, host administration and other runner VMs. NAT alone is not treated as isolation.
- systemd installation with a non-root controller and a trusted privileged host agent.

A pool is an administrator-defined execution environment, not a request for arbitrary CPU/RAM from workflow code:

```yaml
jobs:
  build:
    runs-on: home-linux-lite
    steps:
      - run: python3 --version
```

## Install

Download the versioned `linux-amd64.tar.gz` or `.deb` from Releases and verify `SHA256SUMS`. Go is not needed for a packaged binary. Installation **does not** start services, enroll a node or modify networking. The private repository must first be made public by its owner before anonymous downloads work.

```bash
runnerloom version --json
runnerloom doctor
runnerloom config schema > cluster.schema.json
```

See [installation, verification and safe upgrades](docs/INSTALL.md).

## Start here

Use the [Japanese quickstart](docs/QUICKSTART.ja.md) for the complete sequence, required host packages, GitHub permissions and storage layout. `setup` does not falsely claim that writing a configuration has completed GitHub or VM verification.

```bash
runnerloom doctor
runnerloom config sample > cluster.json
runnerloom config validate --file cluster.json --json
runnerloom setup --file cluster.json --role controller-node --apply
runnerloom --help --json
```

Replace the sample image digest and machine budgets before use. First-time GitHub App installation/authorization is required; RunnerLoom does not extract or reuse your existing login token.

## Development

```bash
go test -race -count=1 ./...
go vet ./...
go build -trimpath -o runnerloom ./cmd/runnerloom
```

CI additionally runs a real disposable Ubuntu VM on a GitHub-owned ephemeral runner. Unit/integration mocks and real-VM evidence are identified separately. CI artifacts include the Linux/amd64 CLI, checksums and VM diagnostic evidence.

## Support boundary

This is a release candidate, not a claim of complete production qualification or hostile multi-tenant security. The current target is **Ubuntu 24.04 x86_64, trusted administrators and explicitly allowed private repositories**. GPU passthrough, Windows/macOS hosts, automatic controller HA and public fork jobs are not enabled. Guest isolation does not protect against an already-compromised host administrator or every hypervisor vulnerability.

Read [SECURITY.md](SECURITY.md) before connecting repositories or granting an agent host privileges. Repository visibility is an owner decision; adding an OSS license does not automatically publish a private repository.

## License

MIT. See [LICENSE](LICENSE). Third-party components keep their own licenses; image tooling downloads official upstream distributions rather than redistributing them in this repository.
