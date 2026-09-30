package bench

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRefuseRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/x", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	_, err := newEntireEndpoint(context.Background(), srv.URL, "sha1", "o/r",
		staticCreds{username: "token", password: "secret"}, refuseRedirects(srv.Client()))
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("probe must refuse redirects, got %v", err)
	}
	// Pushes and cleanup share the client and must refuse outright.
	resp, err := refuseRedirects(srv.Client()).Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("client must refuse redirects")
	}
}

func TestDiscoveryFollowsReplicaRedirect(t *testing.T) {
	var gotUser, gotPass string
	replica := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, _ = r.BasicAuth()
		w.Header().Set("X-Entire-Replicas", "http://"+r.Host)
		_, _ = w.Write([]byte("0000 object-format=sha256"))
	}))
	defer replica.Close()
	// Mirror HandleInfoRefsDiscovery via the ALB: 307 to a hosting replica,
	// with the caller's token embedded in Location.
	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Entire-Replicas", replica.URL)
		loc := strings.Replace(replica.URL, "http://", "http://x-token:secret@", 1) + r.URL.RequestURI()
		http.Redirect(w, r, loc, http.StatusTemporaryRedirect)
	}))
	defer entry.Close()

	ep, err := newEntireEndpoint(context.Background(), entry.URL, "auto", "o/r",
		staticCreds{username: "token", password: "secret"}, refuseRedirects(entry.Client()))
	if err != nil {
		t.Fatalf("ALB-style discovery must succeed: %v", err)
	}
	if len(ep.nodes) != 1 || ep.nodes[0] != replica.URL {
		t.Fatalf("nodes = %v, want [%s]", ep.nodes, replica.URL)
	}
	if ep.objFmt != "sha256" {
		t.Fatalf("object format = %s, want the replica's sha256", ep.objFmt)
	}
	if gotUser != "token" || gotPass != "secret" {
		t.Fatalf("replica got %q:%q, want the probe's own credential", gotUser, gotPass)
	}
}

func TestDiscoveryRefusesUnadvertisedRedirect(t *testing.T) {
	hit := false
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer other.Close()
	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Entire-Replicas", "http://"+r.Host)
		http.Redirect(w, r, other.URL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	defer entry.Close()

	_, err := newEntireEndpoint(context.Background(), entry.URL, "sha1", "o/r",
		staticCreds{username: "token", password: "secret"}, refuseRedirects(entry.Client()))
	if err == nil || !strings.Contains(err.Error(), "not an advertised replica") {
		t.Fatalf("redirect off the replica set must be refused, got %v", err)
	}
	if hit {
		t.Fatal("the credential reached an unadvertised host")
	}
}

func TestVerbatimURLFor(t *testing.T) {
	tests := []struct {
		name string
		node string
		repo string
		want string
	}{
		{name: "full multi-segment path passed through, not inserted", node: "https://n/", repo: "pfx/acme/backend", want: "https://n/pfx/acme/backend"},
		{name: "generic keeps .git as given", node: "https://gh", repo: "you/x.git", want: "https://gh/you/x.git"},
		{name: "generic bare path (no forced .git)", node: "https://gh", repo: "you/x", want: "https://gh/you/x"},
		{name: "surrounding slashes trimmed", node: "https://n", repo: "/pfx/x/", want: "https://n/pfx/x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := verbatimURLFor(tt.node, tt.repo); got != tt.want {
				t.Fatalf("verbatimURLFor(%q, %q) = %q, want %q", tt.node, tt.repo, got, tt.want)
			}
		})
	}
}

func TestPinReplicas(t *testing.T) {
	const base = "https://aws-us-east-2.entire.io"
	ok := []string{
		"https://aws-us-east-2.entire.io",
		"https://node-1.aws-us-east-2.entire.io",
		"https://aws-us-east-2-node-1.entire.io:443",
		"https://NODE-2.AWS-US-EAST-2.ENTIRE.IO",
	}
	got, err := pinReplicas(base, ok)
	if err != nil {
		t.Fatalf("same-domain replicas must be admitted: %v", err)
	}
	if len(got) != len(ok) {
		t.Fatalf("got %d nodes, want %d", len(got), len(ok))
	}
	for _, bad := range []string{
		"https://evil.example",
		"https://entire.io.evil.example",
		"http://node-1.aws-us-east-2.entire.io",
		"https://node-1.aws-us-east-2.entire.io:8443",
		"https://user:pw@node-1.aws-us-east-2.entire.io",
		"https://node-1.aws-us-east-2.entire.io/other",
		"https://node-1.aws-us-east-2.entire.io/?x=1",
		"https://node-1.aws-us-east-2.entire.io/#f",
		"not a url",
	} {
		if _, err := pinReplicas(base, []string{bad}); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
	// A dev entry host with no parent domain admits only itself.
	if _, err := pinReplicas("http://127.0.0.1:8080", []string{"http://127.0.0.1:8081"}); err != nil {
		t.Errorf("same-host dev replica must be admitted: %v", err)
	}
	if _, err := pinReplicas("http://localhost:8080", []string{"http://other:8080"}); err == nil {
		t.Error("a different dev host must be refused")
	}
}
