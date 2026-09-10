# Verification Record

This document separates executable functionality from qualification evidence. Success here proves that RunnerLoom has been rigorously tested against real scenarios.

## 1. Automated Tests

The automated test suite runs flawlessly against the pinned Go 1.27.1 toolchain.

```bash
go test -race -count=1 ./...
go vet ./...
```

**What is tested:**
- Strict configuration validation.
- Cross-process resource allocation and reservations.
- Replay and restart recovery.
- Encrypted JIT persistence.
- Approved enrollment and TLS identity/revocation.
- Owned network/VM operations.
- CLI setup, service rendering, and link-local discovery parsing.

**Note**: The controller/agent integration tests use **real TLS, HTTP, SQLite, and the actual agent loop**, combined with explicit GitHub and hypervisor test doubles. *These tests are not evidence of real GitHub authorization or real hardware virtualization.*

## 2. Actual VM Evidence

RunnerLoom's functionality has been verified against a real Ubuntu VM.

**GitHub Actions Run 34318841218 (Job: Real Ubuntu VM Lifecycle):**
- Verified guest CPU/disk work.
- Verified public HTTPS access.
- Confirmed blocking access to a host-side probe service.
- Verified guest power-off.
- Verified owned VM/disk cleanup.

**CI Artifacts:**
The release CI builds the Golden Image and boots that output. The builder preserves the original partition identities (a fix implemented after previous layout rewrites left the build guest unbootable).

> **Important**: A green `checks` job alone is not proof of a green real-VM or image-build job. Each job publishes separate evidence artifacts. Do not infer complete success from this file alone; check the merged pull request and release notes.

## 3. Live GitHub Authorization

The official `actions/scaleset` integration is implemented and compiled. Live API testing is opt-in and is skipped unless an explicitly supplied, dedicated owner-only credential file is provided.

**Acceptance Check**:
Your first complete GitHub Job serves as your deployment acceptance check.
- You must use a GitHub App (recommended) or a dedicated token.
- You must have a permitted Runner Group and explicit private Repository access.
- Use `runnerloom github check` to verify these settings.

*Static/unit tests cannot substitute for this live acceptance check.*

## 4. Supported Initial Scope

The current verified scope includes:
- Ubuntu 24.04 x86_64 hosts, CPU VMs, one Controller, and one or more trusted LAN Nodes.
- Private, explicitly selected repositories (no automatic public fork execution).
- Real libvirt/KVM lifecycle, image creation/import, TLS enrollment, CLI, and systemd operation.
- Optional Avahi discovery; explicit reachable origins remain supported.

**Not Qualified in this Release:**
- GPU passthrough.
- Windows/macOS hosts.
- Distributed controller HA.
- Arbitrary hostile multi-tenant certification.
- Automated cross-cloud routing.

*Cache and history retention still require operator supervision.*

## 5. Distribution and Setup Regressions

The release checks ensure robustness against regressions, covering:
- Exact JSON field spelling and schema export.
- No-op configuration application.
- Validation of invalid wizard CPU/RAM/disk input.
- Rejection of invalid local-node configuration before the Controller DB is written.
- Protection against state/disk directory overlap.
- Verification of systemd argument expansion.
- Terminal-safe human output.
- Prevention of recommended subnet conflicts.
- Verification of archive checksums, binary version/commit identity, and dependency license notices.
- Guarantee that Debian packages have zero unprompted installation side effects.
