#!/usr/bin/env bash
set -euo pipefail
umask 077
OUT=${1:?output path}
VERSION=${2:-latest}
[[ "$OUT" == /* && "$OUT" == *.qcow2 ]] || { echo 'Output must be an absolute .qcow2 path' >&2; exit 2; }
[[ ! -e "$OUT" && ! -L "$OUT" && ! -e "$OUT.manifest.json" ]] || { echo 'Refusing to replace an existing image or manifest' >&2; exit 2; }
[[ "$VERSION" == latest || "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo 'Invalid runner version' >&2; exit 2; }
for tool in curl gpgv python3 qemu-img virt-resize virt-customize; do command -v "$tool" >/dev/null || { echo "Missing image-builder dependency: $tool" >&2; exit 2; }; done
KEYRING=/usr/share/keyrings/ubuntu-cloudimage-keyring.gpg
[[ -r "$KEYRING" ]] || { echo 'Install ubuntu-keyring to verify Canonical cloud images' >&2; exit 2; }
WORK=$(mktemp -d "$(dirname "$OUT")/.runnerloom-build-XXXXXXXX")
trap 'rm -rf -- "$WORK"' EXIT
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
qemu-img create -f qcow2 "$WORK/golden.qcow2" 20G >&2
export LIBGUESTFS_BACKEND=direct
virt-resize --format qcow2 --output-format qcow2 --expand /dev/sda1 "$WORK/base.qcow2" "$WORK/golden.qcow2" >&2
cat > "$WORK/provision.sh" <<'GUEST'
#!/bin/bash
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
getent passwd runner >/dev/null || useradd --create-home --shell /bin/bash runner
install -d -m 0755 /opt/actions-runner
tar -xzf /tmp/runnerloom-runner.tar.gz -C /opt/actions-runner
cd /opt/actions-runner
./bin/installdependencies.sh
chown -R runner:runner /opt/actions-runner /home/runner
runuser -u runner -- ./bin/Runner.Listener --version >/etc/runnerloom-runner-version
rm -f /tmp/runnerloom-runner.tar.gz
# Never capture any registration, SSH host identity or previous cloud-init state.
rm -f /opt/actions-runner/.runner /opt/actions-runner/.credentials /opt/actions-runner/.credentials_rsaparams
rm -f /etc/ssh/ssh_host_*
systemctl disable ssh.service ssh.socket 2>/dev/null || true
cloud-init clean --logs --machine-id
: >/etc/machine-id
rm -f /var/lib/dbus/machine-id
ln -s /etc/machine-id /var/lib/dbus/machine-id
apt-get clean
rm -rf /var/lib/apt/lists/*
GUEST
virt-customize --format qcow2 -a "$WORK/golden.qcow2" --memsize 2048 --smp 2 \
  --install ca-certificates,curl,git,python3,python3-venv,build-essential,jq,sudo \
  --upload "$WORK/runner.tar.gz:/tmp/runnerloom-runner.tar.gz" \
  --run "$WORK/provision.sh" >&2
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
