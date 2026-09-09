package github

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/golang-jwt/jwt/v4"
)

func TestScaleSetHTTPPolicyFreshAndBounded(t *testing.T) {
	a, b := newScaleSetHTTPClient(), newScaleSetHTTPClient()
	if a == b || a.HTTPClient == b.HTTPClient || a.HTTPClient.Transport == b.HTTPClient.Transport {
		t.Fatal("SDK clients and listener sessions must not share mutable retry clients")
	}
	for _, c := range []*http.Client{a.HTTPClient, b.HTTPClient} {
		if c.CheckRedirect == nil || !errors.Is(c.CheckRedirect(nil, nil), errGitHubRedirect) {
			t.Fatal("redirect policy does not fail closed")
		}
		tr, ok := c.Transport.(*http.Transport)
		if !ok || tr.TLSClientConfig != nil && tr.TLSClientConfig.InsecureSkipVerify {
			t.Fatal("TLS certificate verification was weakened")
		}
		c.CloseIdleConnections()
	}
	if a.RetryMax != 4 || a.RetryWaitMax != 30*time.Second {
		t.Fatal("SDK retry limits changed")
	}
	if a.CheckRetry == nil {
		t.Fatal("redirect retry policy is missing")
	}
	retry, err := a.CheckRetry(context.Background(), nil, &url.Error{
		Op:  "Get",
		URL: "https://api.github.com/redirect",
		Err: errGitHubRedirect,
	})
	if retry || !errors.Is(err, errGitHubRedirect) {
		t.Fatalf("rejected redirect was retried: retry=%t err=%v", retry, err)
	}
}

func TestScaleSetSDKRejectsCredentialRedirects(t *testing.T) {
	for _, stage := range []string{"registration", "admin", "scale-set"} {
		for _, status := range []int{301, 302, 303, 307, 308} {
			for _, encryptedTarget := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%d/target-tls=%t", stage, status, encryptedTarget), func(t *testing.T) {
					var received atomic.Int32
					h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						received.Add(1)
						w.WriteHeader(http.StatusBadRequest)
					})
					var target *httptest.Server
					if encryptedTarget {
						target = httptest.NewTLSServer(h)
					} else {
						target = httptest.NewServer(h)
					}
					t.Cleanup(target.Close)
					client, redirected := scaleSetHTTPFixture(t, stage, status, target)
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_, err := client.GetRunnerScaleSet(ctx, 1, "fixture")
					if err == nil || !strings.Contains(err.Error(), errGitHubRedirect.Error()) {
						t.Fatalf("expected redirect refusal, got %v", err)
					}
					if redirected.Load() != 1 {
						t.Fatal("test did not reach the credential-bearing redirect")
					}
					if received.Load() != 0 {
						t.Fatal("a request reached the redirect destination")
					}
				})
			}
		}
	}
}

func TestScaleSetSDKAllowsVerifiedHTTPSWithoutRedirect(t *testing.T) {
	client, _ := scaleSetHTTPFixture(t, "", 0, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	set, err := client.GetRunnerScaleSet(ctx, 1, "fixture")
	if err != nil || set == nil || set.ID != 7 {
		t.Fatalf("ordinary authenticated HTTPS failed: set=%v err=%v", set, err)
	}
}

// scaleSetHTTPFixture exercises the actual SDK registration, admin-token and
// scale-set request path using local TLS and synthetic credentials only.
func scaleSetHTTPFixture(t *testing.T, redirectStage string, status int, target *httptest.Server) (*scaleset.Client, *atomic.Int32) {
	t.Helper()
	const pat = "fixture-PAT-not-a-real-credential"
	const registration = "fixture-registration-token"
	admin, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}).SignedString([]byte("fixture-signing-key-not-a-real-secret"))
	if err != nil {
		t.Fatal(err)
	}
	var redirected atomic.Int32
	var origin *httptest.Server
	origin = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var stage, wantAuth string
		var body any
		code := http.StatusOK
		switch {
		case strings.HasSuffix(r.URL.Path, "/registration-token"):
			stage, wantAuth = "registration", "Bearer "+pat
			code = http.StatusCreated
			body = map[string]any{"token": registration, "expires_at": time.Now().Add(time.Hour)}
		case strings.HasSuffix(r.URL.Path, "/actions/runner-registration"):
			stage, wantAuth = "admin", "RemoteAuth "+registration
			body = map[string]any{"url": origin.URL + "/actions", "token": admin}
		case strings.HasSuffix(r.URL.Path, "/_apis/runtime/runnerscalesets"):
			stage, wantAuth = "scale-set", "Bearer "+admin
			body = map[string]any{"count": 1, "value": []any{map[string]any{"id": 7, "name": "fixture", "runnerGroupId": 1}}}
		default:
			t.Errorf("unexpected fixture request path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != wantAuth {
			t.Error("fixture request lacks the expected synthetic credential")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if stage == redirectStage {
			redirected.Add(1)
			http.Redirect(w, r, target.URL+"/sink", status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(origin.Close)
	roots := x509.NewCertPool()
	roots.AddCert(origin.Certificate())
	if target != nil && target.TLS != nil {
		roots.AddCert(target.Certificate())
	}
	retries := newScaleSetHTTPClient()
	// Avoid retry delays in adversarial fixtures; do not change redirect or TLS policy.
	retries.RetryMax = 0
	t.Cleanup(retries.HTTPClient.CloseIdleConnections)
	client, err := scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL: origin.URL + "/org/repo", PersonalAccessToken: pat,
	}, scaleset.WithRetryableHTTPClint(retries), scaleset.WithRootCAs(roots))
	if err != nil {
		t.Fatal(err)
	}
	return client, &redirected
}
