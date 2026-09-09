// Package discovery uses the host's Avahi service for link-local discovery.
// Discovery records are hints only, never certificate or enrollment authority.
package discovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Candidate struct {
	Name        string `json:"name"`
	Address     string `json:"address"`
	URL         string `json:"url"`
	Fingerprint string `json:"unverifiedFingerprint"`
	Verified    bool   `json:"verified"`
}

var escape = regexp.MustCompile(`\\([0-9]{3})`)
var urlTXT = regexp.MustCompile(`(?:^|[" ])url=(https://[^" ]+)`)
var pinTXT = regexp.MustCompile(`(?:^|[" ])fingerprint=([a-f0-9]{64})(?:[" ]|$)`)

func unescape(s string) string {
	return escape.ReplaceAllStringFunc(s, func(v string) string {
		n, e := strconv.Atoi(v[1:])
		if e != nil || n < 32 || n > 126 {
			return "?"
		}
		return string(byte(n))
	})
}
func Parse(output string) []Candidate {
	found := map[string]Candidate{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(line, ";")
		if len(fields) < 10 || fields[0] != "=" || fields[2] != "IPv4" || fields[4] != "_runnerloom._tcp" {
			continue
		}
		addr := net.ParseIP(fields[7])
		port, e := strconv.Atoi(fields[8])
		if addr == nil || port < 1 || port > 65535 || e != nil {
			continue
		}
		txt := strings.Join(fields[9:], ";")
		um := urlTXT.FindStringSubmatch(txt)
		pm := pinTXT.FindStringSubmatch(txt)
		if len(um) != 2 || len(pm) != 2 {
			continue
		}
		u, e := url.Parse(um[1])
		if e != nil || u.Scheme != "https" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Hostname() == "" {
			continue
		}
		found[um[1]] = Candidate{Name: unescape(fields[3]), Address: fields[7], URL: um[1], Fingerprint: pm[1], Verified: false}
	}
	out := []Candidate{}
	for _, v := range found {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URL < out[j].URL })
	return out
}
func Discover(ctx context.Context, timeout time.Duration) ([]Candidate, error) {
	if timeout < time.Second || timeout > 30*time.Second {
		return nil, errors.New("discovery timeout must be 1..30 seconds")
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if _, e := exec.LookPath("/usr/bin/avahi-browse"); e != nil {
		return nil, errors.New("LAN discovery requires the optional avahi-utils package; invitation URLs work without discovery")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/avahi-browse", "--resolve", "--terminate", "--parsable", "_runnerloom._tcp")
	cmd.WaitDelay = time.Second
	out := &boundedOutput{limit: 1 << 20}
	cmd.Stdout = out
	e := cmd.Run()
	return discoveryResult(ctx, out, e)
}

func discoveryResult(ctx context.Context, out *boundedOutput, commandErr error) ([]Candidate, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	b := out.Bytes()
	if out.overflow {
		return nil, errors.New("discovery response exceeded limit")
	}
	if commandErr != nil {
		return nil, errors.New("Avahi discovery failed; check avahi-daemon or use an invitation URL")
	}
	return Parse(string(b)), nil
}
func Publish(ctx context.Context, cluster, endpoint, pin string) (func(), error) {
	if !regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`).MatchString(cluster) || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(pin) {
		return nil, errors.New("invalid discovery identity")
	}
	u, e := url.Parse(endpoint)
	if e != nil || u.Scheme != "https" || u.User != nil || u.Path != "" || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || len(endpoint) > 240 {
		return nil, errors.New("invalid advertised URL")
	}
	hostname := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if hostname == "localhost" || strings.HasSuffix(hostname, ".localhost") {
		return nil, errors.New("LAN discovery requires a LAN-reachable controller address")
	}
	if ip := net.ParseIP(hostname); ip != nil && ip.IsLoopback() {
		return nil, errors.New("LAN discovery requires a LAN-reachable controller address")
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	number, e := strconv.Atoi(port)
	if e != nil || number < 1 || number > 65535 {
		return nil, errors.New("invalid discovery port")
	}
	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, "/usr/bin/avahi-publish-service", "RunnerLoom "+cluster, "_runnerloom._tcp", port, "url="+endpoint, "fingerprint="+pin)
	cmd.WaitDelay = time.Second
	if e = cmd.Start(); e != nil {
		cancel()
		return nil, fmt.Errorf("cannot publish optional LAN discovery: %w", e)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	return func() { cancel(); <-done }, nil
}

type boundedOutput struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.Len()
	if remaining > 0 {
		_, _ = b.Buffer.Write(p[:min(remaining, n)])
	}
	if n > remaining {
		b.overflow = true
	}
	return n, nil
}

// ResolveLocal returns a transport locator only; callers must retain the original
// hostname and invitation CA in TLS verification. A forged mDNS record cannot
// authenticate a server with a different key.
func ResolveLocal(ctx context.Context, hostname string) (string, error) {
	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]{0,250}\.local$`).MatchString(hostname) {
		return "", errors.New("invalid .local hostname")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/avahi-resolve-host-name", "-4", "--", hostname)
	cmd.WaitDelay = time.Second
	out := &boundedOutput{limit: 4096}
	cmd.Stdout = out
	if e := cmd.Run(); e != nil {
		return "", errors.New("cannot resolve controller .local address; ensure Avahi is available or use its LAN IP")
	}
	if out.overflow {
		return "", errors.New("resolution output too large")
	}
	fields := strings.Fields(out.String())
	if len(fields) != 2 || !strings.EqualFold(fields[0], hostname) || net.ParseIP(fields[1]) == nil {
		return "", errors.New("invalid Avahi address response")
	}
	return fields[1], nil
}
