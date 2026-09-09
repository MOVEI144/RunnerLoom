#!/usr/bin/env bash
set -euo pipefail
umask 077
OUT=${1:?output path}
VERSION=${2:-latest}
[[ "$OUT" == /* && "$OUT" == *.qcow2 ]] || { echo 'Output must be an absolute .qcow2 path' >&2; exit 2; }
[[ ! -e "$OUT" && ! -L "$OUT" && ! -e "$OUT.manifest.json" ]] || { echo 'Refusing to replace an existing image or manifest' >&2; exit 2; }
[[ "$VERSION" == latest || "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo 'Invalid runner version' >&2; exit 2; }
for tool in curl gpgv python3 qemu-img qemu-system-x86_64 cloud-localds timeout virt-customize; do command -v "$tool" >/dev/null || { echo "Missing image-builder dependency: $tool" >&2; exit 2; }; done
KEYRING=/usr/share/keyrings/ubuntu-cloudimage-keyring.gpg
[[ -r "$KEYRING" ]] || { echo 'Install ubuntu-keyring to verify Canonical cloud images' >&2; exit 2; }
WORK=$(mktemp -d "$(dirname "$OUT")/.runnerloom-build-XXXXXXXX")
TAIL_PID=
cleanup() {
  status=$?
  if [[ -n "$TAIL_PID" ]]; then kill "$TAIL_PID" 2>/dev/null || true; wait "$TAIL_PID" 2>/dev/null || true; fi
  rm -rf -- "$WORK"
  exit "$status"
}
trap cleanup EXIT
BASE=https://cloud-images.ubuntu.com/noble/current
FETCH=(curl --fail --silent --show-error --location --retry 3 --connect-timeout 20 --max-time 900 --proto '=https' --tlsv1.2)
echo 'Verifying Canonical cloud image signature...' >&2
"${FETCH[@]}" "$BASE/SHA256SUMS" -o "$WORK/SHA256SUMS"
"${FETCH[@]}" "$BASE/SHA256SUMS.gpg" -o "$WORK/SHA256SUMS.gpg"
gpgv --keyring "$KEYRING" "$WORK/SHA256SUMS.gpg" "$WORK/SHA256SUMS" >&2
python3 - "$WORK" <<'PY'
import pathlib,re,sys
p=pathlib.Path(sys.argv[1]);matches=[]
for line in (p/'SHA256SUMS').read_text().splitlines():
    parts=line.split()
    if len(parts)==2 and parts[1].lstrip('*')=='noble-server-cloudimg-amd64.img': matches.append(parts[0])
if len(matches)!=1 or not re.fullmatch('[a-f0-9]{64}',matches[0]): raise SystemExit('Signed image checksum was missing or ambiguous')
(p/'base.sha256').write_text(matches[0])
PY
"${FETCH[@]}" "$BASE/noble-server-cloudimg-amd64.img" -o "$WORK/base.qcow2"
printf '%s  %s\n' "$(cat "$WORK/base.sha256")" "$WORK/base.qcow2" | sha256sum --check - >&2
RELEASE=https://api.github.com/repos/actions/runner/releases/latest
if [[ "$VERSION" != latest ]]; then RELEASE="https://api.github.com/repos/actions/runner/releases/tags/v$VERSION"; fi
"${FETCH[@]}" --header 'Accept: application/vnd.github+json' "$RELEASE" -o "$WORK/release.json"
python3 - "$WORK" <<'PY'
import json,pathlib,re,sys,urllib.parse
p=pathlib.Path(sys.argv[1]);r=json.loads((p/'release.json').read_text());tag=r.get('tag_name','')
if not re.fullmatch(r'v\d+\.\d+\.\d+',tag) or r.get('draft') or r.get('prerelease'): raise SystemExit('A stable runner release is required')
name=f'actions-runner-linux-x64-{tag[1:]}.tar.gz';matches=[a for a in r.get('assets',[]) if a.get('name')==name]
if len(matches)!=1: raise SystemExit('Runner asset missing or ambiguous')
a=matches[0];digest=a.get('digest','');url=a.get('browser_download_url','');u=urllib.parse.urlparse(url)
if not re.fullmatch(r'sha256:[a-f0-9]{64}',digest): raise SystemExit('GitHub release has no verified asset digest; refusing unverified download')
if u.scheme!='https' or u.netloc!='github.com' or not u.path.startswith('/actions/runner/releases/download/'): raise SystemExit('Unexpected runner asset origin')
(p/'runner.url').write_text(url);(p/'runner.sha256').write_text(digest[7:]);(p/'runner.version').write_text(tag[1:])
PY
echo 'Downloading and verifying official GitHub runner...' >&2
"${FETCH[@]}" "$(cat "$WORK/runner.url")" -o "$WORK/runner.tar.gz"
printf '%s  %s\n' "$(cat "$WORK/runner.sha256")" "$WORK/runner.tar.gz" | sha256sum --check - >&2
# Preserve GPT partition identities and GRUB's on-disk references. Rebuilding
# the partition table can make a perfectly valid cloud image unbootable.
qemu-img convert -f qcow2 -O qcow2 "$WORK/base.qcow2" "$WORK/golden.qcow2" >&2
qemu-img resize -f qcow2 "$WORK/golden.qcow2" 20G >&2
export LIBGUESTFS_BACKEND=direct
# Package installation happens in a real, disposable build VM. libguestfs is
# used only offline, avoiding distribution-specific passt privilege/namespace
# failures without disabling AppArmor or modifying host networking.
python3 - "$WORK" <<'PYSEED'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1]);url=(p/'runner.url').read_text();digest=(p/'runner.sha256').read_text()
script=r"""#!/bin/bash
set -euo pipefail
trap 'rc=$?; echo RUNNERLOOM_BUILD_EXIT=$rc; sync; sleep 2; systemctl poweroff --no-block' EXIT
export DEBIAN_FRONTEND=noninteractive
printf 'Acquire::Retries "3"; Acquire::http::Timeout "30"; Acquire::https::Timeout "30"; DPkg::Lock::Timeout "120";\n' >/etc/apt/apt.conf.d/90runnerloom-builder
apt-get update -qq
apt-get install -y --no-install-recommends ca-certificates curl git python3 python3-venv build-essential jq sudo
getent passwd runner >/dev/null || useradd --create-home --shell /bin/bash runner
install -d -m 0755 /opt/actions-runner
curl --fail --location --retry 3 --connect-timeout 20 --max-time 900 --proto '=https' --tlsv1.2 '__URL__' -o /tmp/runner.tar.gz
printf '%s  %s\n' '__DIGEST__' /tmp/runner.tar.gz | sha256sum --check -
tar -xzf /tmp/runner.tar.gz -C /opt/actions-runner
cd /opt/actions-runner
./bin/installdependencies.sh
chown -R runner:runner /opt/actions-runner /home/runner
runuser -u runner -- ./bin/Runner.Listener --version | tee /etc/runnerloom-runner-version
rm -f /tmp/runner.tar.gz
rm -f /opt/actions-runner/.runner /opt/actions-runner/.credentials /opt/actions-runner/.credentials_rsaparams
apt-get clean
rm -rf /var/lib/apt/lists/*
echo RUNNERLOOM_GOLDEN_BUILD_COMPLETE
""".replace('__URL__',url).replace('__DIGEST__',digest)
config={'growpart':{'mode':'auto','devices':['/'],'ignore_growroot_disabled':False},'resize_rootfs':True,'ssh_pwauth':False,'disable_root':True,'bootcmd':[['systemctl','mask','--now','serial-getty@ttyS0.service'],['systemctl','mask','--now','ssh.service','ssh.socket']],'write_files':[{'path':'/usr/local/sbin/runnerloom-image-build','permissions':'0700','content':script}],'runcmd':[['bash','-c','exec /usr/local/sbin/runnerloom-image-build >/dev/ttyS0 2>&1']]}
# JSON is valid YAML; the cloud-config header selects cloud-init's parser.
(p/'build-user-data').write_text('#cloud-config\n'+json.dumps(config))
(p/'build-meta-data').write_text('instance-id: runnerloom-image-builder\nlocal-hostname: runnerloom-image-builder\n')
PYSEED
cloud-localds "$WORK/build-seed.iso" "$WORK/build-user-data" "$WORK/build-meta-data"
echo 'Booting a disposable image builder (no SSH, no incoming ports)...' >&2
: > "$WORK/build.log"
tail -n +1 -f "$WORK/build.log" >&2 &
TAIL_PID=$!
CPU_MODEL=max
if [[ -r /dev/kvm && -w /dev/kvm ]]; then CPU_MODEL=host; fi
set +e
timeout --signal=TERM --kill-after=30s 30m qemu-system-x86_64 \
  -machine q35,accel=kvm:tcg -cpu "$CPU_MODEL" -smp 2 -m 2048 \
  -display none -monitor none -serial "file:$WORK/build.log" -no-reboot \
  -drive "file=$WORK/golden.qcow2,if=virtio,format=qcow2,cache=none" \
  -drive "file=$WORK/build-seed.iso,if=virtio,format=raw,readonly=on" \
  -netdev user,id=buildernet -device virtio-net-pci,netdev=buildernet \
  -device virtio-rng-pci
BUILD_STATUS=$?
set -e
kill "$TAIL_PID" 2>/dev/null || true
wait "$TAIL_PID" 2>/dev/null || true
TAIL_PID=
if [[ "$BUILD_STATUS" != 0 ]] || ! grep -q '^RUNNERLOOM_GOLDEN_BUILD_COMPLETE' "$WORK/build.log" || ! grep -q '^RUNNERLOOM_BUILD_EXIT=0' "$WORK/build.log"; then
  tail -n 160 "$WORK/build.log" >&2 || true
  echo 'Image-builder VM did not complete successfully; nothing is published' >&2
  exit 1
fi
tail -n 40 "$WORK/build.log" >&2
# virt-customize --run evaluates this body with /bin/sh, ignoring a bash
# shebang. Keep the offline cleanup POSIX-compatible; the build VM uses bash.
cat > "$WORK/clean.sh" <<'CLEAN'
#!/bin/sh
set -eu
rm -f /usr/local/sbin/runnerloom-image-build /etc/apt/apt.conf.d/90runnerloom-builder /etc/ssh/ssh_host_*
cloud-init clean --logs --machine-id
: >/etc/machine-id
rm -f /var/lib/dbus/machine-id
ln -s /etc/machine-id /var/lib/dbus/machine-id
rm -f /opt/actions-runner/.runner /opt/actions-runner/.credentials /opt/actions-runner/.credentials_rsaparams
CLEAN
virt-customize --no-network --format qcow2 -a "$WORK/golden.qcow2" --memsize 2048 --run "$WORK/clean.sh" >&2
qemu-img check -f qcow2 "$WORK/golden.qcow2" >&2
python3 - "$WORK" "$OUT" <<'PY'
import datetime,hashlib,json,os,pathlib,sys
p=pathlib.Path(sys.argv[1]);out=pathlib.Path(sys.argv[2]);image=p/'golden.qcow2';h=hashlib.sha256()
with image.open('rb') as f:
    for block in iter(lambda:f.read(8<<20),b''): h.update(block)
manifest={'name':'ubuntu-24','digest':'sha256:'+h.hexdigest(),'minimumRootGiB':20,'runnerVersion':(p/'runner.version').read_text(),'runnerArchiveSHA256':(p/'runner.sha256').read_text(),'baseSHA256':(p/'base.sha256').read_text(),'builtAt':datetime.datetime.now(datetime.timezone.utc).isoformat(),'os':'Ubuntu 24.04','architecture':'x86_64','contents':['git','python3','python3-venv','build-essential','GitHub Actions Runner'],'registered':False}
# link() publishes without overwriting a destination created after validation.
os.link(image,out);out.chmod(0o600)
with open(str(out)+'.manifest.json','x') as f: json.dump(manifest,f,indent=2);f.flush();os.fsync(f.fileno())
with out.open('rb') as f: os.fsync(f.fileno())
fd=os.open(out.parent,os.O_DIRECTORY);os.fsync(fd);os.close(fd)
print(json.dumps(manifest))
PY
