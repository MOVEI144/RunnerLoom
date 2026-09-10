#!/usr/bin/env bash
set -euo pipefail
umask 077

IMAGE=${1:?absolute Golden Image path}
EVIDENCE=${2:?absolute evidence directory}
[[ "$IMAGE" == /* && "$EVIDENCE" == /* ]] || { echo 'image and evidence paths must be absolute' >&2; exit 2; }
[[ -f "$IMAGE" && ! -L "$IMAGE" ]] || { echo 'Golden Image must be a regular non-symlink file' >&2; exit 2; }
for tool in qemu-img qemu-system-x86_64 cloud-localds timeout python3; do
  command -v "$tool" >/dev/null || { echo "missing policy qualification dependency: $tool" >&2; exit 2; }
done

mkdir -p "$EVIDENCE"
chmod 0700 "$EVIDENCE"
WORK=$(mktemp -d "${EVIDENCE%/}/.policy-check-XXXXXXXX")
cleanup() { rm -rf -- "$WORK"; }
trap cleanup EXIT

qemu-img info --output=json --force-share "$IMAGE" > "$EVIDENCE/image-info.json"
qemu-img create -f qcow2 -F qcow2 -b "$IMAGE" "$WORK/root.qcow2" >/dev/null

python3 - "$WORK" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1])
script=r'''#!/bin/bash
set -euo pipefail
exec >/dev/ttyS0 2>&1
trap 'rc=$?; echo RUNNERLOOM_POLICY_EXIT=$rc; sync; sleep 2; systemctl poweroff --no-block' EXIT
python3 - <<'VERIFY'
import json,pathlib,subprocess
path=pathlib.Path('/etc/runnerloom-image-policy.json')
policy=json.loads(path.read_text())
required={
    'ssh.service','ssh.socket','serial-getty@ttyS0.service',
    'snapd.service','snapd.socket','snapd.seeded.service',
    'ModemManager.service','udisks2.service','polkit.service',
    'apport.service','apport-autoreport.service','apport-autoreport.timer',
    'unattended-upgrades.service','apt-daily.service','apt-daily.timer',
    'apt-daily-upgrade.service','apt-daily-upgrade.timer',
    'lxd-installer.socket','fwupd.service','fwupd-refresh.service','fwupd-refresh.timer',
}
required_capabilities={
    'network','ca-certificates','git','python3','build-essential','jq','sudo','actions-runner',
}
if policy.get('schemaVersion') != 'runnerloom/image-policy/v1':
    raise SystemExit('unexpected image policy schema')
if policy.get('defaultTarget') != 'multi-user.target':
    raise SystemExit('unexpected image policy default target')
masked=set(policy.get('maskedUnits',[]))
missing=sorted(required-masked)
if missing:
    raise SystemExit(f'required masks missing from policy: {missing}')
capabilities=set(policy.get('retainedCapabilities',[]))
if capabilities != required_capabilities:
    raise SystemExit(f'unexpected retained capabilities: {sorted(capabilities)}')
actual=subprocess.run(['systemctl','get-default'],check=True,text=True,capture_output=True).stdout.strip()
if actual != 'multi-user.target':
    raise SystemExit(f'unexpected default target: {actual}')
for unit in sorted(masked):
    enabled=subprocess.run(['systemctl','is-enabled',unit],text=True,capture_output=True)
    state=(enabled.stdout or enabled.stderr).strip().splitlines()
    state=state[-1] if state else ''
    if state != 'masked':
        raise SystemExit(f'{unit} is not masked: {state!r}')
    active=subprocess.run(['systemctl','is-active',unit],text=True,capture_output=True).stdout.strip()
    if active == 'active':
        raise SystemExit(f'{unit} unexpectedly active')
print('RUNNERLOOM_IMAGE_POLICY_JSON='+json.dumps(policy,sort_keys=True,separators=(',',':')))
print('RUNNERLOOM_IMAGE_POLICY_OK')
VERIFY

test -s /etc/ssl/certs/ca-certificates.crt
command -v git >/dev/null
git --version
command -v python3 >/dev/null
python3 --version
command -v gcc >/dev/null
command -v g++ >/dev/null
command -v make >/dev/null
dpkg-query -W -f='${Status}\n' build-essential | grep -qx 'install ok installed'
command -v jq >/dev/null
jq --version
command -v sudo >/dev/null
sudo -n -u runner true
python3 - <<'NETWORK'
import ssl,urllib.request
with urllib.request.urlopen('https://github.com/robots.txt',timeout=30,context=ssl.create_default_context()) as response:
    if response.status != 200:
        raise SystemExit(f'unexpected HTTPS status: {response.status}')
NETWORK

test -x /opt/actions-runner/bin/Runner.Listener
runuser -u runner -- /opt/actions-runner/bin/Runner.Listener --version
echo RUNNERLOOM_RETAINED_CAPABILITIES_OK
systemd_time=$(systemd-analyze time --no-pager 2>&1 || true)
printf 'RUNNERLOOM_SYSTEMD_TIME=%s\n' "${systemd_time//$'\n'/ }"
systemd-analyze blame --no-pager 2>&1 | head -n 40 || true
systemctl --failed --no-legend --no-pager || true
echo RUNNERLOOM_GOLDEN_POLICY_QUALIFIED
'''
config={
    'ssh_pwauth':False,
    'disable_root':True,
    'write_files':[{'path':'/usr/local/sbin/runnerloom-policy-check','permissions':'0700','content':script}],
    'runcmd':[['bash','-c','exec /usr/local/sbin/runnerloom-policy-check']],
}
(p/'user-data').write_text('#cloud-config\n'+json.dumps(config))
(p/'meta-data').write_text('instance-id: runnerloom-policy-check\nlocal-hostname: runnerloom-policy-check\n')
PY
cloud-localds "$WORK/seed.iso" "$WORK/user-data" "$WORK/meta-data"

start=$(date +%s)
set +e
timeout --signal=TERM --kill-after=30s 10m qemu-system-x86_64 \
  -machine q35,accel=kvm:tcg \
  -cpu "$(if [[ -r /dev/kvm && -w /dev/kvm ]]; then printf host; else printf max; fi)" \
  -smp 2 -m 2048 -display none -monitor none -no-reboot \
  -serial "file:$EVIDENCE/serial.log" \
  -drive "file=$WORK/root.qcow2,if=virtio,format=qcow2,cache=none" \
  -drive "file=$WORK/seed.iso,if=virtio,format=raw,readonly=on" \
  -netdev user,id=policy-net -device virtio-net-pci,netdev=policy-net \
  -device virtio-rng-pci
status=$?
set -e
end=$(date +%s)

if [[ "$status" != 0 ]] || ! grep -q '^RUNNERLOOM_IMAGE_POLICY_OK' "$EVIDENCE/serial.log" || ! grep -q '^RUNNERLOOM_RETAINED_CAPABILITIES_OK' "$EVIDENCE/serial.log" || ! grep -q '^RUNNERLOOM_GOLDEN_POLICY_QUALIFIED' "$EVIDENCE/serial.log" || ! grep -q '^RUNNERLOOM_POLICY_EXIT=0' "$EVIDENCE/serial.log"; then
  tail -n 200 "$EVIDENCE/serial.log" >&2 || true
  echo 'Golden Image policy qualification failed' >&2
  exit 1
fi

python3 - "$EVIDENCE" "$((end-start))" <<'PY'
import json,pathlib,re,sys
p=pathlib.Path(sys.argv[1]);serial=(p/'serial.log').read_text(errors='replace')
image=json.loads((p/'image-info.json').read_text())
policy_match=re.search(r'^RUNNERLOOM_IMAGE_POLICY_JSON=(.+)$',serial,re.M)
time_match=re.search(r'^RUNNERLOOM_SYSTEMD_TIME=(.+)$',serial,re.M)
if not policy_match:
    raise SystemExit('policy evidence missing')
summary={
    'schemaVersion':'runnerloom/golden-image-evidence/v1',
    'qualified':True,
    'wallClockBootAndCheckSeconds':int(sys.argv[2]),
    'systemdAnalyzeTime':time_match.group(1).strip() if time_match else None,
    'imageActualBytes':image.get('actual-size'),
    'imageVirtualBytes':image.get('virtual-size'),
    'policy':json.loads(policy_match.group(1)),
}
(p/'summary.json').write_text(json.dumps(summary,indent=2,sort_keys=True)+'\n')
print(json.dumps(summary,sort_keys=True))
PY
