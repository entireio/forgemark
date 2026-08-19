package bench

import (
	"net/http"
	"net/http/httptest"
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
	ua, ok := client.Transport.(*uaTransport)
	if !ok {
		t.Fatalf("newHTTPClient transport = %T, want *uaTransport", client.Transport)
	}
	tr := ua.base
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

func TestNewHTTPClientTagsUserAgent(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("User-Agent"))
	}))
	defer srv.Close()

	client := newHTTPClient(false, 1)

	// No User-Agent set (forgemark's direct token/probe requests).
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	// Pre-set User-Agent (go-git's transport sets its own).
	req, err = http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "go-git/6.x")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	want := []string{uaToken, "go-git/6.x " + uaToken}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("User-Agent headers = %q, want %q", got, want)
	}
}
