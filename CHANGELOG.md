# Changelog

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
