package discovery

import (
	"context"
	"strings"
	"testing"
)

func TestParseRequiresUnverifiedControllerRecord(t *testing.T) {
	pin := strings.Repeat("a", 64)
	line := `=;eth0;IPv4;RunnerLoom\032home;_runnerloom._tcp;local;controller.local;192.168.1.2;8443;"url=https://controller.local:8443" "fingerprint=` + pin + `"`
	v := Parse(line + "\n" + line)
	if len(v) != 1 || v[0].Verified || v[0].URL != "https://controller.local:8443" || v[0].Name != "RunnerLoom home" {
		t.Fatalf("invalid discovery parse: %#v", v)
	}
}
func TestDiscoveryIgnoresMalformedRecords(t *testing.T) {
	for _, line := range []string{"", `+;eth0;IPv4;name;_runnerloom._tcp;local`, `=;eth0;IPv4;name;_http._tcp;local;h;192.168.1.2;8443;"url=https://h"`, `=;eth0;IPv4;name;_runnerloom._tcp;local;h;not-an-ip;8443;"url=https://h"`, `=;eth0;IPv4;name;_runnerloom._tcp;local;h;192.168.1.2;8443;"url=http://h" "fingerprint=` + strings.Repeat("a", 64) + `"`} {
		if len(Parse(line)) != 0 {
			t.Fatal("malformed record accepted", line)
		}
	}
}
func TestDiscoveryDoesNotTrustReportedFingerprint(t *testing.T) {
	line := `=;eth0;IPv4;attacker;_runnerloom._tcp;local;attacker.local;192.168.1.250;8443;"url=https://attacker.local:8443" "fingerprint=` + strings.Repeat("b", 64) + `"`
	v := Parse(line)
	if len(v) != 1 || v[0].Verified {
		t.Fatal("discovered key was treated as trusted")
	}
}
func TestResolveRejectsNonLocalOrShellInput(t *testing.T) {
	for _, s := range []string{"-evil.local", "example.com", "name.local;whoami", "name.local\n", "127.0.0.1", "../../host.local"} {
		if _, e := ResolveLocal(context.Background(), s); e == nil {
			t.Fatal("invalid .local name accepted", s)
		}
	}
}
func TestBoundedDiscoveryOutput(t *testing.T) {
	b := &boundedOutput{limit: 8}
	n, e := b.Write([]byte("0123456789012"))
	if e != nil || n != 13 || b.Len() != 8 || !b.overflow {
		t.Fatal("output bound was not enforced")
	}
}
