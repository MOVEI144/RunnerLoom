# Security model

RunnerLoom runs code from GitHub inside disposable VMs. A VM is a security boundary, not a guarantee that arbitrary hostile code is harmless. Keep the host kernel, QEMU, libvirt, guest OS and runner current.

## Trust boundaries

- **Administrator / controller:** trusted to define pools, authorize repositories, sign node certificates and approve nodes. The controller holds the GitHub App key or an explicitly provided credential reference. JIT configurations are encrypted at rest with a separate owner-only master key.
- **Host agent:** currently a trusted privileged process, with a typed libvirt/firewall interface. It does not accept arbitrary shell commands or guest-provided XML. It independently enforces its local resource ceiling. This release does not claim a separately audited unprivileged-agent / privileged-helper split.
- **Runner VM:** disposable and untrusted relative to the host. Guest sudo is permitted; host directories, Docker sockets, controller keys and node identities are not mounted into the guest. The official runner receives only a one-job JIT configuration and the credentials GitHub supplies to that job.
- **Enrollment discovery:** discovering a name/IP is not authentication. The invitation pins a CA fingerprint; the administrator approves the requesting key. Each node's certificate is checked on every request, including revocation.

## Default policy

Only explicitly allowed **private repositories** are accepted by the GitHub access check. For an organization, the runner group must be selected-repositories-only, must not allow public repositories, and its repository list must match the declared configuration. A label is not an authorization mechanism.

Private does not mean harmless: contributors or compromised dependencies can still execute code. Do not check out an untrusted PR head inside a privileged/trusted workflow. Do not distribute secrets that the job does not need.

The managed VM network allows public outbound access and the required DNS/DHCP services, while blocking host administration, private/link-local destinations, other VMs and unsolicited incoming connections. Both IPv4 and IPv6 are considered; IPv6 guest traffic is disabled/blocked. Existing host addressing, DNS and router settings are not rewritten. Unexpected firewall drift prevents new VM creation.

Outbound Internet access necessarily permits data exfiltration from a compromised job. The firewall is not a secrets-loss prevention product. Avoid attaching NAS shares or exposing internal production services to runner networks without a separate reviewed policy.

## Agent tasks

An approved task client can run arbitrary commands inside a disposable guest, as an allowed private-repository workflow can. It never receives a host command, and its VM is subject to the same network isolation, ceilings and deletion proof as runner VMs.

Starting a task deliberately copies the chosen agent's local login (files and environment variables named by its profile) and, if allowed, a GitHub token into that guest. Assume that code in the guest, including the agent itself and anything it downloads, can read and send them over the outbound Internet. Only agents listed by the user with `runnerloom client allow` are sent; the MCP interface cannot change that list. A token refresh inside the guest may sign the PC out of that agent. Prefer long-lived tokens or API keys scoped for this use, and use a fine-grained GitHub token limited to the target repository with protected default branches.

Task payloads are encrypted at rest with the master key and erased after confirmed deletion, queue expiry or cancellation before placement. Results and progress come from an untrusted guest. They are bounded, stored encrypted and shown only to the submitting client. The HTTP MCP transport listens on loopback only and requires a secret. Anyone with the tunnel URL that embeds that secret can start tasks with this PC's allowed credentials.

## Storage and cleanup

Mutable VM disks are per-job. Backing images are SHA-256 verified and read-only. Image construction verifies Canonical signatures and the official runner archive digest. Images must be trusted; arbitrary user-supplied qcow2 files are not a safe file-upload format for a privileged service.

VM identifiers, metadata, paths and ownership are checked before destructive operations. Resource commitments survive unknown state and communication failure. A GitHub completion event does not prove that a host VM stopped. CPU/RAM are released only after host stop confirmation; disk capacity remains held until deletion confirmation.

The image cache deliberately does not auto-evict backing images that may still be in use. `cache prune --apply` requires the owning service to be stopped and fails closed unless catalog, instance manifests, actual qcow2 overlay backing paths and base hard links can all be reconciled. Unknown files or malformed ownership evidence block deletion. Plan space for images, logs, VM disks and SQLite history; a cache-capacity error is not automatically repaired by deleting user data.

Interrupted Node downloads use owner-only partial files and resume only against the same digest ETag and a consistent HTTP byte range. The complete file is SHA-256 and qcow2-structure checked before atomic publication. A completed entry with a mismatched digest is quarantined only when it has no other hard links; shared corruption requires stopped-service reconciliation so an active backing inode is never silently replaced. VM creation rechecks that the base path and verified Node cache are the same inode. `cache seed` is an explicit local-admin operation: it verifies the source and uses a hard link only on the same filesystem, never an implicit cross-filesystem copy.

Diagnostic serial logs are bounded per VM, owner-only and may contain sensitive job output. Do not publish them blindly. Deleted files on ordinary storage are not a cryptographic secure erase; use encrypted storage when residual-data risks matter.

## Persistence and recovery

Keep consistent database backups **and** the master key, CA certificate/key, scale-set bindings and referenced GitHub credentials. A database snapshot without the master key cannot recover encrypted JIT configurations. Restore only after isolating the old controller; automatic HA is not implemented.

Do not run two controllers against copied state directories. The process lease protects a shared local directory, not two independent machines with copied identities. Do not copy node state to another server.

## Reporting

Report sensitive vulnerabilities privately through the repository's security reporting feature when available, or directly to the repository owner. Do not place tokens, private keys, invitation secrets, unredacted JIT configurations or live exploit credentials in public issues. General hardening suggestions and non-sensitive reproducible bugs can be filed as issues.
