package host

import (
	"context"
	"strings"
	"testing"
)

func TestNetworkSecurityDriftPreservesIdentityButMustFail(t *testing.T) {
	mutations := map[string]func(string) string{
		"routed":         func(x string) string { return strings.Replace(x, `mode="nat"`, `mode="route"`, 1) },
		"ipv6":           func(x string) string { return strings.Replace(x, `ipv6="no"`, `ipv6="yes"`, 1) },
		"peer-isolation": func(x string) string { return strings.Replace(x, `isolated="yes"`, `isolated="no"`, 1) },
		"gateway": func(x string) string {
			return strings.Replace(x, `address="172.30.240.1"`, `address="172.30.241.1"`, 1)
		},
		"extra-ipv6": func(x string) string {
			return strings.Replace(x, `</network>`, `<ip family="ipv6" address="fd00::1" prefix="64"/></network>`, 1)
		},
		"extra-route": func(x string) string {
			return strings.Replace(x, `</network>`, `<route address="10.0.0.0" prefix="8" gateway="172.30.240.2"/></network>`, 1)
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			n, fake := networkFixture(t)
			if e := n.Apply(context.Background()); e != nil {
				t.Fatal(e)
			}
			fake.NetworkXML = mutate(fake.NetworkXML)
			if e := n.Check(context.Background()); e == nil || !strings.Contains(e.Error(), "libvirt network security policy drift") {
				t.Fatalf("unexpected network drift result: %v", e)
			}
		})
	}
}

func TestHostRouteConflictsWithVMSubnet(t *testing.T) {
	n, fake := networkFixture(t)
	fake.Routes = `[{"dst":"172.30.240.91","dev":"eth0"}]`
	if e := n.Apply(context.Background()); e == nil || !strings.Contains(e.Error(), "VM subnet conflicts with existing route") {
		t.Fatalf("unexpected route conflict result: %v", e)
	}
}
