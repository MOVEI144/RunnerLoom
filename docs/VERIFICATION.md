# Verification record

This file separates executable functionality from qualification evidence. The
detailed v1 gate ledger is in `docs/V1_ACCEPTANCE.md`.

## Automated tests

`go test -race -count=1 ./...` and `go vet ./...` run on the pinned Go 1.27.1
toolchain. Tests exercise strict configuration validation, cross-process
resource allocation, reservations, replay, restart recovery, encrypted JIT
persistence, approved enrollment, TLS identity/revocation, owned network/VM
operations, CLI setup, service rendering and link-local discovery parsing.

CI does not use macOS runners. The supported host is Ubuntu 24.04 x86_64. A
test-only `TestMain` harness still canonicalizes macOS `/var` so a laptop
`go test ./...` does not hit production symlink rejection; `PrivateDir` itself
is unchanged. The map of local commands, CI jobs and what a green check does
not prove is in `docs/TESTING.ja.md`.

The controller/agent integration tests use real TLS, HTTP, SQLite and the actual
agent loop, with explicit GitHub and hypervisor test doubles. They are not
evidence of real GitHub authorization or real hardware virtualization.

## Actual VM and Golden Image evidence

GitHub Actions run `34318841218`, job **Real Ubuntu VM lifecycle**, passed
against a real Ubuntu VM. It verified guest CPU/disk work, public HTTPS,
blocking access to a host-side probe service, guest poweroff and owned VM/disk
cleanup.

Release CI separately builds the Golden Image and boots that exact output. The
builder preserves the original partition identities and performs networked
package installation only inside a disposable QEMU VM. The final image carries
a machine-readable service policy; CI verifies all declared units are masked
and inactive, confirms the Runner binary, and records boot/image metrics. See
`docs/GOLDEN_IMAGE_POLICY.md`.

A green `checks` job alone is not a green real-VM or image-build job. Each job
publishes separate evidence artifacts.

## Live GitHub authorization

Live run `34447818805` completed a standard RunnerLoom-assigned job. Run
`34448089693` completed two concurrent standard jobs followed by a large-pool
job, including artifact upload. Run `34448420671` was cancelled while an active
ephemeral runner was being exercised. These runs demonstrate the tested
installation, not every possible reconnect, rate-limit or host-failure case.

`.github/workflows/v1-live-acceptance.yml` makes the full and cancellation lanes
repeatable without storing Organization App credentials in workflow files or
artifacts. The cancellation result must be correlated with Controller, GitHub
and host cleanup evidence; a `cancelled` conclusion alone is not cleanup proof.

## Retention and compaction

`runnerloom maintenance compact` is dry-run by default. While the Controller is
stopped, `--apply` can retire expired plans, expired unreferenced invitations
and old audit rows beyond a retained tail, then checkpoint and vacuum SQLite.
It deliberately preserves instances, SDK inbox replay rows, enrollments,
images, disks and unknown state. Age is not ownership or deletion proof.

## Image cache lifecycle

Cache tests cover content-addressed status, catalog and active-instance protection, real overlay backing-path protection, dry-run behavior, applied deletion under the Agent/Controller process lock, fail-closed handling of unknown or missing storage, partial-application reporting, same-filesystem hard-link seed, digest-mismatch quarantine and physical-link accounting. HTTP tests interrupt an Image response, retain a partial, resume with `Range`/`If-Range`, quarantine a corrupt completed entry, and verify the final digest before publication. Controller tests verify authenticated range responses retain the digest ETag and the extended Image-transfer deadline.

Applied cache pruning remains an operator action rather than an automatic pressure response. CI evidence does not prove that an arbitrary deployment has stopped every non-RunnerLoom process that might access its dedicated storage.

## Supported initial scope

- Ubuntu 24.04 x86_64 hosts, CPU VMs, one Controller and one or more trusted LAN Nodes.
- Private, explicitly selected repositories; no automatic public fork execution.
- Real libvirt/KVM lifecycle, image creation/import, TLS enrollment, CLI and systemd operation.
- Optional Avahi discovery; explicit reachable origins remain supported without discovery.

GPU passthrough, Windows/macOS hosts, distributed controller HA, arbitrary
hostile multi-tenant certification and automated cross-cloud routing are not
qualified. The Agent remains a trusted privileged host process, not an
independently audited privilege-separated helper. Physical two-Node, storage
pressure, complete backup/restore/fencing and expanded isolation campaigns stay
open in issue #4 until their evidence is recorded.

## Distribution and setup regressions

The release checks cover exact JSON field spelling, no-op configuration
application, invalid wizard CPU/RAM/disk input, invalid local-node configuration
before any Controller DB is written, state/disk directory overlap, systemd
argument expansion, terminal-safe human output and unchanged JSON, schema
export, recommended subnet conflicts, archive checksums, binary version/commit
identity, dependency license notices and Debian packages with no installation
side effects.
