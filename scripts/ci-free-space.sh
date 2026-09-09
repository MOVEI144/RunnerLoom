#!/usr/bin/env bash
set -euo pipefail
# Never use this cleanup on a user's machine or self-hosted Actions runner.
[[ "${GITHUB_ACTIONS:-}" == true && "${RUNNER_ENVIRONMENT:-}" == github-hosted ]] || {
  echo 'Disk preparation is restricted to disposable GitHub-hosted runners.' >&2
  exit 2
}
evidence="${RUNNER_TEMP:?}/runnerloom-evidence"
mkdir -p "$evidence"
df -h / /var/lib > "$evidence/disk-before.txt"
# Fixed, unrelated preinstalled SDK paths only; no project or runtime directories.
# Go, Python, Node and the runner's own tools are intentionally retained.
for unused in /usr/share/dotnet /usr/local/lib/android /opt/ghc \
  /opt/hostedtoolcache/CodeQL /opt/hostedtoolcache/Java_Temurin-Hotspot_jdk \
  /opt/hostedtoolcache/PyPy /opt/hostedtoolcache/Ruby /usr/local/share/powershell; do
  if [[ -d "$unused" && ! -L "$unused" ]]; then
    echo "Removing unused CI image SDK: $unused"
    sudo du -sh -- "$unused"
    sudo rm -rf -- "$unused"
  fi
done
sudo apt-get clean
df -h / /var/lib | tee "$evidence/disk-after.txt"
python3 - <<'PY'
import shutil
free = shutil.disk_usage('/var/lib').free
required = 30 * 1024**3
if free < required:
    raise SystemExit(f'CI disk has {free / 1024**3:.2f} GiB free; at least 30 GiB required. Runtime capacity checks remain enabled.')
PY
