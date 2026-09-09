package host

import (
	"context"
	"strings"
	"testing"
)

func TestNetworkRejectsUnmodeledPolicy(t *testing.T) {
	mutations := map[string]func(string) string{
		"dns-domain": func(x string) string { return strings.Replace(x, `localOnly="yes"`, `localOnly="no"`, 1) },
		"dns-forwarder": func(x string) string {
			return strings.Replace(x, `</network>`, `<dns><forwarder addr="10.0.0.1"/></dns></network>`, 1)
		},
		"dns-disabled": func(x string) string { return strings.Replace(x, `</network>`, `<dns enable="no"/></network>`, 1) },
		"nat-source": func(x string) string {
			return strings.Replace(x, `<forward mode="nat"/>`, `<forward mode="nat"><nat><address start="10.0.0.1" end="10.0.0.10"/></nat></forward>`, 1)
		},
		"nat-ports": func(x string) string {
			return strings.Replace(x, `<forward mode="nat"/>`, `<forward mode="nat"><nat><port start="1" end="65535"/></nat></forward>`, 1)
		},
		"host-registration": func(x string) string {
			return strings.Replace(x, `localOnly="yes"`, `localOnly="yes" register="yes"`, 1)
		},
		"guest-rx-filters": func(x string) string {
			return strings.Replace(x, `<network `, `<network trustGuestRxFilters="yes" `, 1)
		},
		"unknown-child": func(x string) string {
			return strings.Replace(x, `</network>`, `<portgroup name="escape" default="yes"/></network>`, 1)
		},
		"missing-domain": func(x string) string {
			return strings.Replace(x, `<domain name="runnerloom.invalid" localOnly="yes"/>`, "", 1)
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			n, fake := networkFixture(t)
			if e := n.Apply(context.Background()); e != nil {
				t.Fatal(e)
			}
			old := fake.NetworkXML
			fake.NetworkXML = mutate(old)
			if fake.NetworkXML == old {
				t.Fatal("test did not mutate input")
			}
			if e := n.Check(context.Background()); e == nil || !strings.Contains(e.Error(), "libvirt network security policy drift") {
				t.Fatalf("unexpected Check result: %v", e)
			}
		})
	}
}
func TestNetworkAllowsMaterializedLibvirtDefaults(t *testing.T) {
	n, fake := networkFixture(t)
	if e := n.Apply(context.Background()); e != nil {
		t.Fatal(e)
	}
	x := fake.NetworkXML
	for _, replacement := range [][2]string{
		{`<network ipv6="no">`, `<network connections="2">`},
		{`<forward mode="nat"/>`, `<forward mode="nat"><nat><port start="1024" end="65535"/></nat></forward>`},
		{`</network>`, `<mac address="52:54:00:00:12:34"/></network>`},
	} {
		next := strings.Replace(x, replacement[0], replacement[1], 1)
		if next == x {
			t.Fatalf("fixture no longer contains %q", replacement[0])
		}
		x = next
	}
	fake.NetworkXML = x
	if e := n.Check(context.Background()); e != nil {
		t.Fatal(e)
	}
}
