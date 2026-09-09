#!/usr/bin/env bash
set -euo pipefail
# This wrapper intentionally runs ONLY on GitHub-owned ephemeral CI hosts.
# The public `runnerloom smoke-vm` command is the explicit local equivalent.
[[ "${GITHUB_ACTIONS:-}" == "true" ]] || { echo 'CI wrapper requires GitHub Actions' >&2; exit 2; }
ROOT="${RUNNER_TEMP:?}/runnerloom-real-vm"
EVIDENCE="${RUNNER_TEMP}/runnerloom-evidence"
STATE="/var/lib/runnerloom-ci-${GITHUB_RUN_ID:?}-${GITHUB_RUN_ATTEMPT:-1}"
DISKS="/var/lib/libvirt/images/runnerloom-ci-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT:-1}"
mkdir -p "$ROOT" "$EVIDENCE"
sudo install -d -m 0700 "$STATE"
finish() {
  status=$?
  sudo virsh -c qemu:///system list --all > "$EVIDENCE/domains-after.txt" 2>&1 || true
  sudo journalctl -u libvirtd --no-pager -n 100 > "$EVIDENCE/libvirt.log" 2>&1 || true
  sudo find "$STATE" -path '*/logs/*.log' -type f -exec cp '{}' "$EVIDENCE/" \; 2>/dev/null || true
  sudo chown -R "$(id -u):$(id -g)" "$EVIDENCE" || true
  exit "$status"
}
trap finish EXIT
BASE='https://cloud-images.ubuntu.com/noble/current'
curl --fail --location --retry 3 "$BASE/SHA256SUMS" -o "$ROOT/SHA256SUMS"
curl --fail --location --retry 3 "$BASE/SHA256SUMS.gpg" -o "$ROOT/SHA256SUMS.gpg"
gpgv --keyring /usr/share/keyrings/ubuntu-cloudimage-keyring.gpg "$ROOT/SHA256SUMS.gpg" "$ROOT/SHA256SUMS"
FILENAME=noble-server-cloudimg-amd64.img
SHA=$(awk -v name="$FILENAME" '$2==name || $2=="*"name {print $1}' "$ROOT/SHA256SUMS")
[[ "$SHA" =~ ^[a-f0-9]{64}$ ]] || { echo 'Missing unambiguous signed image hash' >&2; exit 1; }
curl --fail --location --retry 3 "$BASE/$FILENAME" -o "$ROOT/base.qcow2"
printf '%s  %s\n' "$SHA" "$ROOT/base.qcow2" | sha256sum --check -
printf '%s\n' "$SHA" > "$EVIDENCE/ubuntu-image.sha256"
sudo python3 - "$STATE" "$DISKS" <<'PY'
import json,pathlib,sys
state=pathlib.Path(sys.argv[1]); disks=sys.argv[2]
config={'node':'ci-node','cluster':'ci','controller':'https://127.0.0.1:8443','stateDir':str(state),'diskDir':disks,'networkCIDR':'172.30.240.0/24','ceiling':{'vcpu':4,'memoryMiB':8192,'diskGiB':80},'cacheGiB':5,'qemuUser':'libvirt-qemu'}
p=state/'agent.json';p.write_text(json.dumps(config));p.chmod(0o600)
PY
sudo ./dist/runnerloom network plan --config "$STATE/agent.json" --json > "$EVIDENCE/network-plan.json"
sudo ./dist/runnerloom network apply --config "$STATE/agent.json" --json > "$EVIDENCE/network-apply.json"
MODE=()
if [[ ! -c /dev/kvm ]]; then MODE=(--tcg); fi
sudo ./dist/runnerloom smoke-vm --config "$STATE/agent.json" --image "$ROOT/base.qcow2" --digest "sha256:$SHA" --timeout 15m "${MODE[@]}" --json > "$EVIDENCE/vm-smoke.json"
python3 - "$EVIDENCE/vm-smoke.json" <<'PY'
import json,sys
r=json.load(open(sys.argv[1]))
assert r['ok'] and r['data']['realVM'] and r['data']['deleted'],r
print('Verified actual VM boot, guest execution, poweroff and deletion; emulator='+r['data']['emulator'])
PY
if sudo virsh -c qemu:///system list --all --name | grep -E '^rl-[0-9a-f]{32}$'; then
  echo 'RunnerLoom left a VM registered after the smoke test' >&2
  exit 1
fi
