package server

import (
	"context"
	"strings"
	"testing"
)

func f64(v float64) *float64 { return &v }
func i(v int) *int           { return &v }

// A client that vanishes during the POST (curl timeout, a script's Ctrl-C —
// most plausibly during the up-to-15s CLI credential resolution) must not have
// a run registered on its behalf: the sweep would write to remotes for hours
// while the requester never learned its run ID.
func TestStartRejectsAbandonedRequest(t *testing.T) {
	m := newRunManager(t.TempDir())
	defer m.beginShutdown()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the requester is already gone
	_, _, err := m.start(ctx, startRequest{
		ConfirmAuthorized: true,
		Targets:           []TargetSpec{{Remote: "demo://x"}},
	})
	if err == nil || !strings.Contains(err.Error(), "abandoned") {
		t.Fatalf("start(cancelled request ctx) = %v, want abandoned-request error", err)
	}
	if runs := m.list(); len(runs) != 0 {
		t.Fatalf("run registered despite abandoned request: %d runs", len(runs))
	}
}

// Absent (nil) fields take CLI defaults; explicit values — including
// meaningful zeros — pass through untouched.
func TestWithDefaultsAbsentVsExplicitZero(t *testing.T) {
	got := WorkloadSpec{}.withDefaults()
	if *got.WarmupSec != 10 || *got.DurationSec != 60 || *got.FilesMin != 1 || *got.CloneDepth != 1 {
		t.Fatalf("absent fields should take CLI defaults, got %+v", got)
	}
	if len(got.Concurrency) != 4 {
		t.Fatalf("absent concurrency should default to the CLI sweep, got %v", got.Concurrency)
	}

	got = WorkloadSpec{Strategy: "clone", WarmupSec: f64(0), CloneDepth: i(0)}.withDefaults()
	if *got.WarmupSec != 0 {
		t.Fatalf("explicit warmup 0 (no warm-up) was rewritten to %v", *got.WarmupSec)
	}
	if *got.CloneDepth != 0 {
		t.Fatalf("explicit clone_depth 0 (full history) was rewritten to %v", *got.CloneDepth)
	}

	// Explicit empty concurrency stays empty so Validate rejects it — it must
	// NOT silently become the default [1,8,32,128] sweep.
	got = WorkloadSpec{Concurrency: []int{}}.withDefaults()
	if len(got.Concurrency) != 0 {
		t.Fatalf("explicit empty concurrency was defaulted to %v", got.Concurrency)
	}

	// Explicit invalid zeros must reach Validate (and fail there), matching
	// the CLI's hard errors, not be silently defaulted.
	got = WorkloadSpec{FilesMin: i(0), FilesMax: i(0)}.withDefaults()
	if err := got.toWorkload("fmX").Validate(); err == nil {
		t.Fatal("explicit files_min=0 should fail Validate like the CLI")
	}
}

// The git-credential helper reply carries sibling keys (password_expiry_utc,
// oauth_refresh_token) that must not be mistaken for the password.
func TestParseGitCredential(t *testing.T) {
	out := "capability[]=authtype\nusername=oauth2\npassword=glpat-realsecret\npassword_expiry_utc=1784831031\noauth_refresh_token=deadbeef\n"
	user, pass, err := parseGitCredential(out, "glab")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if user != "oauth2" || pass != "glpat-realsecret" {
		t.Fatalf("got user=%q pass=%q, want oauth2 / glpat-realsecret", user, pass)
	}
	if _, _, err := parseGitCredential("username=x\npassword_expiry_utc=1\n", "glab"); err == nil {
		t.Fatal("a reply with no password= line should error, not accept a sibling key")
	}
}

func TestCredHost(t *testing.T) {
	for remote, want := range map[string]string{
		"https://gitlab.com":             "gitlab.com",
		"https://gitlab.example.com/foo": "gitlab.example.com",
		"":                               "gitlab.com", // bare "glab" source
	} {
		if got := credHost(remote); got != want {
			t.Errorf("credHost(%q) = %q, want %q", remote, got, want)
		}
	}
}

// An empty host means wildcard bind in a listen address — the one case the
// serve warning must fire for — so it is not loopback.
func TestIsLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": true, "127.0.0.1": true, "::1": true,
		"": false, "0.0.0.0": false, "192.168.1.10": false, "example.com": false,
	} {
		if got := IsLoopbackHost(host); got != want {
			t.Errorf("IsLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}
