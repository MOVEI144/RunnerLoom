package host

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MOVEI144/RunnerLoom/internal/core"
)

type fakeHost struct {
	NetworkXML    string
	Active        bool
	Table         bool
	Drift         bool
	LiveVM        bool
	Routes        string
	Commands      []string
	ForeignDomain string
}

func (f *fakeHost) Run(_ context.Context, name string, args []string, input []byte) ([]byte, error) {
	f.Commands = append(f.Commands, name+" "+strings.Join(args, " "))
	switch name {
	case "ip":
		if f.Routes != "" {
			return []byte(f.Routes), nil
		}
		return []byte(`[]`), nil
	case "qemu-img":
		return []byte(`{"format":"qcow2","virtual-size":21474836480}`), nil
	case "nft":
		if len(args) > 0 && args[0] == "--check" {
			return nil, nil
		}
		if len(args) > 0 && args[0] == "--file" {
			f.Table = true
			f.Drift = false
			return nil, nil
		}
		if !f.Table {
			return nil, errors.New("table missing")
		}
		v := "drop"
		if f.Drift {
			v = "accept"
		}
		return []byte(`{"nftables":[{"rule":{"handle":7,"expr":"` + v + `"}}]}`), nil
	case "virsh":
		if len(args) < 3 {
			return nil, errors.New("invalid virsh invocation")
		}
		args = args[2:]
		switch args[0] {
		case "list":
			if f.ForeignDomain != "" {
				return []byte(f.ForeignDomain), nil
			}
			if f.LiveVM {
				return []byte("rl-running-vm"), nil
			}
			return []byte(""), nil
		case "dumpxml":
			return []byte(`<domain><name>` + f.ForeignDomain + `</name></domain>`), nil
		case "net-dumpxml":
			if f.NetworkXML == "" {
				return nil, errors.New("network missing")
			}
			return []byte(f.NetworkXML), nil
		case "net-define":
			b, e := os.ReadFile(args[1])
			if e != nil {
				return nil, e
			}
			f.NetworkXML = strings.Replace(string(b), "<name>", "<uuid>12345678-1234-1234-1234-123456789012</uuid><name>", 1)
			return nil, nil
		case "net-start":
			f.Active = true
			return nil, nil
		case "net-autostart":
			return nil, nil
		case "net-info":
			if f.Active {
				return []byte("Active: yes\nAutostart: no\n"), nil
			}
			return []byte("Active: no"), nil
		}
	}
	return nil, errors.New("test executor refuses unsupported command")
}
func networkFixture(t *testing.T) (Network, *fakeHost) {
	t.Helper()
	f := &fakeHost{}
	return Network{Cluster: "test-cluster", CIDR: "172.30.240.0/24", Dir: filepath.Join(t.TempDir(), "network"), Exec: f}, f
}
func TestNetworkPlanIsScoped(t *testing.T) {
	n, _ := networkFixture(t)
	p, e := n.Plan()
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Bridge) > 15 {
		t.Fatal("Linux interface name exceeds limit")
	}
	for _, must := range []string{"169.254.0.0/16", "100.64.0.0/10", "meta nfproto ipv6 drop", "oifname", "ct state established,related"} {
		if !strings.Contains(p.Rules, must) {
			t.Fatal("missing isolation rule", must)
		}
	}
	if strings.Contains(p.Rules, "flush ruleset") || strings.Contains(p.XML, "<filesystem") {
		t.Fatal("network plan changes unrelated resources")
	}
	if !strings.Contains(p.XML, `<port isolated="yes"/>`) {
		t.Fatal("guest isolation missing")
	}
}
func TestNetworkRejectsInvalidSubnets(t *testing.T) {
	for _, cidr := range []string{"8.8.8.0/24", "172.30.240.1/24", "10.0.0.0/8", "::/64", "garbage"} {
		t.Run(cidr, func(t *testing.T) {
			n, _ := networkFixture(t)
			n.CIDR = cidr
			if _, e := n.Plan(); e == nil {
				t.Fatal("invalid subnet accepted")
			}
		})
	}
}
func TestNetworkApplyAndDrift(t *testing.T) {
	n, f := networkFixture(t)
	if e := n.Check(context.Background()); e == nil {
		t.Fatal("unconfigured network treated as ready")
	}
	if e := n.Apply(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e := n.Check(context.Background()); e != nil {
		t.Fatal(e)
	}
	count := len(f.Commands)
	if e := n.Apply(context.Background()); e != nil {
		t.Fatal(e)
	}
	for _, cmd := range f.Commands[count:] {
		if strings.Contains(cmd, "--file -") {
			t.Fatal("healthy network was needlessly rewritten")
		}
	}
	f.Drift = true
	if e := n.Check(context.Background()); e == nil {
		t.Fatal("firewall drift was not detected")
	}
}
func TestNetworkDoesNotAdoptForeignResources(t *testing.T) {
	n, f := networkFixture(t)
	f.Table = true
	if e := n.Apply(context.Background()); e == nil {
		t.Fatal("unowned firewall table adopted")
	}
}
func TestNetworkRefusesChangesDuringVMExecution(t *testing.T) {
	n, f := networkFixture(t)
	f.LiveVM = true
	if e := n.Apply(context.Background()); e == nil {
		t.Fatal("network changed under live VMs")
	}
}
func TestNetworkRouteConflict(t *testing.T) {
	n, f := networkFixture(t)
	f.Routes = `[{"dst":"172.30.240.0/24","dev":"vpn0"}]`
	if e := n.Apply(context.Background()); e == nil {
		t.Fatal("overlapping route accepted")
	}
}
func TestImageImportAndIntegrity(t *testing.T) {
	data := []byte("FAKE_IMAGE_FOR_UNIT_TEST")
	digest := "sha256:" + core.Hash(data)
	images := &Images{Dir: filepath.Join(t.TempDir(), "images"), LimitGiB: 1, Exec: &fakeHost{}}
	path, e := images.Import(context.Background(), bytes.NewReader(data), digest)
	if e != nil {
		t.Fatal(e)
	}
	if e = images.Verify(context.Background(), digest); e != nil {
		t.Fatal(e)
	}
	if _, e = images.Import(context.Background(), bytes.NewReader([]byte("tampered")), "sha256:"+strings.Repeat("a", 64)); e == nil {
		t.Fatal("invalid image hash accepted")
	}
	if e = os.WriteFile(path, []byte("changed-after-import"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = images.Verify(context.Background(), digest); e == nil {
		t.Fatal("changed cache image accepted")
	}
}
func TestImageSymlinkRejected(t *testing.T) {
	dir := t.TempDir()
	images := &Images{Dir: filepath.Join(dir, "images"), LimitGiB: 1, Exec: &fakeHost{}}
	if e := os.Mkdir(images.Dir, 0700); e != nil {
		t.Fatal(e)
	}
	content := []byte("fake")
	digest := "sha256:" + core.Hash(content)
	target := filepath.Join(dir, "target")
	if e := os.WriteFile(target, content, 0600); e != nil {
		t.Fatal(e)
	}
	p, _ := images.Path(digest)
	if e := os.Symlink(target, p); e != nil {
		t.Fatal(e)
	}
	if e := images.Verify(context.Background(), digest); e == nil {
		t.Fatal("symlink image accepted")
	}
}
func fixtureInstance() core.Instance {
	c := core.Example()
	return core.Instance{ID: core.ID(), RequestID: core.ID(), Node: "node-a", Pool: c.Pools[0], Image: c.Images[0], Created: time.Now(), Deadline: time.Now().Add(time.Hour)}
}
func TestVMXMLIsHeadlessAndOwned(t *testing.T) {
	n, _ := networkFixture(t)
	l := &Libvirt{Cluster: "test-cluster", Node: "node-a", DiskDir: "/var/lib/libvirt/images/test-owned", Network: n}
	a := fixtureInstance()
	x, e := l.DomainXML(a)
	if e != nil {
		t.Fatal(e)
	}
	var parsed any
	_ = parsed
	decoder := xml.NewDecoder(strings.NewReader(x))
	for {
		_, e = decoder.Token()
		if e != nil {
			break
		}
	}
	if e.Error() != "EOF" {
		t.Fatal("invalid XML", e)
	}
	if e = l.verifyDomain([]byte(x), a); e != nil {
		t.Fatal(e)
	}
	for _, bad := range []string{"<filesystem", "<hostdev", "<graphics", "/dev/sda", "virtiofs"} {
		if strings.Contains(x, bad) {
			t.Fatal("unsafe device in generated XML", bad)
		}
	}
	if !strings.Contains(x, "<on_reboot>destroy</on_reboot>") {
		t.Fatal("runner VM could reboot and reuse its environment")
	}
	if strings.Contains(x, "PRIVATE KEY") {
		t.Fatal("private key in VM XML")
	}
}
func TestInvalidVMIdentityRejected(t *testing.T) {
	n, _ := networkFixture(t)
	l := &Libvirt{Cluster: "test-cluster", Node: "node-a", DiskDir: "/var/lib/libvirt/images/test", Network: n}
	a := fixtureInstance()
	a.ID = "../../other"
	if _, e := l.DomainXML(a); e == nil {
		t.Fatal("path traversal accepted")
	}
}
func TestCloudInitCannotInjectKeys(t *testing.T) {
	a := fixtureInstance()
	secret := "\nusers:\n  - name: evil\n\"; reboot\n"
	b := cloudConfig(a, secret, false)
	if strings.Contains(string(b), "\n  - name: evil\n") {
		t.Fatal("JIT content injected cloud-init keys")
	}
	if !strings.Contains(string(b), `\nusers:\n`) {
		t.Fatal("test did not exercise escaped JIT")
	}
	if !bytes.HasPrefix(b, []byte("#cloud-config")) {
		t.Fatal("wrong seed format")
	}
}
func TestDeleteDoesNotRemoveUnknownFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vm")
	if e := os.Mkdir(dir, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(dir, "unknown-data"), []byte("keep"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := removeKnown(dir, []string{"root.qcow2"}); e == nil {
		t.Fatal("unexpected files deleted")
	}
	if _, e := os.Stat(filepath.Join(dir, "unknown-data")); e != nil {
		t.Fatal("unknown file was removed")
	}
}
func TestUnownedDomainCannotBeDeleted(t *testing.T) {
	dir := t.TempDir()
	a := fixtureInstance()
	f := &fakeHost{ForeignDomain: a.Name()}
	l := &Libvirt{StateDir: filepath.Join(dir, "private"), DiskDir: filepath.Join(dir, "disks"), Cluster: "home", Node: "node-a", Exec: f}
	if e := l.save(manifest{Instance: a, Phase: "stopped"}); e != nil {
		t.Fatal(e)
	}
	if e := l.Delete(context.Background(), a.ID); e == nil {
		t.Fatal("foreign domain deletion accepted")
	}
	for _, command := range f.Commands {
		if strings.Contains(command, "undefine") || strings.Contains(command, "destroy") {
			t.Fatal("destructive operation issued before ownership proof")
		}
	}
}
func TestNeverCreatedVMCanBeSafelyCancelled(t *testing.T) {
	dir := t.TempDir()
	a := fixtureInstance()
	l := &Libvirt{StateDir: filepath.Join(dir, "private"), DiskDir: filepath.Join(dir, "disks"), Cluster: "home", Node: "node-a", Exec: &fakeHost{}}
	if e := l.EnsureStopIntent(context.Background(), a); e != nil {
		t.Fatal(e)
	}
	if e := l.Stop(context.Background(), a.ID); e != nil {
		t.Fatal(e)
	}
	if e := l.Delete(context.Background(), a.ID); e != nil {
		t.Fatal(e)
	}
	m, e := l.load(a.ID)
	if e != nil || m.Phase != "deleted" {
		t.Fatal("cancellation did not leave a durable tombstone", e)
	}
}
func TestRuleHashIgnoresKernelHandles(t *testing.T) {
	a, _ := json.Marshal(map[string]any{"nftables": []any{map[string]any{"rule": map[string]any{"handle": 1, "expr": "drop"}}}})
	b := bytes.ReplaceAll(a, []byte(`"handle":1`), []byte(`"handle":99`))
	x, e := canonicalRules(a)
	if e != nil {
		t.Fatal(e)
	}
	y, e := canonicalRules(b)
	if e != nil || x != y {
		t.Fatal("kernel handle changed semantic rule hash")
	}
}

func TestActualHostCeilingValidation(t *testing.T) {
	m, e := parseMemory("MemTotal: 33554432 kB\nMemAvailable: 16777216 kB\n")
	if e != nil {
		t.Fatal(e)
	}
	if e = validateCeiling(core.Resources{CPU: 14, Memory: 24576, Disk: 100}, 16, m); e != nil {
		t.Fatal(e)
	}
	if e = validateCeiling(core.Resources{CPU: 17, Memory: 24576, Disk: 100}, 16, m); e == nil {
		t.Fatal("physical CPU capacity was exceeded")
	}
	if e = validateCeiling(core.Resources{CPU: 16, Memory: 32768, Disk: 100}, 16, m); e == nil {
		t.Fatal("host memory reserve was exhausted")
	}
	if _, e = parseMemory("MemTotal: -1 kB\n"); e == nil {
		t.Fatal("invalid host inventory accepted")
	}
}
