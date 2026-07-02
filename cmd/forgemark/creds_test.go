package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func newTestJurisdictionCreds(t *testing.T, calls *atomic.Int32) *jurisdictionCreds {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = fmt.Fprint(w, `{"access_token":"fresh","expires_in":899}`)
	}))
	t.Cleanup(srv.Close)
	return newJurisdictionCreds(srv.Client(), srv.URL, "https://us.example", "entire-cli", "subject", "token")
}

func TestJurisdictionCredsCachesValidToken(t *testing.T) {
	var calls atomic.Int32
	j := newTestJurisdictionCreds(t, &calls)
	j.tok = tokenEntry{token: "cached", exp: time.Now().Add(10 * time.Minute)}

	got, err := j.get(context.Background())
	if err != nil || got != "cached" {
		t.Fatalf("get = %q, %v; want cached token with no exchange", got, err)
	}
	if calls.Load() != 0 {
		t.Fatalf("exchanged %d times; a comfortably-valid token must not trigger one", calls.Load())
	}
}

// Regression: an expired token must re-exchange synchronously AND must not leak
// the refreshing flag. Leaking it permanently disabled ahead-of-expiry refresh,
// reintroducing the synchronized refresh stall the design avoids.
func TestJurisdictionCredsExpiredReexchangesWithoutLeakingFlag(t *testing.T) {
	var calls atomic.Int32
	j := newTestJurisdictionCreds(t, &calls)
	j.tok = tokenEntry{token: "stale", exp: time.Now().Add(-time.Second)}

	got, err := j.get(context.Background())
	if err != nil || got != "fresh" {
		t.Fatalf("get = %q, %v; want a freshly exchanged token", got, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("exchanged %d times; want exactly 1 (blocking refresh)", calls.Load())
	}
	j.mu.Lock()
	leaked := j.refreshing
	j.mu.Unlock()
	if leaked {
		t.Fatal("refreshing flag leaked on the expired path → ahead-refresh permanently disabled")
	}
}

// basicAuth returns the exchanged token as the password (Entire ignores the
// username) for any repo — one jurisdiction token covers every repo.
func TestJurisdictionCredsBasicAuthUsesTokenAsPassword(t *testing.T) {
	var calls atomic.Int32
	j := newTestJurisdictionCreds(t, &calls)

	auth, err := j.basicAuth(context.Background(), "any/repo")
	if err != nil {
		t.Fatalf("basicAuth: %v", err)
	}
	if auth.Password != "fresh" || auth.Username != "token" {
		t.Fatalf("basicAuth = %q:%q, want token:fresh", auth.Username, auth.Password)
	}
}
