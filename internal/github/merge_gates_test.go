package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MOVEI144/RunnerLoom/internal/core"
)

type policyTransport func(*http.Request) (*http.Response, error)

func (f policyTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRepositoryAuthorizationEscapesPathComponents(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("TEST_ONLY_TOKEN"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"ORG/name#suffix", "ORG/name?suffix", "OR?G/na#me", "ORG/normal"} {
		t.Run(repo, func(t *testing.T) {
			calls := 0
			client := newAuth(Credentials{TokenFile: tokenFile})
			client.HTTP.Transport = policyTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				owner, name, _ := strings.Cut(repo, "/")
				want := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
				if req.URL.EscapedPath() != want || req.URL.RawQuery != "" || req.URL.Fragment != "" {
					t.Fatalf("wrong authorization target: %s, want %s", req.URL, want)
				}
				payload, _ := json.Marshal(map[string]any{"private": true, "full_name": repo})
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(payload)))}, nil
			})
			conf := core.Example()
			conf.GitHub.URL = "https://github.com/ORG/repository"
			conf.GitHub.AllowedRepositories = []string{repo}
			if err := client.CheckAccess(context.Background(), conf); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatal("repository lookup not performed", calls)
			}
		})
	}
}
