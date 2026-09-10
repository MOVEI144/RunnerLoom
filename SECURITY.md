# Security Model

RunnerLoom runs code from GitHub inside disposable VMs. A VM is a security boundary, not a guarantee that arbitrary hostile code is harmless. Keep the host kernel, QEMU, libvirt, guest OS, and runner up to date.

## Trust Boundaries

- **Administrator / Controller**: Trusted to define pools, authorize repositories, sign node certificates, and approve nodes. The controller holds the GitHub App key or an explicitly provided credential reference. JIT configurations are encrypted at rest with a separate owner-only master key.
- **Host Agent**: Currently acts as a trusted privileged process, with a typed libvirt/firewall interface. It does not accept arbitrary shell commands or guest-provided XML, and independently enforces its local resource ceiling. (Note: This release does not claim a separately audited unprivileged-agent / privileged-helper split).
- **Runner VM**: Disposable and untrusted relative to the host. Guest `sudo` is permitted; however, host directories, Docker sockets, controller keys, and node identities are not mounted into the guest. The official runner receives only a one-job JIT configuration and the credentials GitHub supplies to that specific job.
- **Enrollment Discovery**: Discovering a name/IP is not authentication. The invitation pins a CA fingerprint, and the administrator approves the requesting key. Each node's certificate is checked on every request, including for revocation.

## Default Policy

- **Repository Access**: Only explicitly allowed **private repositories** are accepted by the GitHub access check. For an organization, the runner group must be set to `selected-repositories` only, must not allow public repositories, and its repository list must match the declared configuration. Labels are not treated as an authorization mechanism.
- **Private but not Harmless**: Private does not mean harmless: contributors or compromised dependencies can still execute code. Do not check out an untrusted PR head inside a privileged/trusted workflow. Do not distribute secrets that the job does not need.
- **Network Isolation**: The managed VM network allows public outbound access and required DNS/DHCP services, while blocking host administration, private/link-local destinations, other VMs, and unsolicited incoming connections. Both IPv4 and IPv6 are considered; IPv6 guest traffic is disabled/blocked. Existing host addressing, DNS, and router settings are not rewritten. Unexpected firewall drift prevents new VM creation.
- **Data Exfiltration**: Outbound Internet access necessarily permits data exfiltration from a compromised job. The firewall is not a secrets-loss prevention product. Avoid attaching NAS shares or exposing internal production services to runner networks without a separately reviewed policy.

## Storage and Cleanup

- **Verified Images**: Mutable VM disks are per-job. Backing images are SHA-256 verified and read-only. Image construction verifies Canonical signatures and the official runner archive digest. Images must be trusted; arbitrary user-supplied qcow2 files are not a safe file-upload format for a privileged service.
- **Resource Management**: VM identifiers, metadata, paths, and ownership are checked before destructive operations. Resource commitments survive unknown states and communication failures. A GitHub completion event does not prove that a host VM stopped. CPU/RAM are released only after host stop confirmation; disk capacity remains held until deletion confirmation.
- **Data Retention**: The current image cache deliberately does not auto-evict backing images that may still be in use. Plan adequate space for images, logs, VM disks, and SQLite history. A cache-capacity error is not automatically repaired by deleting user data.
- **Diagnostic Logs**: Serial logs are bounded per VM, owner-only, and may contain sensitive job output. Do not publish them blindly. Deleted files on ordinary storage are not a cryptographic secure erase; use encrypted storage when residual-data risks matter.

## Persistence and Recovery

Keep consistent database backups **and** the master key, CA certificate/key, scale-set bindings, and referenced GitHub credentials.

- A database snapshot without the master key cannot recover encrypted JIT configurations.
- Restore only after isolating the old controller; automatic HA is not implemented.
- **Do not** run two controllers against copied state directories. The process lease protects a shared local directory, not two independent machines with copied identities.
- **Do not** copy node state to another server.

## Reporting Vulnerabilities

Please report sensitive vulnerabilities privately through the repository's security reporting feature when available, or directly to the repository owner.

**Do not** place tokens, private keys, invitation secrets, unredacted JIT configurations, or live exploit credentials in public issues.

General hardening suggestions and non-sensitive reproducible bugs can be filed as regular issues.
