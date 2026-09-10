# Architecture and Operational Contract

## 1. Roles and Overview

A single Go executable provides the CLI, Controller, and Agent modes.

- **Controller**: Does not execute workflow shell commands. It uses the official `actions/scaleset` library to observe demand and create one-job runner configurations.
- **Agent**: Enrolls with the Controller, makes an outbound mutually authenticated HTTPS connection (mTLS), and executes a typed host protocol: *ensure, stop, and delete an owned VM*.

A one-machine installation runs both roles. Adding a second Agent does not require rebuilding the cluster. The initial production target is **Ubuntu 24.04 x86_64 with KVM/libvirt**. Software-emulation mode is strictly a diagnostic/CI fallback.

## 2. Selection and Capacity

Pool definitions are administrator-authored.

A **Pool** fixes the following variables:
- Image digest
- CPU, guest memory, and memory overhead
- Root/scratch disk capacities and disk overhead
- Time limits and maximum runner count
- Node selection policy

Workflows select the corresponding scale-set name. Workflow inputs cannot submit arbitrary VM sizes or host commands.

### Resource Accounting
Each Node declares a local ceiling; Controller budgets cannot exceed it. Eligibility requires permission, selection compatibility, a fresh heartbeat, verified network state, and a verified image.

- CPU, RAM, and disk commitments across all Pools share one ledger.
- A hard reservation holds a complete VM's resource vector on one Node; unused reserved resources cannot be borrowed by general jobs.
- SQLite transactions take the writer lock before reading or changing commitments.
- Node agents additionally account for their own persisted VM manifests.
- Current disk accounting is intentionally conservative: reported free space is reduced by outstanding disk commitments and unconsumed reservations, even if some writes have already materialized.

## 3. Runner Lifecycle

```text
Reserved → Provisioning → Idle → Busy
                  │          │     │
                  └──────────┴─────┴─→ Stopping → Deleting → Deleted
```

The diagram above describes Controller intent. Host manifests separately distinguish preparation, a persisted start intent, running, stopped, and deleted. The two are reconciled rather than assumed identical.

1. **Persist Allocation**: Persist an allocation before executing external side effects.
2. **JIT Configuration**: Generate an official JIT configuration for a unique runner name. Persist it securely (encrypted with AES-GCM) before delivery.
3. **Agent Verification**: The Agent records the request, verifies its local budget/network/image, constructs per-job disks, and creates owned libvirt domain XML.
4. **Issue Start**: Persist `start-issued` before invoking libvirt. An ambiguous reply never causes the same VM to be blindly restarted.
5. **Execution**: The VM's root wrapper starts the official Runner as the guest `runner` user. The guest may use sudo, but has no host filesystem mount or host agent credentials.
6. **Completion**: Guest completion triggers poweroff. A guest-side execution deadline also terminates work if management connectivity is lost.
7. **GitHub Acknowledgment**: GitHub completion records a result but does not immediately release host resources.
8. **Resource Release**: Host-observed shutdown releases CPU/RAM. Disk commitments remain until owned disks are confirmed removed.
9. **Tombstone**: Keep a durable tombstone so replayed operations cannot resurrect a deleted VM.

## 4. GitHub Message Handling

The adapter uses the official Go SDK rather than a copied private protocol. Session statistics and runner events are committed before acquisition/acknowledgment. Duplicate session/message IDs are idempotent; the same ID with different contents is rejected.

Demand is bounded by Pool count limits and the shared placement ledger. Stale demand does not create new VMs. If the host observes shutdown before a matching GitHub completion, a barrier requires a fresh server-side demand snapshot before replacement.

## 5. Enrollment and Discovery

An invitation contains a Controller HTTPS origin, the CA certificate/fingerprint, a high-entropy one-use secret, and an expiry.

- Only the secret hash is stored in the database.
- The first request binds the invitation to a CSR, Node name, and local ceiling.
- An administrator reviews the key fingerprint and approves it.
- Node certificates use a cluster-specific URI identity. Every request checks the identity, expiry, and current revocation state.
- Certificates renew using the existing Node key before expiry.

Optional Avahi discovery advertises the origin and an **untrusted hint** of the CA fingerprint. The invitation remains the trust anchor.

## 6. Network and Filesystem Boundaries

The Agent creates a dedicated libvirt NAT network and a dedicated nftables table. It **never** globally flushes the firewall or rewrites the host's main IP/DNS/DHCP settings.

The rules strictly block guest access to:
- Private/link-local ranges
- Host administration
- Other guests
- Unsolicited incoming traffic

A privileged host Agent is an explicit trust boundary in this release. Host operations use fixed executables, argv arrays, time limits, and owned-resource validation; there is no remote `exec` endpoint.

Golden images are standalone qcow2 files with exact digests. Per-job writable overlays are never promoted into new base images. Logs and configuration are not hidden inside reusable images.

## 7. Configuration and Operation

- `config plan` creates a bounded, expiring declarative plan.
- `config apply` applies the plan but rejects revision conflicts.
- Node enrollment can add a Node without disrupting the existing GitHub listener.
- GitHub binding or Pool changes require a controlled Controller restart.

## 8. Recovery and Current Scope Limits

A local process lock prevents two Controllers or Agents from using the same state directory. It is not distributed fencing and does not protect against two copied state directories on different machines.

**Current Limitations (V1):**
The first release intentionally has no GPU sharing/passthrough qualification, controller HA, automatic cross-cloud routing, public fork execution, distributed filesystem, online pool migration, or arbitrary remote shell. Cache cleanup and history retention require operator supervision.
