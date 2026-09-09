package host

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/MOVEI144/RunnerLoom/internal/core"
)

type Provider interface {
	Ensure(context.Context, core.Instance, string) error
	Stop(context.Context, string) error
	Delete(context.Context, string) error
	Inventory(context.Context) ([]core.VMReport, error)
	Ready(context.Context) error
}
type Libvirt struct {
	DiagnosticProbe string
	StateDir        string
	DiskDir         string
	Node            string
	Cluster         string
	Ceiling         core.Resources
	Network         Network
	Images          *Images
	Exec            Executor
	QEMUUser        string
	Emulator        string
	mu              sync.Mutex
	consoles        map[string]net.Listener
}
type manifest struct {
	Instance   core.Instance `json:"instance"`
	Phase      string        `json:"phase"`
	JITHash    string        `json:"jitHash"`
	Diagnostic bool          `json:"diagnostic"`
}

func (l *Libvirt) Ready(ctx context.Context) error {
	if !l.Ceiling.Valid() {
		return errors.New("invalid local resource ceiling")
	}
	return l.Network.Check(ctx)
}
func (l *Libvirt) manifestPath(id string) (string, error) {
	if !core.ValidID(id) {
		return "", errors.New("invalid VM ID")
	}
	return filepath.Join(l.StateDir, "instances", id+".json"), nil
}
func (l *Libvirt) load(id string) (manifest, error) {
	path, e := l.manifestPath(id)
	if e != nil {
		return manifest{}, e
	}
	b, e := core.ReadSecret(path)
	if e != nil {
		return manifest{}, e
	}
	var m manifest
	e = core.Decode(bytes.NewReader(b), &m)
	return m, e
}
func (l *Libvirt) save(m manifest) error {
	path, e := l.manifestPath(m.Instance.ID)
	if e != nil {
		return e
	}
	b, _ := json.Marshal(m)
	return core.WritePrivate(path, b)
}
func (l *Libvirt) path(id string) (string, error) {
	if !core.ValidID(id) {
		return "", errors.New("invalid VM ID")
	}
	if !filepath.IsAbs(l.DiskDir) || filepath.Clean(l.DiskDir) != l.DiskDir || l.DiskDir == "/" {
		return "", errors.New("invalid VM storage path")
	}
	return filepath.Join(l.DiskDir, id), nil
}
func regular(path string) error {
	st, e := os.Lstat(path)
	if e != nil {
		return e
	}
	if !st.Mode().IsRegular() {
		return errors.New("non-regular VM file")
	}
	return nil
}
func safeDirectory(path string, mode os.FileMode) error {
	for p := path; ; p = filepath.Dir(p) {
		st, e := os.Lstat(p)
		if e != nil && !os.IsNotExist(e) {
			return e
		}
		if e == nil && (st.Mode()&os.ModeSymlink != 0 || !st.IsDir()) {
			return errors.New("unsafe storage directory component")
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	_, before := os.Lstat(path)
	if e := os.MkdirAll(path, mode); e != nil {
		return e
	}
	if os.IsNotExist(before) {
		return os.Chmod(path, mode)
	}
	return nil
}
func (l *Libvirt) diskRoot() error {
	if e := safeDirectory(l.DiskDir, 0711); e != nil {
		return e
	}
	marker := filepath.Join(l.DiskDir, ".runnerloom-owner")
	if b, e := os.ReadFile(marker); e == nil {
		if string(b) != l.Cluster+"/"+l.Node {
			return errors.New("VM storage belongs to another node")
		}
		return nil
	} else if !os.IsNotExist(e) {
		return e
	}
	entries, e := os.ReadDir(l.DiskDir)
	if e != nil {
		return e
	}
	if len(entries) != 0 {
		return errors.New("refusing to adopt a nonempty VM storage directory")
	}
	f, e := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = f.WriteString(l.Cluster + "/" + l.Node)
	if e == nil {
		e = f.Sync()
	}
	f.Close()
	return e
}
func (l *Libvirt) makeReadable(path string) error {
	if e := regular(path); e != nil {
		return e
	}
	name := l.QEMUUser
	if name == "" {
		name = "libvirt-qemu"
	}
	u, e := user.Lookup(name)
	if e != nil {
		return fmt.Errorf("QEMU service account %s is not installed", name)
	}
	uid, e := strconv.Atoi(u.Uid)
	if e != nil {
		return e
	}
	gid, e := strconv.Atoi(u.Gid)
	if e != nil {
		return e
	}
	if uid == 0 {
		return errors.New("QEMU must not run as root")
	}
	if e = os.Chown(path, uid, gid); e != nil {
		return e
	}
	return os.Chmod(path, 0600)
}
func (l *Libvirt) virsh(ctx context.Context, args ...string) ([]byte, error) {
	return l.Exec.Run(ctx, "virsh", append([]string{"--connect", "qemu:///system"}, args...), nil)
}
func (l *Libvirt) domainState(ctx context.Context, a core.Instance) (string, error) {
	b, e := l.virsh(ctx, "list", "--all", "--name")
	if e != nil {
		return "Unknown", e
	}
	found := false
	for _, s := range strings.Fields(string(b)) {
		if s == a.Name() {
			found = true
		}
	}
	if !found {
		return "Absent", nil
	}
	b, e = l.virsh(ctx, "dumpxml", a.Name())
	if e != nil {
		return "Unknown", e
	}
	if e = l.verifyDomain(b, a); e != nil {
		return "Unknown", e
	}
	b, e = l.virsh(ctx, "domstate", a.Name())
	if e != nil {
		return "Unknown", e
	}
	state := strings.TrimSpace(string(b))
	switch state {
	case "running", "idle", "paused", "in shutdown", "pmsuspended":
		return "Running", nil
	case "shut off", "crashed":
		return "Stopped", nil
	default:
		return "Unknown", fmt.Errorf("unsupported libvirt state %q", state)
	}
}
func (l *Libvirt) verifyDomain(b []byte, a core.Instance) error {
	var d struct {
		Name     string `xml:"name"`
		Metadata struct {
			Owner struct {
				Cluster string `xml:"cluster,attr"`
				Node    string `xml:"node,attr"`
				ID      string `xml:"id,attr"`
				Spec    string `xml:"spec,attr"`
			} `xml:"urn:runnerloom:vm owner"`
		} `xml:"metadata"`
	}
	if e := xml.Unmarshal(b, &d); e != nil {
		return e
	}
	o := d.Metadata.Owner
	if d.Name != a.Name() || o.Cluster != l.Cluster || o.Node != l.Node || o.ID != a.ID || o.Spec != core.Fingerprint(a.Pool) {
		return errors.New("refusing to operate on an unowned or changed domain")
	}
	return nil
}
func escaped(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
func (l *Libvirt) DomainXML(a core.Instance) (string, error) {
	if e := core.ValidateInstance(a); e != nil {
		return "", e
	}
	dir, e := l.path(a.ID)
	if e != nil {
		return "", e
	}
	network, _, _ := l.Network.names()
	kind := "kvm"
	if l.Emulator == "tcg" {
		kind = "qemu"
	} else if l.Emulator != "" && l.Emulator != "kvm" {
		return "", errors.New("unknown emulator")
	}
	uuid := a.ID[:8] + "-" + a.ID[8:12] + "-" + a.ID[12:16] + "-" + a.ID[16:20] + "-" + a.ID[20:]
	mac := "52:54:" + a.ID[:2] + ":" + a.ID[2:4] + ":" + a.ID[4:6] + ":" + a.ID[6:8]
	disk := func(name, target, device, format string) string {
		ro := ""
		if device == "cdrom" {
			ro = "<readonly/>"
		}
		return fmt.Sprintf(`<disk type="file" device="%s"><driver name="qemu" type="%s" cache="none"/><source file="%s"/><target dev="%s" bus="%s"/>%s</disk>`, device, format, escaped(filepath.Join(dir, name)), target, map[bool]string{true: "sata", false: "virtio"}[device == "cdrom"], ro)
	}
	disks := disk("root.qcow2", "vda", "disk", "qcow2") + disk("seed.iso", "sda", "cdrom", "raw")
	if a.Pool.ScratchGiB > 0 {
		disks += disk("scratch.qcow2", "vdb", "disk", "qcow2")
	}
	cpu := `<cpu mode="host-passthrough" migratable="off"/>`
	if kind == "qemu" {
		cpu = `<cpu mode="custom"><model fallback="allow">qemu64</model></cpu>`
	}
	return fmt.Sprintf(`<domain type="%s"><name>%s</name><uuid>%s</uuid><metadata><owner xmlns="urn:runnerloom:vm" cluster="%s" node="%s" id="%s" spec="%s"/></metadata><memory unit="MiB">%d</memory><currentMemory unit="MiB">%d</currentMemory><vcpu placement="static">%d</vcpu><os><type arch="x86_64" machine="q35">hvm</type><boot dev="hd"/><bootmenu enable="no"/></os><features><acpi/><apic/></features>%s<clock offset="utc"/><on_poweroff>destroy</on_poweroff><on_reboot>destroy</on_reboot><on_crash>destroy</on_crash><devices><emulator>/usr/bin/qemu-system-x86_64</emulator>%s<interface type="network"><mac address="%s"/><source network="%s"/><model type="virtio"/><port isolated="yes"/><filterref filter="clean-traffic"><parameter name="CTRL_IP_LEARNING" value="dhcp"/></filterref></interface><serial type="unix"><source mode="connect" path="%s"><reconnect enabled="yes" timeout="5"/></source><target port="0"/></serial><memballoon model="none"/><rng model="virtio"><backend model="random">/dev/urandom</backend></rng></devices></domain>`, kind, a.Name(), uuid, l.Cluster, l.Node, a.ID, core.Fingerprint(a.Pool), a.Pool.MemoryMiB, a.Pool.MemoryMiB, a.Pool.VCPU, cpu, disks, mac, network, escaped(filepath.Join(dir, "serial.sock"))), nil
}
func cloudConfig(a core.Instance, jit string, diagnostic bool, probes ...string) []byte {
	run := `#!/bin/bash
set -eu
trap 'rm -f /run/runnerloom-jit; sync; systemctl poweroff --no-block' EXIT
chmod 0600 /run/runnerloom-jit
chown runner:runner /run/runnerloom-jit
cd /opt/actions-runner
timeout --signal=TERM --kill-after=30s RUNNERLOOM_TIMEOUT_SECONDS runuser -u runner -- /bin/bash -c 'exec ./run.sh --jitconfig "$(cat /run/runnerloom-jit)"'
echo RUNNERLOOM_RUNNER_EXITED
`
	run = strings.ReplaceAll(run, "RUNNERLOOM_TIMEOUT_SECONDS", strconv.FormatInt(max(int64(1), a.Deadline.Unix()-time.Now().Unix()), 10))
	if a.Pool.ScratchGiB > 0 {
		run = strings.Replace(run, "cd /opt/actions-runner", `if [ -b /dev/vdb ]; then
  mkfs.ext4 -q /dev/vdb
  mkdir -p /scratch
  mount -o nodev,nosuid /dev/vdb /scratch
  chown runner:runner /scratch
fi
cd /opt/actions-runner`, 1)
	}
	if diagnostic {
		run = `#!/bin/bash
set -eu
trap 'rc=$?; echo RUNNERLOOM_DIAGNOSTIC_EXIT=$rc; sync; sleep 2; systemctl poweroff --no-block' EXIT
echo RUNNERLOOM_REAL_VM_STARTED
uname -a
python3 -u - <<'PY'
import socket,urllib.request,json,tempfile,pathlib,hashlib
probe=json.loads(RUNNERLOOM_PROBE_JSON)
print("RUNNERLOOM_PROBE_START",probe,flush=True)
if probe:
    target,port=probe.rsplit(':',1)
    s=socket.socket();s.settimeout(3)
    try: s.connect((target,int(port))); raise SystemExit('Host isolation failed: reachable test service')
    except (TimeoutError,OSError): pass
    finally: s.close()
print('RUNNERLOOM_HOST_PROBE_BLOCKED',flush=True)
with urllib.request.urlopen('https://github.com/robots.txt',timeout=30) as response:
    assert response.status==200
print('RUNNERLOOM_PUBLIC_HTTPS_OK')
with tempfile.TemporaryDirectory() as d:
    p=pathlib.Path(d)/'work';payload=b'RunnerLoom CPU job\n'*4096;p.write_bytes(payload)
    assert hashlib.sha256(p.read_bytes()).digest()==hashlib.sha256(payload).digest()
assert sum(i*i for i in range(10000))==333283335000
print('RUNNERLOOM_CPU_AND_DISK_JOB_OK')
print('RUNNERLOOM_LAN_PROBES_BLOCKED')
PY
if [ -x /opt/actions-runner/bin/Runner.Listener ]; then
  runuser -u runner -- /opt/actions-runner/bin/Runner.Listener --version
  echo RUNNERLOOM_GITHUB_RUNNER_PRESENT
fi
echo RUNNERLOOM_REAL_VM_COMPLETED
`
	}
	if diagnostic {
		probe := ""
		if len(probes) > 0 {
			probe = probes[0]
		}
		encoded, _ := json.Marshal(probe)
		literal, _ := json.Marshal(string(encoded))
		run = strings.ReplaceAll(run, "RUNNERLOOM_PROBE_JSON", string(literal))
	}
	// JSON string scalars are also valid YAML scalars. No interpolation can add YAML keys.
	quote := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	return []byte("#cloud-config\nbootcmd:\n  - [systemctl, mask, --now, serial-getty@ttyS0.service]\n  - [systemctl, mask, --now, ssh.service, ssh.socket]\nssh_pwauth: false\ndisable_root: true\nusers:\n  - name: runner\n    lock_passwd: true\n    shell: /bin/bash\n    sudo: ['ALL=(ALL) NOPASSWD:ALL']\nwrite_files:\n  - path: /run/runnerloom-jit\n    permissions: '0600'\n    content: " + quote(jit) + "\n  - path: /usr/local/sbin/runnerloom-job\n    permissions: '0700'\n    content: " + quote(run) + "\nruncmd:\n  - [bash, -c, 'exec /usr/local/sbin/runnerloom-job >/dev/ttyS0 2>&1']\n")
}
func (l *Libvirt) ensureConsole(id string, dir string) error {
	if l.consoles == nil {
		l.consoles = map[string]net.Listener{}
	}
	if _, ok := l.consoles[id]; ok {
		return nil
	}
	path := filepath.Join(dir, "serial.sock")
	if st, e := os.Lstat(path); e == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return errors.New("serial socket path is occupied")
		}
		if e = os.Remove(path); e != nil {
			return e
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	ln, e := net.Listen("unix", path)
	if e != nil {
		return e
	}
	account := l.QEMUUser
	if account == "" {
		account = "libvirt-qemu"
	}
	u, lookupErr := user.Lookup(account)
	if lookupErr != nil {
		ln.Close()
		return lookupErr
	}
	uid, e := strconv.Atoi(u.Uid)
	if e != nil || uid == 0 {
		ln.Close()
		return errors.New("non-root QEMU account required")
	}
	gid, e := strconv.Atoi(u.Gid)
	if e != nil {
		ln.Close()
		return e
	}
	if e = os.Chown(path, uid, gid); e == nil {
		e = os.Chmod(path, 0600)
	}
	if e != nil {
		ln.Close()
		return e
	}
	l.consoles[id] = ln
	logdir := filepath.Join(l.StateDir, "logs")
	if e = core.PrivateDir(logdir); e != nil {
		ln.Close()
		delete(l.consoles, id)
		return e
	}
	logpath := filepath.Join(logdir, id+".log")
	go func() {
		for {
			conn, e := ln.Accept()
			if e != nil {
				return
			}
			func() {
				defer conn.Close()
				f, e := os.OpenFile(logpath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
				if e != nil {
					return
				}
				defer f.Close()
				st, e := f.Stat()
				if e != nil {
					return
				}
				remaining := max(int64(0), 16*(1<<20)-st.Size())
				if remaining > 0 {
					_, _ = io.CopyN(f, conn, remaining)
				}
				_, _ = io.Copy(io.Discard, conn)
			}()
		}
	}()
	return nil
}
func (l *Libvirt) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, c := range l.consoles {
		_ = c.Close()
		delete(l.consoles, id)
	}
	return nil
}
func (l *Libvirt) Ensure(ctx context.Context, a core.Instance, jit string) error {
	return l.ensure(ctx, a, jit, false)
}

// EnsureDiagnostic is a local-only fixed smoke test, never accepted in the node protocol.
func (l *Libvirt) EnsureDiagnostic(ctx context.Context, a core.Instance) error {
	return l.ensure(ctx, a, "diagnostic", true)
}
func (l *Libvirt) ensure(ctx context.Context, a core.Instance, jit string, diagnostic bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := core.ValidateInstance(a); e != nil {
		return e
	}
	if a.Node != l.Node || !a.Pool.Charge().Fits(l.Ceiling) || len(jit) == 0 || len(jit) > 131072 {
		return errors.New("VM request exceeds local policy")
	}
	if a.Pool.VCPU < 1 || a.Pool.MemoryMiB < 512 || a.Pool.OverheadMiB < 512 || a.Pool.RootGiB < a.Image.MinimumRootGiB || a.Pool.ScratchGiB < 0 || a.Pool.DiskOverheadGiB < 1 {
		return errors.New("invalid VM component sizes")
	}
	m, e := l.load(a.ID)
	if e == nil {
		if core.Fingerprint(m.Instance.Pool) != core.Fingerprint(a.Pool) || m.Instance.Image.Digest != a.Image.Digest || m.Instance.Node != a.Node || m.JITHash != core.Hash([]byte(jit)) || m.Diagnostic != diagnostic {
			return errors.New("VM identity already bound to another request")
		}
		if m.Phase == "deleted" || m.Phase == "stopped" {
			return nil
		}
		if m.Phase == "start-issued" || m.Phase == "running" {
			dir, _ := l.path(a.ID)
			if e = l.ensureConsole(a.ID, dir); e != nil {
				return e
			}
			state, e := l.domainState(ctx, a)
			if e != nil {
				return e
			}
			if state == "Running" {
				m.Phase = "running"
				return l.save(m)
			}
			if state == "Stopped" || state == "Absent" {
				m.Phase = "stopped"
				return l.save(m)
			}
			return errors.New("VM state is unknown")
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	if !time.Now().Before(a.Deadline) {
		return errors.New("VM request expired")
	}
	if e = l.Ready(ctx); e != nil {
		return e
	}
	if e = l.Images.Verify(ctx, a.Image.Digest); e != nil {
		return e
	}
	if e = l.diskRoot(); e != nil {
		return e
	}
	// A local ledger independently enforces the administrator-approved ceiling.
	entries, e := os.ReadDir(filepath.Join(l.StateDir, "instances"))
	if e != nil && !os.IsNotExist(e) {
		return e
	}
	used := core.Resources{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if id == a.ID {
			continue
		}
		other, e := l.load(id)
		if e != nil {
			return e
		}
		if other.Phase == "deleted" {
			continue
		}
		charge := other.Instance.Pool.Charge()
		if other.Phase == "stopped" {
			charge.CPU = 0
			charge.Memory = 0
		}
		used = used.Add(charge)
	}
	if !used.Add(a.Pool.Charge()).Fits(l.Ceiling) {
		return errors.New("local resource commitments exhausted")
	}
	m = manifest{Instance: a, Phase: "prepared", JITHash: core.Hash([]byte(jit)), Diagnostic: diagnostic}
	if e = l.save(m); e != nil {
		return e
	}
	dir, _ := l.path(a.ID)
	if e = safeDirectory(dir, 0711); e != nil {
		return e
	}
	// Publish a read-only hard link in the libvirt-accessible storage tree. The
	// private cache stays inaccessible, and the base never contains credentials.
	baseDir := filepath.Join(l.DiskDir, "base")
	if e = safeDirectory(baseDir, 0711); e != nil {
		return e
	}
	base := filepath.Join(baseDir, a.Image.Digest[7:]+".qcow2")
	source, _ := l.Images.Path(a.Image.Digest)
	if _, e = os.Lstat(base); os.IsNotExist(e) {
		if e = os.Link(source, base); e != nil {
			return fmt.Errorf("image cache and VM storage must share a filesystem: %w", e)
		}
		if e = os.Chmod(base, 0444); e != nil {
			return e
		}
	} else if e != nil {
		return e
	}
	if e = regular(base); e != nil {
		return e
	}
	root := filepath.Join(dir, "root.qcow2")
	if _, e = os.Lstat(root); os.IsNotExist(e) {
		if _, e = l.Exec.Run(ctx, "qemu-img", []string{"create", "-f", "qcow2", "-F", "qcow2", "-b", base, root, fmt.Sprintf("%dG", a.Pool.RootGiB)}, nil); e != nil {
			return e
		}
	} else if e != nil {
		return e
	}
	if e = l.makeReadable(root); e != nil {
		return e
	}
	if a.Pool.ScratchGiB > 0 {
		scratch := filepath.Join(dir, "scratch.qcow2")
		if _, e = os.Lstat(scratch); os.IsNotExist(e) {
			if _, e = l.Exec.Run(ctx, "qemu-img", []string{"create", "-f", "qcow2", scratch, fmt.Sprintf("%dG", a.Pool.ScratchGiB)}, nil); e != nil {
				return e
			}
		} else if e != nil {
			return e
		}
		if e = l.makeReadable(scratch); e != nil {
			return e
		}
	}
	private := filepath.Join(l.StateDir, "seeds", a.ID)
	if e = core.PrivateDir(private); e != nil {
		return e
	}
	if e = core.WritePrivate(filepath.Join(private, "user-data"), cloudConfig(a, jit, diagnostic, l.DiagnosticProbe)); e != nil {
		return e
	}
	if e = core.WritePrivate(filepath.Join(private, "meta-data"), []byte("instance-id: "+a.ID+"\nlocal-hostname: "+a.Name()+"\n")); e != nil {
		return e
	}
	seed := filepath.Join(dir, "seed.iso")
	if _, e = os.Lstat(seed); os.IsNotExist(e) {
		if _, e = l.Exec.Run(ctx, "cloud-localds", []string{seed, filepath.Join(private, "user-data"), filepath.Join(private, "meta-data")}, nil); e != nil {
			return e
		}
	} else if e != nil {
		return e
	}
	if e = l.makeReadable(seed); e != nil {
		return e
	}
	if e = l.ensureConsole(a.ID, dir); e != nil {
		return e
	}
	x, e := l.DomainXML(a)
	if e != nil {
		return e
	}
	xmlpath := filepath.Join(private, "domain.xml")
	if e = core.WritePrivate(xmlpath, []byte(x)); e != nil {
		return e
	}
	state, e := l.domainState(ctx, a)
	if e != nil {
		return e
	}
	if state == "Absent" {
		if _, e = l.virsh(ctx, "define", xmlpath); e != nil {
			return e
		}
	} else if state == "Running" {
		return errors.New("unexpected running domain before start intent")
	}
	// Persist intent before start: after an ambiguous response, NEVER start again.
	m.Phase = "start-issued"
	if e = l.save(m); e != nil {
		return e
	}
	if _, e = l.virsh(ctx, "start", a.Name()); e != nil {
		return e
	}
	m.Phase = "running"
	return l.save(m)
}
func (l *Libvirt) Inventory(ctx context.Context) ([]core.VMReport, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries, e := os.ReadDir(filepath.Join(l.StateDir, "instances"))
	if os.IsNotExist(e) {
		return []core.VMReport{}, nil
	}
	if e != nil {
		return nil, e
	}
	out := []core.VMReport{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		m, e := l.load(id)
		if e != nil {
			return nil, e
		}
		r := core.VMReport{ID: id, State: "Unknown"}
		if m.Phase == "deleted" {
			r.State = "Deleted"
			out = append(out, r)
			continue
		}
		dir, _ := l.path(id)
		if m.Phase == "running" || m.Phase == "start-issued" {
			if e = l.ensureConsole(id, dir); e != nil {
				return nil, e
			}
		}
		state, e := l.domainState(ctx, m.Instance)
		if e == nil {
			switch state {
			case "Running":
				r.State = "Running"
			case "Stopped":
				r.State = "Stopped"
				m.Phase = "stopped"
				if e = l.save(m); e != nil {
					return nil, e
				}
			case "Absent":
				if m.Phase != "prepared" {
					r.State = "Stopped"
					m.Phase = "stopped"
					if e = l.save(m); e != nil {
						return nil, e
					}
				}
			}
		} else {
			r.Detail = "HOST_STATE_UNCERTAIN"
		}
		out = append(out, r)
	}
	return out, nil
}
func (l *Libvirt) Stop(ctx context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	m, e := l.load(id)
	if os.IsNotExist(e) {
		return errors.New("cannot stop an unknown VM; reconciliation required")
	}
	if e != nil {
		return e
	}
	if m.Phase == "deleted" {
		return nil
	}
	state, e := l.domainState(ctx, m.Instance)
	if e != nil {
		return e
	}
	if state == "Running" {
		if _, e = l.virsh(ctx, "destroy", m.Instance.Name()); e != nil {
			return e
		}
	} else if state != "Stopped" && state != "Absent" {
		return errors.New("VM stop not confirmed")
	}
	m.Phase = "stopped"
	return l.save(m)
}
func (l *Libvirt) Delete(ctx context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	m, e := l.load(id)
	if e != nil {
		return e
	}
	if m.Phase == "deleted" {
		return nil
	}
	state, e := l.domainState(ctx, m.Instance)
	if e != nil {
		return e
	}
	if state == "Running" || state == "Unknown" {
		return errors.New("refusing to delete disks before confirmed shutdown")
	}
	if state == "Stopped" {
		if _, e = l.virsh(ctx, "undefine", m.Instance.Name()); e != nil {
			return e
		}
	}
	if ln, ok := l.consoles[id]; ok {
		_ = ln.Close()
		delete(l.consoles, id)
	}
	dir, _ := l.path(id)
	if e = removeKnown(dir, []string{"root.qcow2", "scratch.qcow2", "seed.iso", "serial.sock"}); e != nil {
		return e
	}
	if e = removeKnown(filepath.Join(l.StateDir, "seeds", id), []string{"user-data", "meta-data", "domain.xml"}); e != nil {
		return e
	}
	m.Phase = "deleted"
	return l.save(m)
}
func removeKnown(dir string, files []string) error {
	root, e := os.OpenRoot(dir)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	defer root.Close()
	for _, name := range files {
		if e = root.Remove(name); e != nil && !os.IsNotExist(e) {
			return e
		}
	}
	if e = os.Remove(dir); e != nil && !os.IsNotExist(e) {
		return e
	}
	return nil
}
func FreeGiB(path string) (int64, error) {
	var st syscall.Statfs_t
	if e := syscall.Statfs(path, &st); e != nil {
		return 0, e
	}
	return int64(st.Bavail) * int64(st.Bsize) / (1 << 30), nil
}

// EnsureStopIntent certifies a never-created request as stopped only after both
// libvirt and the owned disk directory prove it was not provisioned.
func (l *Libvirt) EnsureStopIntent(ctx context.Context, a core.Instance) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := core.ValidateInstance(a); e != nil {
		return e
	}
	if a.Node != l.Node {
		return errors.New("node mismatch")
	}
	if _, e := l.load(a.ID); e == nil {
		return nil
	} else if !os.IsNotExist(e) {
		return e
	}
	state, e := l.domainState(ctx, a)
	if e != nil {
		return e
	}
	if state != "Absent" {
		return errors.New("unrecorded domain requires reconciliation")
	}
	dir, e := l.path(a.ID)
	if e != nil {
		return e
	}
	if _, e = os.Lstat(dir); !os.IsNotExist(e) {
		return errors.New("unrecorded disk directory requires reconciliation")
	}
	return l.save(manifest{Instance: a, Phase: "stopped"})
}

// Init prepares only the explicitly approved, owned VM storage directory.
func (l *Libvirt) Init(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := core.PrivateDir(l.StateDir); e != nil {
		return e
	}
	if e := l.diskRoot(); e != nil {
		return e
	}
	return nil
}
