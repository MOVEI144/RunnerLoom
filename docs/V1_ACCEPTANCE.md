# RunnerLoom v1 acceptance ledger

This ledger is the authoritative distinction between implemented behavior,
automated evidence, live deployment evidence and work that still requires a
specific physical installation. It tracks issue #4 without turning missing
evidence into a software success.

Status date: **2026-09-10**.

## Current gate status

| Gate | Status | Evidence and remaining boundary |
|---|---|---|
| Real GitHub-assigned one-job runner | Passed for the tested deployment | Live run `34447818805` completed on the RunnerLoom standard pool. Live run `34448089693` completed two concurrent standard jobs followed by a large-pool job, including artifact upload. The exact Organization App, private Runner Group and repository authorization remain deployment-owned secrets and are not copied into the repository. |
| Cancellation and post-job cleanup | Partially accepted | Run `34448420671` was cancelled while an ephemeral runner was active. The reusable `RunnerLoom v1 live acceptance` workflow preserves pre-cancellation runner identity; controller, scale-set and host evidence must still confirm runner removal and owned VM/disk deletion for each deployment. |
| GitHub reconnect, replay, rate limits and capacity changes | Partially accepted | Unit/integration tests cover durable SDK message persistence, replay and stale-demand barriers. The live workflow covers concurrent and differently sized pools. Deliberate session disconnect, API rate-limit and delayed post-job campaigns remain deployment tests. |
| Two physical LAN nodes | Pending external evidence | Requires two actual Nodes, repeated jobs, Node/process interruption, reboot, DHCP address change, enrollment, drain and revocation. A single host or a simulated provider cannot satisfy this gate. |
| Restricted privileged helper | Pending implementation and audit | The Agent remains an explicitly trusted privileged host process. Existing ownership checks, constrained systemd capabilities and fail-closed VM/network operations reduce risk but do not constitute an independently isolated helper. |
| Aggregate retention and compaction | Partially implemented | `runnerloom maintenance compact` previews and, only while the Controller is stopped, removes expired plans, unreferenced expired invitations and old audit rows beyond a retained tail. It checkpoints WAL and vacuums SQLite. Instance ownership records, SDK inbox replay rows, enrollments, images, disks and unknown host state are deliberately retained until a separate retirement fence exists. |
| Physical storage pressure | Pending external evidence | Requires concurrent image imports and full guest writes against the intended physical filesystem, including interruption and recovery. Configuration arithmetic and free-space checks are not a substitute. |
| Backup, restore, fencing, update and renewal | Partially accepted | SQLite snapshot backup, encrypted JIT persistence and automatic Node certificate renewal are implemented and tested. A complete encrypted multi-file backup/restore drill, old-Controller fencing and failed update/rollback campaign remain required. |
| Beginner setup, upgrade and uninstall | Pending external evidence | Must be exercised on clean and pre-existing Ubuntu/libvirt hosts in interactive and JSON modes. The first real GitHub Job and every host change must be included in the record. |
| Guest isolation | Partially accepted | CI boots a real VM and verifies public HTTPS, host-probe rejection, shutdown and owned cleanup. Peer-VM, LAN, VPN/public local routes, link-local/metadata and firewall-reload campaigns remain physical acceptance work. |

## Reusable live workflow

Run **Actions → RunnerLoom v1 live acceptance → Run workflow**.

- `full` starts two standard jobs concurrently, verifies the large pool, performs
  CPU/disk/public-HTTPS work and uploads per-run evidence.
- `cancellation` starts one standard job, uploads the runner identity, then waits.
  Cancel the workflow while it is waiting and correlate the run ID and runner
  name with Controller, GitHub and host cleanup records.

A workflow conclusion of `cancelled` is only the trigger. It is not by itself
proof that GitHub registration, VM, disks and reservations were cleaned up.

## Safe controller compaction

Preview first:

```bash
runnerloom maintenance compact \
  --state /var/lib/runnerloom/controller \
  --older-than 720h \
  --keep-audit 1000 \
  --json
```

Stop the Controller and apply exactly the reviewed policy:

```bash
sudo systemctl stop runnerloom-controller.service
sudo runnerloom maintenance compact \
  --state /var/lib/runnerloom/controller \
  --older-than 720h \
  --keep-audit 1000 \
  --apply \
  --json
sudo systemctl start runnerloom-controller.service
```

The command obtains the same Controller process lock used by the service. It
refuses online compaction and reports both reclaimed candidates and protected
state. It never treats age as proof that a VM, message or enrollment is safe to
forget.

## Issue-closing rule

Issue #4 remains open until every required physical and administrative gate has
reviewable evidence. This document may move a row from pending to passed only
when the exact commit, environment, run/log identifiers, expected failure model
and cleanup result are recorded. A green unit test or a written procedure does
not replace the corresponding live gate.
