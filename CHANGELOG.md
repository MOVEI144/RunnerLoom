# Changelog

## 0.1.0-rc.6

Sixth Linux/amd64 CPU/LAN release candidate. Adds agent tasks: a self-hosted "cloud agent" that runs long coding tasks from Claude Code, Codex or ChatGPT in disposable VMs.

- Add agent tasks: approved task clients submit long-running coding tasks that run one agent CLI in a disposable VM on Pools marked `tasks: true`. Built-in profiles cover claude, codex, opencode, pi, gemini, grok (Grok Build), musecode and agy (Antigravity); others can be added in `agents.json`. Results come back as a pushed work branch, an optional draft PR, or a patch.
- Add task-client identities (`client init/approve/install/allow/revoke`), with keys generated on the PC, a certificate URI kind separate from Nodes, CSR and CA fingerprint checks during enrollment, and automatic certificate renewal with a 7-day overlap.
- Add `runnerloom mcp serve` (stdio and loopback-only Streamable HTTP) for Claude Code, Codex, the ChatGPT desktop app and ChatGPT web through a tunnel. Add a Claude Code plugin and marketplace.
- Run the agent as the unprivileged runner user without sudo by default. Keep the GitHub token out of every agent-owned process: commit as the runner without secrets, kill the runner's processes, and push as root from a separate clean repository. The token is sent only for github.com repositories.
- Never read well-known secret locations as agent credentials, and pin allowed custom profiles by digest (`AGENT_CHANGED`).
- Encrypt task payloads and results at rest; erase payloads after confirmed deletion, queue expiry, early cancellation or client revocation. Accept guest results only from the owning Node after host-confirmed shutdown, and only for the task's own branch and repository.
- Revoking a client cancels its unfinished tasks and stops placed VMs. Cancelling never relabels a task that already has a result.
- Place queued tasks first-in first-out per Pool, refuse to start a task VM with under 10 minutes left, and require task Pools to allow at least 30 minutes.
- Allow a tasks-only cluster without a `github` section; the GitHub manager starts only when a GitHub Pool exists.
- Add `smoke-vm --task`, which runs the real guest runner in a real VM, and run it in CI.
- Publish macOS (arm64 and amd64) task-client archives. Windows clients are not provided.
- Database schema version 3, which adds the `clients` and `tasks` tables. Older binaries refuse this database.

## 0.1.0-rc.5

Fifth Linux/amd64 CPU/LAN release candidate.

- Keep an in-flight job-acquire fence separate from the host-stop demand barrier so `RefreshDemand` cannot unblock unacked `AcquireJobs`.
- Treat GitHub account type `Organization` as fail-closed for runner-group lookup, including HTTP 404; skip the group API only for `User` accounts.
- Advertise Golden Image digests to a Node that still has a non-deleted instance after its Pool is disabled.
- Quote the official runner download URL inside the Golden Image builder and reject shell metacharacters in that URL.
- Disable Actions Runner self-update on newly created scale sets so the guest keeps the image-pinned binary.
- Remove leftover `internal-*` GitHub Actions and `scripts/apply-interactive-rc4.py`.
- Document `release.yml` as the only publisher, index the interactive CLI, and use a fictional private repository in samples.

## 0.1.0-rc.4

Fourth Linux/amd64 CPU/LAN release candidate, focused on fail-closed job intake and host lifecycle.

- Keep `stop`/`delete` working after a node ceiling shrink; only `ensure` is gated on the current ceiling.
- Retry scale-set acquire/ACK on the same session; fence demand until every job request ID is acquired; do not ACK a partial acquire.
- Remove leaked GitHub JIT runners when the returned name does not match or persistence fails.
- Rebuild `seed.iso` on `prepared` retries so guest timeouts follow the current deadline.
- Serialize `network apply` with the agent lock and refuse defined, not only running, RunnerLoom domains.
- Enforce Runner Group selected/private policy for organization owners even when `github.url` is a single repository; skip the group API only on HTTP 404 (personal accounts).
- Serve Golden Images for disabled pools only to nodes that still have a non-deleted instance on that digest.
- Prefer delete/stop commands over `ensure`, and do not `ensure` a VM reported `Unknown`.
- Open secrets with `O_NOFOLLOW`, re-check private directories after `MkdirAll`, and create invitation files exclusively.
- Pin peer certificates in `Authorize` and refresh the stored hash on renew.
- Require free VM storage to cover outstanding disk commitments plus the incoming VM and the 2 GiB safety margin.

## 0.1.0-rc.3

Third Linux/amd64 CPU/LAN release candidate, focused on bounded Image cache lifecycle and clearer operations.

- Add `cache status` for Controller and Node capacity, references, partial downloads, base hard links, verification and safe-prune readiness.
- Add dry-run-first `cache prune`; applied cleanup requires the owning Controller/Agent to be stopped and rescans catalog, instance manifests and real qcow2 overlay backing paths under locks.
- Keep active, stopping, unknown and currently catalogued Image digests protected; fail closed on unknown files, symlinks, malformed manifests or mismatched base links.
- Add `cache seed` to hard-link a verified Controller Image into a same-filesystem Node cache for single-host deduplication, without silently copying across filesystems.
- Resume interrupted authenticated Node Image downloads with digest-pinned ETag, `Range` and `If-Range`, then re-check the complete SHA-256 and qcow2 structure before atomic publication.
- Count retained partials against cache limits and preserve a fixed 2 GiB filesystem safety reserve for new imports/downloads.
- Preserve digest-mismatched completed cache files under explicit quarantine names before recovery; report and age-gate their later cleanup instead of overwriting diagnostic evidence.
- Report partially applied prune operations with the exact removed paths, and block Node pruning when its approved VM storage is missing or cannot be reconciled.
- Stop distributing Images used only by disabled Pools; non-deleted instances and real overlays continue to protect their exact digest.
- Add a dedicated cache guide and regression coverage for dry runs, active references, unknown storage, service locks, hard-link accounting and resumed transfers.

Automatic LRU eviction remains intentionally disabled: capacity pressure never overrides ownership or backing-image evidence.

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
