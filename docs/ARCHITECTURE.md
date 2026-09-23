# Architecture and operational contract

## Roles

A single Go executable provides CLI, Controller and Agent modes. The Controller does not execute workflow shell commands. It uses the official `actions/scaleset` library to observe demand and create one-job runner configurations. Each enrolled Agent makes an outbound mutually authenticated HTTPS connection and executes a small, typed host protocol: ensure, stop and delete an owned VM.

A one-machine installation runs both roles; adding a second Agent does not require rebuilding the cluster. The initial production target is Ubuntu 24.04 x86_64 with KVM/libvirt. The software-emulation mode is explicitly a diagnostic/CI fallback.

## Selection and capacity

Pool definitions are administrator-authored. A Pool fixes the image digest, CPU, guest memory, memory overhead, root/scratch disk capacities, disk overhead, time limit, maximum runner count and Node selection policy. A workflow selects the corresponding scale-set name. Workflow inputs cannot submit arbitrary VM sizes or host commands.

Each Node declares a local ceiling; Controller budgets cannot exceed it. Eligibility requires permission, selection compatibility, a fresh heartbeat, verified network state and a verified image. CPU, RAM and disk commitments across all Pools share one ledger. A hard reservation holds a complete VM's resource vector on one Node; unused reserved resources cannot be borrowed by general jobs.

SQLite transactions take the writer lock before reading or changing commitments. Node agents additionally account for their own persisted VM manifests. Neither low instantaneous CPU utilization nor a lost heartbeat releases an allocation. Current disk accounting is intentionally conservative: reported free space is reduced by outstanding disk commitments and unconsumed reservations, even if some writes have already materialized. This may underutilize a disk rather than promise unavailable space.

## Runner lifecycle

```text
Reserved → Provisioning → Idle → Busy
                  │          │     │
                  └──────────┴─────┴─→ Stopping → Deleting → Deleted
```

The diagram describes Controller intent. Host manifests separately distinguish preparation, a persisted start intent, running, stopped and deleted. The two are reconciled rather than assumed to be identical.

1. Persist an allocation before external side effects.
2. Generate an official JIT configuration for a unique runner name. Persist it encrypted with AES-GCM before delivery.
3. The Agent records the request, verifies its local budget/network/image, constructs per-job disks and creates owned libvirt domain XML.
4. Persist `start-issued` before invoking libvirt. An ambiguous reply never causes the same VM to be blindly restarted.
5. The VM's root wrapper starts the official Runner as the guest `runner` user. The guest may use sudo; it has no host filesystem mount or host agent credentials.
6. Guest completion triggers poweroff. A guest-side execution deadline also terminates work if management connectivity is lost.
7. GitHub completion records a result but does not release host resources.
8. Host-observed shutdown releases CPU/RAM. Disk commitments remain until owned disks are confirmed removed.
9. Keep a durable tombstone so replayed operations cannot resurrect a deleted VM.

Unknown host state retains resources. New Nodes cannot steal another Node's identity or report deletion of another Node's VM. A known-but-never-provisioned allocation can be cancelled only after both libvirt and the owned disk directory prove it was not created.

## GitHub message handling

The adapter uses the official Go SDK rather than a copied private protocol. Session statistics and runner events are committed before acquisition/acknowledgment. Duplicate session/message IDs are idempotent; the same ID with different contents is rejected.

Demand is bounded by Pool count limits and the shared placement ledger. Stale demand does not create new VMs. If the host observes shutdown before a matching GitHub completion, a barrier requires a fresh server-side demand snapshot before replacement. This avoids converting one stale counter into repeated VM creation.

A Scale Set binding is persisted locally. Existing remote names without a local ownership record require an explicit `github adopt` operation. Creation failures with ambiguous remote outcome are surfaced rather than silently adopting a potentially foreign resource.

## Enrollment and discovery

An invitation contains a Controller HTTPS origin, the CA certificate/fingerprint, a high-entropy one-use secret and an expiry. Only the secret hash is stored in the database. The first request binds the invitation to a CSR, Node name and local ceiling. An administrator reviews the key fingerprint and approves it.

Node certificates use a cluster-specific URI identity. Every request checks the identity, expiry and current revocation state, not only the initial TLS handshake. Certificates renew using the existing Node key before expiry.

Optional Avahi discovery advertises the origin and an **untrusted hint** of the CA fingerprint. The invitation remains the trust anchor. `.local` transport resolution can change with DHCP; TLS still verifies the original hostname and pinned CA. Discovery is link-local and optional; routed LAN/VLAN or cloud scenarios can use explicit reachable HTTPS origins.

