package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
)

// SuggestSubnet reads routing tables only. Apply must check again: a suggestion
// is not an allocation or permission to rewrite host networking.
func SuggestSubnet(ctx context.Context, executor Executor) (string, error) {
	b, e := executor.Run(ctx, "ip", []string{"-j", "-4", "route", "show", "table", "all"}, nil)
	if e != nil {
		return "", e
	}
	var routes []struct {
		Dst string `json:"dst"`
	}
	if e = json.Unmarshal(b, &routes); e != nil {
		return "", e
	}
	prefixes := []netip.Prefix{}
	for _, r := range routes {
		if r.Dst == "default" || r.Dst == "0.0.0.0/0" {
			continue
		}
		p, e := routePrefix(r.Dst)
		if e != nil {
			return "", errors.New("cannot safely interpret a host route")
		}
		prefixes = append(prefixes, p)
	}
	return availableSubnet(prefixes)
}
func availableSubnet(routes []netip.Prefix) (string, error) {
	for _, first := range []string{"172.30", "10.253", "10.252"} {
		for i := 0; i < 256; i++ {
			candidate := netip.MustParsePrefix(fmt.Sprintf("%s.%d.0/24", first, (240+i)%256))
			overlaps := false
			for _, p := range routes {
				if candidate.Overlaps(p) {
					overlaps = true
					break
				}
			}
			if !overlaps {
				return candidate.String(), nil
			}
		}
	}
	return "", errors.New("no conflict-free candidate; select a private /24 with the network administrator")
}
