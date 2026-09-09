package host

import (
	"context"
	"net/netip"
	"strings"
	"testing"
)

func TestSubnetSuggestionChecksRoutesWithoutModifyingHost(t *testing.T) {
	f := &fakeHost{Routes: `[{"dst":"default"},{"dst":"172.30.240.91"},{"dst":"172.30.241.0/24"}]`}
	v, e := SuggestSubnet(context.Background(), f)
	if e != nil || v != "172.30.242.0/24" {
		t.Fatal(v, e)
	}
	if len(f.Commands) != 1 || f.Commands[0] != "ip -j -4 route show table all" {
		t.Fatal("suggestion modified host", f.Commands)
	}
}

func TestSubnetSuggestionReportsMalformedRoute(t *testing.T) {
	f := &fakeHost{Routes: `[{"dst":"not-a-route"}]`}
	_, err := SuggestSubnet(t.Context(), f)
	if err == nil || !strings.Contains(err.Error(), `host route "not-a-route"`) {
		t.Fatalf("malformed route was not identified: %v", err)
	}
}

func TestSubnetSuggestionFailsClosedOnExhaustion(t *testing.T) {
	_, e := availableSubnet([]netip.Prefix{netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("10.0.0.0/8")})
	if e == nil {
		t.Fatal("overlap accepted")
	}
}