## Network and filesystem boundaries

The Agent creates a dedicated libvirt NAT network and dedicated nftables table. It never globally flushes the firewall or rewrites the host's main IP/DNS/DHCP settings. The rules block guest access to private/link-local ranges, host administration, other guests and unsolicited incoming traffic. Managed rules are hashed and verified before VM creation. Existing overlapping routes, foreign network names and unexpected drift fail closed.

A privileged host Agent is an explicit trust boundary in this release. The Controller can run under a dedicated non-root account. Host operations use fixed executables, argv arrays, time limits and owned-resource validation; there is no remote `exec` endpoint.

Golden images are standalone qcow2 files with exact digests. Per-job writable overlays are never promoted into new base images. The image builder verifies Canonical's signed checksums and the official runner release digest, installs the runner without registration, then removes machine/cloud-init/SSH identities. Logs and configuration are not hidden inside reusable images.

Controller and Node caches are content-addressed by the complete SHA-256. Node downloads retain an owner-only partial and resume only when the Controller returns the same digest ETag and a consistent byte range. Publication remains atomic and requires a full-file hash plus standalone-qcow2 inspection.

Cache retirement is a separate local administrative operation. Applied pruning takes the owning process lock, then rescans enabled-Pool demand, durable instances, Node catalog, instance manifests, actual overlay backing paths and cache/base inode relationships. Unknown state disables deletion. A same-host `cache seed` may share one immutable inode across Controller, Node and VM-base paths; removal reports logical paths separately from physical bytes that remain linked elsewhere.

## Configuration and operation

`config plan` is a bounded, expiring declarative plan; `config apply` rejects revision conflicts. Omission retains existing resources. Active Pool resize/image changes and reductions below committed resources are rejected. Node enrollment can add a Node without disrupting the existing GitHub listener. GitHub binding or Pool changes require a controlled Controller restart.

`setup` saves configuration and identities, but does not pretend that network creation, external GitHub authorization or a test VM already succeeded. Each verification has its own command. systemd installation is explicit. Host package installation is documented, not silently performed.

## Agent tasks

A task client is a user's PC, not a Node. It holds a P-256 key generated locally. The administrator signs its CSR out of band with a separate URI kind (`.../client/<name>`), so a client certificate cannot synchronize VMs and a Node certificate cannot use the task API. Every request re-checks the client's certificate hash and revocation.

A task is placed only on a Pool marked `tasks`. Such a Pool is never bound to a GitHub scale set, and GitHub allocation refuses it. Placement uses the same transactional ledger, reservations and candidate reasons as runner allocation, and creates an ordinary Instance with a `task` link. The task payload (prompt, the allowed agent's credential files and environment, an optional GitHub token) is encrypted with the master key. It is delivered only inside an `ensure` command to the owning Node, within a per-reply byte budget, and erased when the host confirms deletion, when queueing expires, or when a task is cancelled before placement. Task state is derived from the Instance rather than kept as a second lifecycle.

The Node builds a task-specific cloud-init seed through the same `ensure` path (ownership, ceiling, image, start intent). The guest runner installs and runs one agent CLI as the unprivileged user. It commits and pushes a work branch, optionally opens a draft pull request, and prints a chunked, SHA-256-checked result to the serial console before powering off. After the host observes shutdown, the Agent parses the bounded log and reports the result. The Controller accepts a result only from the owning Node, and only for an Instance in `Deleting` or `Deleted`. Guest output never changes VM ownership or resource accounting.

The MCP server (`runnerloom mcp serve`) runs on the client PC over stdio, or over loopback-only stateless Streamable HTTP. The user's local allow list decides which agents' credentials may leave the PC; MCP callers cannot change it.

## Recovery and current scope limits

A local process lock prevents two Controllers or Agents from using the same state directory. It is not distributed fencing and does not protect against two copied state directories on different machines. Restore only after isolating the old Controller. Back up the DB, encryption key, CA keys, bindings and credential references together.

The first release intentionally has no GPU sharing/passthrough qualification, controller HA, automatic cross-cloud routing, public fork execution, distributed filesystem, online pool migration or arbitrary remote shell. Cache cleanup and history retention require operator supervision; safe ownership and capacity errors take precedence over speculative deletion. See `docs/VERIFICATION.md` for evidence rather than inferring production certification from this design.
