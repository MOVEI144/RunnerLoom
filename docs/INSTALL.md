# Installation & Upgrades

This guide covers installing, verifying, upgrading, and removing RunnerLoom.

## Supported Environment

RunnerLoom targets **Ubuntu 24.04**, Linux/amd64, and CPU-only KVM guests.

> **Note**: As a Release Candidate, it is highly functional but may not have complete production qualification across all potential host environments. The `.tar.gz` and `.deb` packages install only the CLI. Virtualization tools, GitHub authorization, and configuration are performed in separate, explicit steps.

## Verify Before Installing

Always verify the downloaded archive (or Debian package) and `SHA256SUMS` from the **same release**.

Run the following command in the directory containing the downloaded files:

```bash
# --ignore-missing allows the check to pass if you only downloaded specific assets
sha256sum --check --ignore-missing SHA256SUMS
```

Ensure you receive an `OK` result for the asset you plan to use. Checksums detect file corruption and mismatched assets. `BUILDINFO.json` records the exact source commit, compiler, target, and dependency versions.

## Installation

### Method 1: Using the `.tar.gz` Archive (Recommended)

*Replace the version number below with the actual release you downloaded.*

```bash
tar -xzf runnerloom-0.1.0-rc.1-linux-amd64.tar.gz
cd runnerloom-0.1.0-rc.1-linux-amd64
./runnerloom version --json
sudo install -m 0755 runnerloom /usr/local/bin/runnerloom
```

### Method 2: Using the `.deb` Package

```bash
sudo apt install ./runnerloom_0.1.0~rc.1_amd64.deb
```

> **Important**: Use only one installation method. (`/usr/local/bin` normally takes precedence over `/usr/bin`). The Debian package **does not** automatically enable a service or change your firewall rules.

### Next Steps After Installation

Follow the [Japanese Quickstart Guide (日本語セットアップガイド)](QUICKSTART.ja.md) for step-by-step instructions on host packages, image creation, GitHub App setup, pool configuration, network planning, and service installation.

- Use `runnerloom --help --json` for a machine-readable command reference.
- Use `runnerloom config schema` to generate a JSON Schema.

## Upgrading

Follow these steps to safely upgrade an existing RunnerLoom cluster:

1. **Drain Nodes**: Stop new assignments using `runnerloom node drain` and wait for all held resources to reach zero.
2. **Stop Services**: Stop both the Controller and Agent services. Ensure no management processes remain running.
3. **Backup Everything**:
   - Run `runnerloom backup --out /absolute/private/path/controller.db`.
   - **Crucial**: Separately back up the Controller's master key, CA keys/certificates, bindings, and GitHub credential references.
   - Preserve each node's identity and configuration.
   - *Note: A DB-only backup is NOT a complete recovery backup.*
4. **Install Update**: Verify and install the new package (reading any migration notes first).
5. **Restart Services**: Start the Controller and agents.
6. **Verify**: Check `runnerloom status`, `runnerloom doctor --strict`, `runnerloom pool explain`, and run a test job before resuming nodes.

> **Warning**: Do not restore a DB snapshot alongside live agents without reconciling their actual VMs. Do not run old and restored Controllers simultaneously.

## Removing RunnerLoom

To cleanly remove RunnerLoom without deleting your data:

1. Drain and wait for all active jobs to finish.
2. Stop and disable the generated RunnerLoom systemd services.
3. Remove the `.deb` package or the binary in `/usr/local/bin`.

> **Note**: Removing the package intentionally **does not** erase your state or image directories. Please preserve these directories until you have verified all VM/domain cleanup and backed up any necessary data. Do not remove libvirt's shared `default` network or other applications' firewall rules.

## Deployment Acceptance

Before connecting valuable repositories, verify the following on your host:

- CPU/RAM/disk limits are enforced.
- Dedicated reservations function correctly.
- LAN and inter-VM isolation are working.
- The system recovers gracefully after a reboot.
- A real job runs successfully using your authorized GitHub App.

**Requirements for Production**:
- Explicit Organization authorization (via App/PAT and restricted Runner Groups).
- Successful Golden Image build and boot qualification with a verified, unregistered official Runner.

*This checklist must be passed; missing credentials or skipping tests do not satisfy these acceptance gates.*
