# Install, verify, upgrade and remove

## Supported package

RunnerLoom 0.1.0-rc.1 targets Ubuntu 24.04, Linux/amd64 and CPU-only KVM guests.
A release candidate is usable software awaiting deployment acceptance, not a claim that every part of the broader design is production-qualified.
The archive/deb installs the CLI. Virtualization tools, GitHub authorization and an explicit setup are separate.
Do not run a downloaded installation script directly from a pipe.

## Verify before installing

Download the archive (or Debian package) and `SHA256SUMS` from the **same release**.
Run this in the directory containing those files:

```bash
# When only selected assets were downloaded, --ignore-missing ignores other listed assets.
sha256sum --check --ignore-missing SHA256SUMS
```

Require an `OK` result for the asset you are about to use. Checksums detect corruption and mismatched assets;
they do not protect against compromise of the release publisher itself. `BUILDINFO.json` records the exact
source commit, compiler, target and dependency versions. Third-party license notices are included.

Archive installation (replace the version only with the release actually downloaded):

```bash
tar -xzf runnerloom-0.1.0-rc.1-linux-amd64.tar.gz
cd runnerloom-0.1.0-rc.1-linux-amd64
./runnerloom version --json
sudo install -m 0755 runnerloom /usr/local/bin/runnerloom
```

Alternatively, `sudo apt install ./runnerloom_0.1.0~rc.1_amd64.deb` installs `/usr/bin/runnerloom`.
Use one installation method, not both: `/usr/local/bin` normally takes precedence over `/usr/bin`.
The Debian package has no maintainer scripts and **does not enable a service or change a firewall**.
Its virtualization dependencies are suggestions, not automatically enabled daemons.

Follow [the Japanese setup guide](QUICKSTART.ja.md) for the host packages, image creation, GitHub App,
Pool configuration, VM-network plan and explicit service installation.
`runnerloom --help --json` is a machine-readable command reference; `config schema` emits JSON Schema.

## Before an upgrade

1. Stop new assignments with `node drain` and wait for held resources to reach zero.
2. Stop the Controller and Agent services. Verify no management process remains.
3. Run `backup --out /absolute/private/path/controller.db` and separately preserve the Controller's
   master key, CA keys/certificate, bindings and dedicated GitHub credential references. Preserve each
   node's identity and configuration too. A DB-only backup is **not** a complete recovery backup.
4. Verify and install the new package, read its migration notes, then start the Controller and agents.
5. Check `status`, `doctor --strict`, `pool explain`, and a disposable test job before resuming nodes.

Do not restore a DB snapshot alongside live agents without reconciling their actual VMs. Do not run old
and restored Controllers simultaneously. Downgrade is allowed only if the target version explicitly supports
the current schema; otherwise restore a consistent offline backup after fencing the old Controller.

## Remove without deleting data

Drain and finish jobs, stop/disable only the generated RunnerLoom services, then remove the package or
manually installed binary. Preserve state and image directories until you have verified all VM/domain
cleanup and backed up data. Package removal intentionally does not recursively erase these directories.
Do not remove libvirt's shared `default` network or other applications' firewall rules.

## Deployment acceptance

Before connecting valuable repository secrets, verify on your own host: CPU/RAM/disk limits, dedicated
reservations, LAN and inter-VM isolation, reboot recovery and a real job through your authorized GitHub App.
GitHub-hosted CI evidence does not establish isolation on an arbitrary host with existing VPN/firewall rules.
GPU, hostile public PRs, Controller HA and a privilege-separated host helper are not certified by this release.
