package discovery

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCanceledDiscoveryDoesNotReportPartialSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := &boundedOutput{limit: 4096}
	_, _ = out.Write([]byte(`=;eth0;IPv4;RunnerLoom;_runnerloom._tcp;local;controller.local;192.168.1.2;8443;"url=https://controller.local:8443" "fingerprint=` + strings.Repeat("a", 64) + `"`))
	if len(Parse(out.String())) != 1 {
		t.Fatal("partial-result fixture invalid")
	}
	got, err := discoveryResult(ctx, out, errors.New("terminated"))
	if !errors.Is(err, context.Canceled) || len(got) != 0 {
		t.Fatal("cancellation swallowed", got, err)
	}
	got, err = Discover(ctx, time.Second)
	if !errors.Is(err, context.Canceled) || len(got) != 0 {
		t.Fatal("already-canceled discovery started", got, err)
	}
}

func TestPublishRejectsLoopbackHostnamesBeforeAvahi(t *testing.T) {
	for _, endpoint := range []string{"https://localhost", "https://LOCALHOST.:8443", "https://controller.localhost", "https://127.0.0.1:8443", "https://[::1]:8443"} {
		stop, err := Publish(context.Background(), "home", endpoint, strings.Repeat("a", 64))
		if stop != nil {
			stop()
			t.Fatal("Avahi was started for loopback")
		}
		if err == nil || !strings.Contains(err.Error(), "LAN-reachable") {
			t.Fatalf("%s: %v", endpoint, err)
		}
	}
}
