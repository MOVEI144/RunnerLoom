package github

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MOVEI144/RunnerLoom/internal/core"
	"github.com/actions/scaleset"
)

// This is never automatically enabled in CI or for contributors. The operator
// must opt in with an existing owner-only credential file and an exact repo URL.
// It creates only a uniquely named temporary scale set and JIT runner, then
// removes both. It does not execute workflows or expose a long-lived token.
func TestLiveGitHubScaleSet(t *testing.T) {
	tokenFile := os.Getenv("RUNNERLOOM_LIVE_GITHUB_TOKEN_FILE")
	origin := os.Getenv("RUNNERLOOM_LIVE_GITHUB_URL")
	if tokenFile == "" || origin == "" {
		t.Skip("explicit live GitHub credentials not provided")
	}
	if !validLiveOrigin(origin) {
		t.Fatal("live GitHub URL must use HTTPS without userinfo, query or fragment")
	}
	b, e := core.ReadSecret(tokenFile)
	if e != nil {
		t.Fatal("cannot read live test credential")
	}
	token := strings.TrimSpace(string(b))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client, e := scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{GitHubConfigURL: origin, PersonalAccessToken: token, SystemInfo: scaleset.SystemInfo{System: "runnerloom", Version: "integration-test", Subsystem: "temporary-verification"}}, scaleset.WithRetryableHTTPClint(newScaleSetHTTPClient()))
	if e != nil {
		t.Fatal("live client initialization failed")
	}
	name := "rl-verify-" + core.ID()
	set, e := client.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{Name: name, RunnerGroupID: 1, Labels: []scaleset.Label{{Name: name, Type: "System"}}})
	if e != nil {
		t.Fatal("live scale-set creation failed: " + strings.ReplaceAll(e.Error(), token, "[REDACTED]"))
	}
	if set == nil || set.ID <= 0 {
		t.Fatal("live API returned no scale-set identity")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if e := client.DeleteRunnerScaleSet(ctx, set.ID); e != nil {
			t.Errorf("temporary scale-set cleanup failed, ID=%d", set.ID)
		} else {
			t.Logf("temporary scale set %d removed", set.ID)
		}
	})
	session, e := client.MessageSessionClient(ctx, set.ID, "runnerloom-verification", scaleset.WithRetryableHTTPClint(newScaleSetHTTPClient()))
	if e != nil {
		t.Fatal("live listener session creation failed")
	}
	if session.Session().Statistics == nil {
		t.Fatal("live session returned no statistics")
	}
	if e = session.Close(ctx); e != nil {
		t.Fatal("live listener session close failed")
	}
	jit, e := client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: "rl-" + core.ID(), WorkFolder: "_work"}, set.ID)
	if e != nil || jit == nil || jit.Runner == nil || jit.EncodedJITConfig == "" {
		t.Fatal("live JIT generation failed")
	}
	runnerID := int64(jit.Runner.ID)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		r, e := client.GetRunnerByName(ctx, jit.Runner.Name)
		if e == nil && r == nil {
			return
		}
		if e = client.RemoveRunner(ctx, runnerID); e != nil {
			t.Errorf("temporary runner cleanup failed, ID=%d", runnerID)
		}
	})
	found, e := client.GetRunnerByName(ctx, jit.Runner.Name)
	if e != nil || found == nil || found.ID != jit.Runner.ID || found.RunnerScaleSetID != set.ID {
		t.Fatal("live runner identity lookup mismatch")
	}
	if e = client.RemoveRunner(ctx, runnerID); e != nil {
		t.Fatal("live runner removal failed")
	}
	// Persist an explicitly nonsecret verification record for the development log.
	if path := os.Getenv("RUNNERLOOM_LIVE_EVIDENCE"); path != "" {
		if !filepath.IsAbs(path) {
			t.Fatal("evidence path must be absolute")
		}
		if e = core.WritePrivate(path, []byte("Live GitHub scale-set creation, listener session, JIT generation, identity lookup and runner removal passed. Cleanup is checked by test cleanup. No VM job was executed by this test.\n")); e != nil {
			t.Fatal(e)
		}
	}
	t.Log("LIVE: scale-set, listener, JIT, identity lookup and runner removal verified; no token or JIT printed")
}

func validLiveOrigin(origin string) bool {
	u, err := url.Parse(origin)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func TestLiveOriginRequiresHTTPS(t *testing.T) {
	for _, origin := range []string{"http://github.com/org", "ftp://github.com/org", "https://", "https://token@github.com/org", "https://github.com/org?x=y", "https://github.com/org#fragment"} {
		if validLiveOrigin(origin) {
			t.Fatalf("insecure live origin accepted: %q", origin)
		}
	}
	if !validLiveOrigin("https://github.com/organization") {
		t.Fatal("valid live origin refused")
	}
}
