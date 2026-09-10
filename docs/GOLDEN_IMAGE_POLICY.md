# Ephemeral Golden Image service policy

RunnerLoom's Golden Image is a headless, single-job GitHub Actions runner. The
image builder keeps the official Ubuntu base and the GitHub Actions Runner
runtime, but prevents desktop, removable-device, package-update and unattended
background services from doing work during an ephemeral job.

## Policy

The default boot target is `multi-user.target`. The following units are masked:

- remote access and unused consoles: `ssh.service`, `ssh.socket`,
  `serial-getty@ttyS0.service`;
- snap background work: `snapd.service`, `snapd.socket`,
  `snapd.seeded.service`;
- desktop/removable-device brokers: `ModemManager.service`, `udisks2.service`,
  `polkit.service`;
- crash and unattended package work: `apport.service`,
  `apport-autoreport.service`, `apport-autoreport.timer`,
  `unattended-upgrades.service`, `apt-daily.service`, `apt-daily.timer`,
  `apt-daily-upgrade.service`, `apt-daily-upgrade.timer`;
- installer and firmware refresh work: `lxd-installer.socket`, `fwupd.service`,
  `fwupd-refresh.service`, `fwupd-refresh.timer`.

The builder does **not** purge packages or run `autoremove`. Package removal can
silently break a dependency added by a future official Actions Runner release.
Masking is explicit, reversible in a new image revision and independently
checked after boot.

Networking, certificate validation, Git, Python, the build toolchain, `jq`,
`sudo`, cloud-init and the official Actions Runner are retained. SSH remains
masked and no inbound management path is added.

## Machine-readable evidence

Every image contains `/etc/runnerloom-image-policy.json`. The release manifest
embeds the same policy and its SHA-256 digest. CI boots an overlay of the final
published image and verifies:

1. the policy schema and required mask set;
2. `multi-user.target` is the default;
3. every declared unit is masked and inactive;
4. the official Runner listener executes as the unprivileged `runner` account;
5. image virtual/actual bytes, wall-clock boot/check time and
   `systemd-analyze time` are preserved in `summary.json`.

The `golden-image-evidence-*` artifact contains the image manifest, build log,
policy serial log, image information and qualification summary. Compare the
summary against a baseline built from the previous release before claiming a
boot-time or image-size improvement; correctness gates take priority over a
smaller number.
