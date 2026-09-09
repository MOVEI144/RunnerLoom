# Verification record

This file separates executable functionality from qualification evidence.

## Automated tests

`go test -race -count=1 ./...` and `go vet ./...` pass on the pinned Go 1.27.1 toolchain. Tests exercise strict configuration validation, cross-process resource allocation, reservations, replay, restart recovery, encrypted JIT persistence, approved enrollment, TLS identity/revocation, owned network/VM operations, CLI setup, service rendering and link-local discovery parsing.

The controller/agent integration tests use **real TLS, HTTP, SQLite and the actual agent loop**, with explicit GitHub and hypervisor test doubles. They are not evidence of real GitHub authorization or real hardware virtualization.

## Actual VM evidence

GitHub Actions run **34318841218**, job **Real Ubuntu VM lifecycle**, passed against a real Ubuntu VM. It verified guest CPU/disk work, public HTTPS, blocking access to a host-side probe service, guest poweroff and owned VM/disk cleanup. The run's separate Golden Image build failed in the libguestfs network appliance; the builder was subsequently changed to use a real disposable QEMU VM for network/package installation and libguestfs only offline.

The latest branch CI additionally qualifies the rebuilt Golden Image. A green `checks` job alone is not a green real-VM or image-build job. Each job publishes separate evidence artifacts. The final passing run is recorded in the merged pull request.

## Live GitHub authorization

The official `actions/scaleset` integration is implemented and compiled. A live API test is opt-in and skipped without an explicitly supplied dedicated owner-only credential file. This development session did **not** extract an existing login token or claim a user-authorized Organization Scale Set was created. First installation requires a GitHub App (recommended) or a dedicated token, a permitted Runner Group and explicit private Repository access. `runnerloom github check` verifies those settings. The user's first complete GitHub Job is a deployment acceptance check, not something that static/unit tests can substitute for.

## Supported initial scope

- Ubuntu 24.04 x86_64 hosts, CPU VMs, one Controller and one or more trusted LAN Nodes.
- Private, explicitly selected repositories; no automatic public fork execution.
- Real libvirt/KVM lifecycle, image creation/import, TLS enrollment, CLI and systemd operation.
- Optional Avahi discovery; explicit reachable origins remain supported without discovery.

GPU passthrough, Windows/macOS hosts, distributed controller HA, arbitrary hostile multi-tenant certification and automated cross-cloud routing are not qualified. Cache/history retention needs operator supervision; unknown files and active backing images are not deleted speculatively. The Agent remains a trusted privileged host process, not an independently audited privilege-separated helper.
