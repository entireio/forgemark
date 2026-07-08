package main

import (
	"net/http"
	"net/url"
	"reflect"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
)

func TestExpandRepos(t *testing.T) {
	tests := []struct {
		name     string
		reposCSV string
		pattern  string
		count    int
		want     []string
		wantErr  bool
	}{
		{name: "repos passthrough", reposCSV: "a/b,c/d", want: []string{"a/b", "c/d"}},
		{name: "pattern expands 1..N", pattern: "org/bench-{n}", count: 3,
			want: []string{"org/bench-1", "org/bench-2", "org/bench-3"}},
		{name: "pattern repeats {n}", pattern: "grp-{n}/bench-{n}", count: 2,
			want: []string{"grp-1/bench-1", "grp-2/bench-2"}},
		{name: "pattern keeps verbatim suffix", pattern: "you/bench-{n}.git", count: 2,
			want: []string{"you/bench-1.git", "you/bench-2.git"}},
		{name: "missing {n}", pattern: "bench-", count: 3, wantErr: true},
		{name: "pattern without count", pattern: "bench-{n}", count: 0, wantErr: true},
		{name: "count without pattern", count: 3, wantErr: true},
		{name: "repos plus count", reposCSV: "a/b", count: 3, wantErr: true},
		{name: "both repos and pattern", reposCSV: "a/b", pattern: "c-{n}", count: 2, wantErr: true},
		{name: "neither", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := expandRepos(tt.reposCSV, tt.pattern, tt.count)
			if (err != nil) != tt.wantErr {
				t.Fatalf("expandRepos(%q,%q,%d) err = %v, wantErr %v", tt.reposCSV, tt.pattern, tt.count, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("expandRepos(%q,%q,%d) = %v, want %v", tt.reposCSV, tt.pattern, tt.count, got, tt.want)
			}
		})
	}
}

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

func TestDestRef(t *testing.T) {
	// The prefix is prepended verbatim before the run ID; the assembled ref is what
	// parseFlags validates. Readable prefixes pass; typo shapes git rejects fail fast
	// (before any push pollutes the measured window).
	tests := []struct {
		name      string
		prefix    string
		c, i      int
		want      string
		wantValid bool
	}{
		{name: "no prefix", prefix: "", c: 32, i: 5, want: "refs/heads/fmX-c32-a5", wantValid: true},
		{name: "slash namespace", prefix: "bench/", c: 32, i: 5, want: "refs/heads/bench/fmX-c32-a5", wantValid: true},
		{name: "dash separator", prefix: "bench-", c: 8, i: 0, want: "refs/heads/bench-fmX-c8-a0", wantValid: true},
		{name: "no separator", prefix: "bench", c: 1, i: 0, want: "refs/heads/benchfmX-c1-a0", wantValid: true}, // ugly-but-valid
		{name: "space", prefix: "my bench/", c: 1, i: 0, want: "refs/heads/my bench/fmX-c1-a0", wantValid: false},
		{name: "tilde", prefix: "bench~1", c: 1, i: 0, want: "refs/heads/bench~1fmX-c1-a0", wantValid: false},
		{name: "leading slash", prefix: "/bench", c: 1, i: 0, want: "refs/heads//benchfmX-c1-a0", wantValid: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := destRef(tt.prefix, "fmX", tt.c, tt.i)
			if got != tt.want {
				t.Errorf("destRef(%q, ...) = %q, want %q", tt.prefix, got, tt.want)
			}
			if valid := plumbing.ReferenceName(got).Validate() == nil; valid != tt.wantValid {
				t.Errorf("Validate(%q) valid = %v, want %v", got, valid, tt.wantValid)
			}
		})
	}
}
