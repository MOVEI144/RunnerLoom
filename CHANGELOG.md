# Changelog

## 0.1.0-rc.2

Second Linux/amd64 CPU/LAN release candidate, focused on portability, Golden Image hardening and qualification evidence.

- Make the documented repository-wide Go tests pass on macOS without weakening production private-path symlink checks.
- Minimize unnecessary services in ephemeral Golden Images and boot the finished image to verify every required mask.
- Verify retained image capabilities including networking, CA trust, Git, Python, build tools, jq, sudo and the official Actions Runner.
- Add reusable live acceptance lanes for concurrent standard runners, large runners, artifact evidence and external cancellation.
- Add dry-run-first Controller maintenance/SQLite compaction while preserving instance ownership and SDK replay evidence.
- Report maintenance as applied once its deletion transaction commits, even if later checkpoint or VACUUM work fails.
- Preserve qualification artifacts on failed acceptance runs for diagnosis.
- Keep deployment-only v1 gates explicit: two physical LAN Nodes, restricted privileged-helper qualification, storage-pressure campaigns, complete backup/restore fencing and expanded isolation remain separate evidence requirements.

See `docs/VERIFICATION.md` and `docs/V1_ACCEPTANCE.md` for the qualification boundary.

## 0.1.0-rc.1

First Linux/amd64 CPU/LAN release candidate. This is not a GPU or high-availability release.

- Operational controller, outbound authenticated agents and disposable libvirt VMs.
- Resource accounting with hard reservations, crash-safe replay and fail-closed cleanup.
- Official GitHub Scale Set adapter with durable message processing and one-job credentials.
- Pinned node identity, explicit enrollment approval, revocation and certificate renewal.
- Verified base/Runner downloads and an unregistered, reusable Ubuntu image builder.
- Preserve boot partition identities when expanding images; root expansion occurs inside the guest.
- Strict case-sensitive JSON configuration, schema export and idempotent no-change application.
- Validate interactive resource input before calculating Pool capacity or writing Controller state.
- Readable CLI tables, terminal-safe guest logs, read-only subnet recommendation and strict doctor mode.
- Versioned archives and Debian packages with checksums, build provenance and dependency notices.
- Installation does not start a daemon, change host networking or enroll a machine.

See `docs/VERIFICATION.md` for the boundary between implemented, tested and unqualified behavior.
