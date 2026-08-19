package bench

import (
	"net/http"
	"net/url"
	"os"
	"testing"

	"github.com/go-git/go-git/v6/plumbing/protocol/capability"
)

func TestNewHTTPClientUsesHTTPSProxy(t *testing.T) {
	proxyURL := "http://proxy.example:8080"
	t.Setenv("HTTPS_PROXY", proxyURL)
	t.Setenv("https_proxy", "")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	client := newHTTPClient(false, 1)
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		// Must stay a bare *http.Transport: wrappers knock Client.Timeout off
		// net/http's known-transport fast path (goroutine per request).
		t.Fatalf("newHTTPClient transport = %T, want *http.Transport", client.Transport)
	}
	if tr.Proxy == nil {
		t.Fatal("newHTTPClient transport Proxy is nil")
	}

	req := &http.Request{URL: &url.URL{Scheme: "https", Host: "git.example"}}
	got, err := tr.Proxy(req)
	if err != nil {
		t.Fatalf("Proxy returned error: %v", err)
	}
	if got == nil || got.String() != proxyURL {
		t.Fatalf("Proxy returned %v, want %s", got, proxyURL)
	}
}

func TestUATokenSeedsGoGitAgentExtra(t *testing.T) {
	// package init seeds the env var go-git appends to its agent string,
	// tagging all git traffic; an operator's pre-set value must win instead.
	if got := os.Getenv("GO_GIT_USER_AGENT_EXTRA"); got != uaToken {
		t.Skipf("GO_GIT_USER_AGENT_EXTRA preset to %q by the environment; init defers to it", got)
	}
	if got := capability.DefaultAgent(); got != "go-git/6.x "+uaToken {
		t.Fatalf("go-git DefaultAgent() = %q, want %q", got, "go-git/6.x "+uaToken)
	}
}
