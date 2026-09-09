// Package host is the trusted node boundary. Only typed RunnerLoom operations
// reach libvirt; guests and controllers cannot submit host commands or XML.
package host

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/MOVEI144/RunnerLoom/internal/core"
)

type Executor interface {
	Run(context.Context, string, []string, []byte) ([]byte, error)
}
type SystemExecutor struct{}
type bounded struct {
	bytes.Buffer
	Limit    int
	Overflow bool
}

func (b *bounded) Write(p []byte) (int, error) {
	n := len(p)
	left := b.Limit - b.Len()
	if left > 0 {
		_, _ = b.Buffer.Write(p[:min(left, n)])
	}
	if n > left {
		b.Overflow = true
	}
	return n, nil
}
func (SystemExecutor) Run(ctx context.Context, name string, args []string, input []byte) ([]byte, error) {
	paths := map[string]string{"virsh": "/usr/bin/virsh", "qemu-img": "/usr/bin/qemu-img", "cloud-localds": "/usr/bin/cloud-localds", "nft": "/usr/sbin/nft", "ip": "/usr/sbin/ip"}
	path, ok := paths[name]
	if !ok {
		return nil, errors.New("host command is not allowlisted")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C", "HOME=/root"}
	cmd.Stdin = bytes.NewReader(input)
	cmd.WaitDelay = 2 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	out := &bounded{Limit: 2 << 20}
	errout := &bounded{Limit: 16384}
	cmd.Stdout = out
	cmd.Stderr = errout
	e := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if out.Overflow || errout.Overflow {
		return nil, errors.New("host helper output exceeds limit")
	}
	if e != nil {
		detail := strings.TrimSpace(errout.String())
		detail = regexp.MustCompile(`[A-Za-z0-9_+/=-]{128,}`).ReplaceAllString(detail, "[redacted]")
		detail = regexp.MustCompile(`gh[pousr]_[A-Za-z0-9_]+`).ReplaceAllString(detail, "[redacted]")
		return nil, fmt.Errorf("%s failed (%w): %.1500s", name, e, detail)
	}
	return out.Bytes(), nil
}

type Network struct {
	Cluster string   `json:"cluster"`
	CIDR    string   `json:"cidr"`
	Dir     string   `json:"-"`
	Exec    Executor `json:"-"`
}
type NetworkPlan struct {
	Name   string `json:"name"`
	Bridge string `json:"bridge"`
	Table  string `json:"table"`
	XML    string `json:"xml"`
	Rules  string `json:"rules"`
	Hash   string `json:"hash"`
}
type networkSeal struct {
	Plan  string `json:"plan"`
	Rules string `json:"rules"`
	UUID  string `json:"uuid"`
}

func (n Network) names() (string, string, string) {
	suffix := core.Hash([]byte(n.Cluster))[:10]
	return "rl-" + suffix, "rlb" + suffix, "rl_" + suffix
}
func (n Network) Plan() (NetworkPlan, error) {
	if !core.ValidName(n.Cluster) {
		return NetworkPlan{}, errors.New("invalid cluster")
	}
	prefix, e := netip.ParsePrefix(n.CIDR)
	if e != nil || !prefix.Addr().Is4() || !prefix.Addr().IsPrivate() || prefix.Bits() != 24 || prefix != prefix.Masked() {
		return NetworkPlan{}, errors.New("VM subnet must be a canonical private IPv4 /24")
	}
	name, bridge, table := n.names()
	ip := prefix.Addr().As4()
	ip[3] = 1
	gateway := netip.AddrFrom4(ip).String()
	ip[3] = 10
	start := netip.AddrFrom4(ip).String()
	ip[3] = 250
	end := netip.AddrFrom4(ip).String()
	x := fmt.Sprintf(`<network ipv6="no"><name>%s</name><metadata><owner xmlns="urn:runnerloom:network" cluster="%s"/></metadata><forward mode="nat"/><bridge name="%s" stp="on" delay="0"/><domain name="runnerloom.invalid" localOnly="yes"/><port isolated="yes"/><ip address="%s" netmask="255.255.255.0"><dhcp><range start="%s" end="%s"/></dhcp></ip></network>`, name, n.Cluster, bridge, gateway, start, end)
	blocked := []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4"}
	// Include public addresses assigned to this host, not just RFC1918 ranges.
	ifaces, e := net.Interfaces()
	if e != nil {
		return NetworkPlan{}, e
	}
	for _, iface := range ifaces {
		addrs, e := iface.Addrs()
		if e != nil {
			return NetworkPlan{}, e
		}
		for _, addr := range addrs {
			p, e := netip.ParsePrefix(addr.String())
			if e == nil && p.Addr().Is4() && !p.Addr().IsPrivate() && !p.Addr().IsLoopback() && !p.Addr().IsLinkLocalUnicast() {
				blocked = append(blocked, p.Masked().String())
			}
		}
	}
	sort.Strings(blocked)
	rules := fmt.Sprintf(`table inet %s {
 set private4 { type ipv4_addr; flags interval; auto-merge; elements = { %s } }
 chain host_input { type filter hook input priority -210; policy accept;
  iifname "%s" meta nfproto ipv6 drop
  iifname "%s" udp dport { 53, 67 } accept
  iifname "%s" tcp dport 53 accept
  iifname "%s" drop
 }
 chain guest_forward { type filter hook forward priority -210; policy accept;
  iifname "%s" meta nfproto ipv6 drop
  iifname "%s" oifname "%s" drop
  iifname "%s" ip daddr @private4 drop
  oifname "%s" ct state established,related accept
  oifname "%s" drop
 }
}
`, table, strings.Join(blocked, ", "), bridge, bridge, bridge, bridge, bridge, bridge, bridge, bridge, bridge, bridge)
	return NetworkPlan{Name: name, Bridge: bridge, Table: table, XML: x, Rules: rules, Hash: core.Hash([]byte(x + rules))}, nil
}
func canonicalRules(b []byte) (string, error) {
	var v map[string]any
	if e := json.Unmarshal(b, &v); e != nil {
		return "", e
	}
	delete(v, "metainfo")
	var scrub func(any)
	scrub = func(x any) {
		switch vv := x.(type) {
		case map[string]any:
			delete(vv, "handle")
			delete(vv, "index")
			delete(vv, "metainfo")
			for _, z := range vv {
				scrub(z)
			}
		case []any:
			for _, z := range vv {
				scrub(z)
			}
		}
	}
	scrub(v)
	return core.Fingerprint(v), nil
}
func (n Network) Check(ctx context.Context) error {
	p, e := n.Plan()
	if e != nil {
		return e
	}
	b, e := core.ReadSecret(filepath.Join(n.Dir, "network-seal.json"))
	if e != nil {
		return errors.New("network has not been explicitly applied")
	}
	var seal networkSeal
	if e = core.Decode(bytes.NewReader(b), &seal); e != nil {
		return e
	}
	if seal.Plan != p.Hash {
		return errors.New("host addressing or network plan changed; drain and reapply")
	}
	b, e = n.Exec.Run(ctx, "nft", []string{"-j", "list", "table", "inet", p.Table}, nil)
	if e != nil {
		return e
	}
	hash, e := canonicalRules(b)
	if e != nil {
		return e
	}
	if hash != seal.Rules {
		return errors.New("network firewall drift detected")
	}
	b, e = n.Exec.Run(ctx, "virsh", []string{"--connect", "qemu:///system", "net-dumpxml", p.Name}, nil)
	if e != nil {
		return e
	}
	uuid, e := verifyNetwork(b, n.Cluster, p.Name, p.Bridge)
	if e != nil {
		return e
	}
	if uuid != seal.UUID {
		return errors.New("network identity changed")
	}
	b, e = n.Exec.Run(ctx, "virsh", []string{"--connect", "qemu:///system", "net-info", p.Name}, nil)
	if e != nil {
		return e
	}
	if !strings.Contains(string(b), "Active:") || !strings.Contains(strings.Join(strings.Fields(string(b)), " "), "Active: yes") {
		return errors.New("VM network is not active")
	}
	return nil
}
func verifyNetwork(b []byte, cluster, name, bridge string) (string, error) {
	var n struct {
		Name   string `xml:"name"`
		UUID   string `xml:"uuid"`
		Bridge struct {
			Name string `xml:"name,attr"`
		} `xml:"bridge"`
		Metadata struct {
			Owner struct {
				Cluster string `xml:"cluster,attr"`
			} `xml:"urn:runnerloom:network owner"`
		} `xml:"metadata"`
	}
	if e := xml.Unmarshal(b, &n); e != nil {
		return "", e
	}
	if n.Name != name || n.Bridge.Name != bridge || n.Metadata.Owner.Cluster != cluster || n.UUID == "" {
		return "", errors.New("network is not owned by this cluster")
	}
	return n.UUID, nil
}
func (n Network) Apply(ctx context.Context) error {
	p, e := n.Plan()
	if e != nil {
		return e
	}
	if e = core.PrivateDir(n.Dir); e != nil {
		return e
	}
	if n.Check(ctx) == nil {
		return nil
	}
	// Do not rewrite a live network while any RunnerLoom VM uses it.
	b, e := n.Exec.Run(ctx, "virsh", []string{"--connect", "qemu:///system", "list", "--name"}, nil)
	if e != nil {
		return e
	}
	if strings.Contains(string(b), "rl-") {
		return errors.New("drain and stop RunnerLoom VMs before changing network policy")
	}
	b, e = n.Exec.Run(ctx, "ip", []string{"-j", "-4", "route", "show", "table", "all"}, nil)
	if e != nil {
		return e
	}
	var routes []struct {
		Dst string `json:"dst"`
		Dev string `json:"dev"`
	}
	if e = json.Unmarshal(b, &routes); e != nil {
		return e
	}
	wanted, _ := netip.ParsePrefix(n.CIDR)
	for _, r := range routes {
		if r.Dst == "default" || r.Dev == p.Bridge {
			continue
		}
		actual, e := netip.ParsePrefix(r.Dst)
		if e == nil && wanted.Overlaps(actual) {
			return fmt.Errorf("VM subnet conflicts with existing route %s", r.Dst)
		}
	}
	exists := false
	b, e = n.Exec.Run(ctx, "virsh", []string{"--connect", "qemu:///system", "net-dumpxml", p.Name}, nil)
	if e == nil {
		if _, e = verifyNetwork(b, n.Cluster, p.Name, p.Bridge); e != nil {
			return e
		}
		exists = true
	}
	// Refuse to overwrite an nft table without our existing private ownership seal.
	_, tableErr := n.Exec.Run(ctx, "nft", []string{"list", "table", "inet", p.Table}, nil)
	rules := p.Rules
	if tableErr == nil {
		if _, e = core.ReadSecret(filepath.Join(n.Dir, "network-seal.json")); e != nil {
			return errors.New("existing firewall table has no RunnerLoom ownership seal")
		}
		rules = "delete table inet " + p.Table + "\n" + rules
	}
	if _, e = n.Exec.Run(ctx, "nft", []string{"--check", "--file", "-"}, []byte(rules)); e != nil {
		return e
	}
	// Install restrictive filtering before the virtual network is started.
	if tableErr != nil {
		pending, _ := json.Marshal(networkSeal{Plan: p.Hash})
		if e = core.WritePrivate(filepath.Join(n.Dir, "network-seal.json"), pending); e != nil {
			return e
		}
	}
	if _, e = n.Exec.Run(ctx, "nft", []string{"--file", "-"}, []byte(rules)); e != nil {
		return e
	}
	if !exists {
		path := filepath.Join(n.Dir, "network.xml")
		if e = core.WritePrivate(path, []byte(p.XML)); e != nil {
			return e
		}
		if _, e = n.Exec.Run(ctx, "virsh", []string{"--connect", "qemu:///system", "net-define", path}, nil); e != nil {
			return e
		}
	}
	if _, startErr := n.Exec.Run(ctx, "virsh", []string{"--connect", "qemu:///system", "net-start", p.Name}, nil); startErr != nil {
		info, infoErr := n.Exec.Run(ctx, "virsh", []string{"--connect", "qemu:///system", "net-info", p.Name}, nil)
		if infoErr != nil || !strings.Contains(strings.Join(strings.Fields(string(info)), " "), "Active: yes") {
			return fmt.Errorf("VM network could not start: %w", startErr)
		}
	}
	// Do not libvirt-autostart the network before firewall restoration on reboot.
	if _, e = n.Exec.Run(ctx, "virsh", []string{"--connect", "qemu:///system", "net-autostart", p.Name, "--disable"}, nil); e != nil {
		return e
	}
	b, e = n.Exec.Run(ctx, "virsh", []string{"--connect", "qemu:///system", "net-dumpxml", p.Name}, nil)
	if e != nil {
		return e
	}
	uuid, e := verifyNetwork(b, n.Cluster, p.Name, p.Bridge)
	if e != nil {
		return e
	}
	b, e = n.Exec.Run(ctx, "nft", []string{"-j", "list", "table", "inet", p.Table}, nil)
	if e != nil {
		return e
	}
	hash, e := canonicalRules(b)
	if e != nil {
		return e
	}
	seal, _ := json.Marshal(networkSeal{p.Hash, hash, uuid})
	if e = core.WritePrivate(filepath.Join(n.Dir, "network-seal.json"), seal); e != nil {
		return e
	}
	return n.Check(ctx)
}
