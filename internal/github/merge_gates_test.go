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

func TestRepositoryURLEnforcesOrgRunnerGroupWhenPresent(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("TEST_ONLY_TOKEN"), 0600); err != nil {
		t.Fatal(err)
	}
	client := newAuth(Credentials{TokenFile: tokenFile})
	client.HTTP.Transport = policyTransport(func(req *http.Request) (*http.Response, error) {
		if strings.HasPrefix(req.URL.Path, "/repos/") {
			payload, _ := json.Marshal(map[string]any{"private": true, "full_name": "ORG/repository"})
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(payload))), Header: make(http.Header)}, nil
		}
		if strings.HasPrefix(req.URL.Path, "/users/") {
			payload, _ := json.Marshal(map[string]any{"type": "Organization"})
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(payload))), Header: make(http.Header)}, nil
		}
		if strings.HasSuffix(req.URL.Path, "/actions/runner-groups/1") {
			payload, _ := json.Marshal(map[string]any{"visibility": "all", "allows_public_repositories": true})
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(payload))), Header: make(http.Header)}, nil
		}
		t.Fatalf("unexpected request %s", req.URL)
		return nil, nil
	})
	conf := core.Example()
	conf.GitHub.URL = "https://github.com/ORG/repository"
	conf.GitHub.AllowedRepositories = []string{"ORG/repository"}
	err := client.CheckAccess(context.Background(), conf)
	if err == nil || !strings.Contains(err.Error(), "RUNNER_GROUP_ACCESS") {
		t.Fatal("org runner group policy skipped for repository URL", err)
	}
}

func TestOrganizationRunnerGroup404FailsClosed(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("TEST_ONLY_TOKEN"), 0600); err != nil {
		t.Fatal(err)
	}
	client := newAuth(Credentials{TokenFile: tokenFile})
	client.HTTP.Transport = policyTransport(func(req *http.Request) (*http.Response, error) {
		if strings.HasPrefix(req.URL.Path, "/repos/") {
			payload, _ := json.Marshal(map[string]any{"private": true, "full_name": "ORG/repository"})
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(payload))), Header: make(http.Header)}, nil
		}
		if strings.HasPrefix(req.URL.Path, "/users/") {
			payload, _ := json.Marshal(map[string]any{"type": "Organization"})
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(payload))), Header: make(http.Header)}, nil
		}
		if strings.Contains(req.URL.Path, "/actions/runner-groups/") {
			return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
		}
		t.Fatalf("unexpected request %s", req.URL)
		return nil, nil
	})
	conf := core.Example()
	conf.GitHub.URL = "https://github.com/ORG/repository"
	conf.GitHub.AllowedRepositories = []string{"ORG/repository"}
	if err := client.CheckAccess(context.Background(), conf); err == nil {
		t.Fatal("organization runner-group 404 was treated as a personal account")
	}
}

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
				if strings.HasPrefix(req.URL.Path, "/users/") {
					payload, _ := json.Marshal(map[string]any{"type": "User"})
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(payload))), Header: make(http.Header)}, nil
				}
				if strings.Contains(req.URL.Path, "/actions/runner-groups/") {
					t.Fatal("personal account looked up an organization runner group")
				}
				owner, name, _ := strings.Cut(repo, "/")
				want := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
				if req.URL.EscapedPath() != want || req.URL.RawQuery != "" || req.URL.Fragment != "" {
					t.Fatalf("wrong authorization target: %s, want %s", req.URL, want)
				}
				payload, _ := json.Marshal(map[string]any{"private": true, "full_name": repo})
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(payload))), Header: make(http.Header)}, nil
			})
			conf := core.Example()
			conf.GitHub.URL = "https://github.com/ORG/repository"
			conf.GitHub.AllowedRepositories = []string{repo}
			if err := client.CheckAccess(context.Background(), conf); err != nil {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Fatal("repository lookup not performed", calls)
			}
		})
	}
}
