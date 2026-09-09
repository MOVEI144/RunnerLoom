#!/usr/bin/env python3
"""Build a versioned Linux/amd64 archive and optional .deb; never install services."""
from __future__ import annotations
import argparse, gzip, hashlib, io, json, os, pathlib, re, shutil, subprocess, sys, tarfile, tempfile

if sys.version_info < (3, 12):
    raise SystemExit("Packaging requires Python 3.12 or newer")

ROOT = pathlib.Path(__file__).resolve().parents[1]
VERSION_RE = re.compile(r"(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[a-z0-9]+(?:\.[a-z0-9]+)*)?")

def run(args: list[str], **kw) -> str:
    return subprocess.check_output(args, text=True, cwd=ROOT, **kw).strip()

def sha(path: pathlib.Path) -> str:
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()

def build(out: pathlib.Path, deb: bool) -> list[pathlib.Path]:
    version = (ROOT / 'VERSION').read_text().strip()
    if not VERSION_RE.fullmatch(version):
        raise ValueError('VERSION must be a release version such as 0.1.0-rc.1')
    commit = os.environ.get('SOURCE_COMMIT') or run(['git', 'rev-parse', 'HEAD'])
    if not re.fullmatch('[a-f0-9]{40}', commit):
        raise ValueError('SOURCE_COMMIT must be an exact 40-character commit SHA')
    epoch = int(os.environ.get('SOURCE_DATE_EPOCH') or run(['git', 'show', '-s', '--format=%ct', 'HEAD']))
    if epoch < 0:
        raise ValueError('invalid SOURCE_DATE_EPOCH')
    go = os.environ.get('GO', 'go')
    env = dict(os.environ, CGO_ENABLED='0', GOOS='linux', GOARCH='amd64')
    out.mkdir(parents=True, exist_ok=True)
    out = out.resolve()
    vendor = ROOT / 'vendor'
    if not (vendor / 'modules.txt').is_file():
        raise ValueError('Run go mod vendor first; dependency license notices are required')
    with tempfile.TemporaryDirectory(prefix='runnerloom-package-') as temp:
        stage = pathlib.Path(temp) / f'runnerloom-{version}-linux-amd64'
        stage.mkdir()
        binary = stage / 'runnerloom'
        subprocess.run([go, 'build', '-trimpath', '-buildvcs=false', '-ldflags',
                        f'-s -w -X github.com/MOVEI144/RunnerLoom/internal/cli.Version={version} '
                        f'-X github.com/MOVEI144/RunnerLoom/internal/cli.Commit={commit}',
                        '-o', str(binary), './cmd/runnerloom'], cwd=ROOT, env=env, check=True)
        binary.chmod(0o755)
        for name in ['LICENSE', 'README.md', 'SECURITY.md', 'CHANGELOG.md']:
            shutil.copyfile(ROOT / name, stage / name)
        for name in ['docs', 'examples']:
            shutil.copytree(ROOT / name, stage / name)
        shutil.copyfile(ROOT/'internal/cli/cluster.schema.json',stage/'cluster.schema.json')
        notices = []
        for path in sorted(vendor.rglob('*')):
            if path.is_file() and path.name.upper().startswith(('LICENSE','COPYING','NOTICE')):
                notices.append(f'\n\n===== {path.relative_to(vendor)} =====\n'+path.read_text(errors='replace'))
        if not notices:
            raise ValueError('No third-party license notices found')
        goroot = pathlib.Path(run([go, 'env', 'GOROOT']))
        for name in ['LICENSE', 'PATENTS']:
            if (goroot/name).is_file():
                notices.append(f'\n\n===== Go toolchain / {name} =====\n'+(goroot/name).read_text())
        (stage/'THIRD_PARTY_NOTICES.txt').write_text('RunnerLoom dependency notices\n'+''.join(notices))
        modules = run([go, 'version', '-m', str(binary)])
        info = {'version':version,'sourceCommit':commit,'sourceDateEpoch':epoch,
                'goVersion':run([go,'version']),'target':'linux/amd64',
                'binarySHA256':sha(binary),'dependencies':modules,
                'servicesEnabledOnInstall':False}
        (stage/'BUILDINFO.json').write_text(json.dumps(info,indent=2)+'\n')
        archive=out/f'{stage.name}.tar.gz'
        # Fixed metadata makes equal source, dependency graph and toolchain produce equal archives.
        with archive.open('wb') as f, gzip.GzipFile(filename='',mode='wb',fileobj=f,mtime=epoch) as g, tarfile.open(fileobj=g,mode='w') as tar:
            for path in [stage]+sorted(stage.rglob('*')):
                if path.is_symlink():
                    raise ValueError(f'Symlinks are not included in distribution: {path}')
                name=str(path.relative_to(stage.parent))
                t=tar.gettarinfo(str(path),name);t.mtime=epoch;t.uid=t.gid=0;t.uname=t.gname='root'
                t.mode=0o755 if path.is_dir() or path==binary else 0o644
                if path.is_file():
                    with path.open('rb') as content:tar.addfile(t,content)
                else:tar.addfile(t)
        files=[archive]
        if deb:
            if not shutil.which('dpkg-deb'):
                raise ValueError('dpkg-deb is required for --deb')
            dest=pathlib.Path(temp)/'deb'
            (dest/'DEBIAN').mkdir(parents=True)
            debversion=version.replace('-','~',1)
            (dest/'DEBIAN/control').write_text(f'''Package: runnerloom
Version: {debversion}
Architecture: amd64
Maintainer: RunnerLoom contributors <noreply@github.com>
Section: devel
Priority: optional
Depends: ca-certificates
Suggests: qemu-system-x86, qemu-utils, libvirt-daemon-system, libvirt-clients, cloud-image-utils, nftables, dnsmasq-base, libguestfs-tools, ubuntu-keyring, python3, curl, gpgv
Homepage: https://github.com/MOVEI144/RunnerLoom
Description: Disposable GitHub Actions VMs on your own Linux machines
 Resource-aware controller, authenticated LAN agents and isolated KVM runners.
 Installation does not start services, enroll nodes or alter networking.
''')
            (dest/'usr/bin').mkdir(parents=True)
            shutil.copyfile(binary,dest/'usr/bin/runnerloom');(dest/'usr/bin/runnerloom').chmod(0o755)
            share=dest/'usr/share/doc/runnerloom';share.mkdir(parents=True)
            for path in stage.iterdir():
                if path.name=='runnerloom':continue
                if path.is_dir():shutil.copytree(path,share/path.name)
                else:shutil.copyfile(path,share/path.name)
            for path in [dest]+list(dest.rglob('*')):
                if path.is_dir():path.chmod(0o755)
                elif path!=dest/'usr/bin/runnerloom':path.chmod(0o644)
                os.utime(path,(epoch,epoch))
            package=out/f'runnerloom_{debversion}_amd64.deb'
            subprocess.run(['dpkg-deb','--root-owner-group','--build',str(dest),str(package)],
                           env=dict(os.environ,SOURCE_DATE_EPOCH=str(epoch)),check=True)
            files.append(package)
        infofile=out/f'runnerloom-{version}-buildinfo.json';shutil.copyfile(stage/'BUILDINFO.json',infofile);files.append(infofile)
        checks=out/'SHA256SUMS';checks.write_text(''.join(f'{sha(p)}  {p.name}\n' for p in files));files.append(checks)
        return files

if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out',type=pathlib.Path,default=ROOT/'dist')
    parser.add_argument('--deb',action='store_true')
    args=parser.parse_args()
    try:
        for file in build(args.out,args.deb):print(file)
    except (OSError,ValueError,subprocess.CalledProcessError) as exc:
        parser.exit(1,f'Packaging failed: {exc}\n')
