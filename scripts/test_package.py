#!/usr/bin/env python3
"""Validate distribution checksums and test the extracted CLI, without installation."""
import hashlib,json,pathlib,subprocess,sys,tarfile,tempfile
if sys.version_info < (3, 12):
    raise SystemExit("Package verification requires Python 3.12 or newer")
root=pathlib.Path(sys.argv[1] if len(sys.argv)>1 else 'dist').resolve()
for line in (root/'SHA256SUMS').read_text().splitlines():
    expected,name=line.split('  ',1)
    assert pathlib.Path(name).name==name,'unsafe checksum path'
    with (root/name).open('rb') as f:assert hashlib.file_digest(f,'sha256').hexdigest()==expected,name
archives=list(root.glob('runnerloom-*-linux-amd64.tar.gz'));assert len(archives)==1
with tempfile.TemporaryDirectory() as tmp:
    with tarfile.open(archives[0]) as tar:
        assert all(m.isfile() or m.isdir() for m in tar.getmembers()),'links not permitted'
        tar.extractall(tmp,filter='data')
    stage=next(pathlib.Path(tmp).iterdir());binary=stage/'runnerloom'
    info=json.loads((stage/'BUILDINFO.json').read_text())
    version=json.loads(subprocess.check_output([binary,'version','--json'],text=True))
    assert version['ok'] and version['data']['version']==info['version']
    assert version['data']['commit']==info['sourceCommit']
    subprocess.run([binary,'config','validate','--file',stage/'examples/cluster.json','--json'],check=True)
    schema=json.loads(subprocess.check_output([binary,'config','schema'],text=True))
    assert schema==json.loads((stage/'cluster.schema.json').read_text())
    assert (stage/'THIRD_PARTY_NOTICES.txt').stat().st_size>1000
    for package in root.glob('*.deb'):
        assert subprocess.check_output(['dpkg-deb','-f',package,'Package'],text=True).strip()=='runnerloom'
        ctrl=pathlib.Path(tmp)/'control';ctrl.mkdir(exist_ok=True)
        subprocess.run(['dpkg-deb','-e',package,ctrl],check=True)
        assert {p.name for p in ctrl.iterdir()}=={'control'},'install scripts unexpectedly added'
print('Distribution archive, hashes, embedded version, schema, CLI and optional Debian package verified.')
