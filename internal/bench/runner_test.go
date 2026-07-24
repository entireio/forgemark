package bench

import (
	"net/http"
	"net/url"
	"testing"
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
