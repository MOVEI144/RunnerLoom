package github

import (
	"errors"
	"net/http"
	"time"

	"github.com/hashicorp/go-retryablehttp"
)

var errGitHubRedirect = errors.New("GitHub API redirects are forbidden")

// rejectGitHubRedirect prevents any credential-bearing request from following a
// redirect, including same-host HTTPS-to-HTTP redirects that retain Authorization.
func rejectGitHubRedirect(*http.Request, []*http.Request) error {
	// Do not use http.ErrUseLastResponse: the SDK wraps retryablehttp in another
	// http.Client, which could otherwise follow the returned redirect itself.
	return errGitHubRedirect
}

// newScaleSetHTTPClient preserves the SDK's retry bounds and TLS verification
// while rejecting redirects. Each SDK client and listener session receives a
// fresh instance because the SDK mutates its retry policy during authentication.
func newScaleSetHTTPClient() *retryablehttp.Client {
	c := retryablehttp.NewClient()
	c.RetryMax = 4
	c.RetryWaitMax = 30 * time.Second
	c.HTTPClient.CheckRedirect = rejectGitHubRedirect
	return c
}
