package bench

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
)

func testJWT(exp int64) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":` + strconv.FormatInt(exp, 10) + `}`))
	return "h." + payload + ".s"
}

func TestTokenCovers(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	w := Workload{Concurrency: []int{1, 4}, Warmup: time.Minute, Duration: 10 * time.Minute}
	r := &Runner{w: w, levelsLeft: 2, tokenExp: now.Add(15 * time.Minute)}
	if err := r.tokenCovers(now); err == nil {
		t.Fatal("15m cannot cover 2×11m")
	}
	r.levelsLeft = 1
	if err := r.tokenCovers(now); err != nil {
		t.Fatalf("15m covers the last 11m level: %v", err)
	}
	if err := r.tokenCovers(now.Add(5 * time.Minute)); err == nil {
		t.Fatal("10m left cannot cover an 11m level")
	}
	// 12m15s covers an 11m level plus drain and skew, but not a session
	// level's post-window ref cleanup on top.
	r.tokenExp = now.Add(12*time.Minute + 15*time.Second)
	if err := r.tokenCovers(now); err != nil {
		t.Fatalf("12m15s covers a branch level plus drain and skew: %v", err)
	}
	r.w.Strategy = "session"
	if err := r.tokenCovers(now); err == nil {
		t.Fatal("12m15s cannot also cover session cleanup")
	}
	r.tokenExp = time.Time{}
	if err := r.tokenCovers(now); err != nil {
		t.Fatalf("an opaque secret must pass: %v", err)
	}
}

// Discovery spends token lifetime: a token that covers the sweep at
// startup but runs out during the probe must fail NewRunner.
func TestNewRunnerChecksTokenAfterDiscovery(t *testing.T) {
	exp := time.Now().Unix() + 1 + int64(tokenSkew/time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Until(time.Unix(exp, 0).Add(-tokenSkew + 50*time.Millisecond)))
		_, _ = w.Write([]byte("0000"))
	}))
	defer srv.Close()
	tgt := Target{Remote: srv.URL, TokenURL: "x", Jurisdiction: "j", Repos: []string{"o/r"}, Secret: testJWT(exp)}
	w := Workload{Strategy: "branch", Concurrency: []int{1}, Duration: time.Millisecond,
		Commit: CommitConfig{FilesMin: 1, FilesMax: 1, FileSize: 1}}
	_, err := NewRunner(context.Background(), tgt, w, nil)
	if err == nil || !strings.Contains(err.Error(), "token expires") {
		t.Fatalf("a token expired during discovery must fail setup, got %v", err)
	}
}

// A generic forge's JWT-shaped secret is not Entire's account token:
// its exp must not gate the run.
func TestNewRunnerIgnoresGenericTokenExpiry(t *testing.T) {
	tgt := Target{Remote: "https://git.example", Repos: []string{"o/r"}, Secret: testJWT(time.Now().Add(-time.Hour).Unix())}
	w := Workload{Strategy: "branch", Concurrency: []int{1}, Duration: time.Minute,
		Commit: CommitConfig{FilesMin: 1, FilesMax: 1, FileSize: 1}}
	r, err := NewRunner(context.Background(), tgt, w, nil)
	if err != nil {
		t.Fatalf("a generic target must ignore JWT exp: %v", err)
	}
	r.Close()
}

// Setup, the barrier and the last level's cleanup spend token lifetime
// too: RunLevel rechecks before opening its window.
func TestRunLevelRechecksToken(t *testing.T) {
	r := &Runner{
		target:     Target{Repos: []string{"o/r"}},
		w:          Workload{Strategy: "branch", Concurrency: []int{1}, Duration: time.Minute, Commit: CommitConfig{FilesMin: 1, FilesMax: 1, FileSize: 1}},
		creds:      staticCreds{username: "token", password: "secret"},
		ep:         &endpoint{nodes: []string{"http://127.0.0.1:1"}, objFmt: formatcfg.SHA1},
		httpc:      http.DefaultClient,
		tokenExp:   time.Now().Add(30 * time.Second),
		levelsLeft: 1,
	}
	_, err := r.RunLevel(context.Background(), 1, nil)
	if err == nil || !strings.Contains(err.Error(), "token expires") {
		t.Fatalf("a level the token cannot cover must not start, got %v", err)
	}
}

// net/http reads proxy env once per process (envProxyOnce), so any
// earlier test request pins it; assert the policy, not the env.
func TestNewHTTPClientUsesHTTPSProxy(t *testing.T) {
	client := newHTTPClient(false, 1)
	ua, ok := client.Transport.(*uaTransport)
	if !ok {
		t.Fatalf("newHTTPClient transport = %T, want *uaTransport", client.Transport)
	}
	if client.Timeout != 0 {
		// A nonzero Client.Timeout on a wrapped RoundTripper falls off
		// net/http's known-transport fast path (goroutine per request);
		// uaTransport owns the timeout instead.
		t.Fatalf("Client.Timeout = %v, want 0 (uaTransport enforces the timeout)", client.Timeout)
	}
	tr := ua.base
	if tr.Proxy == nil || reflect.ValueOf(tr.Proxy).Pointer() != reflect.ValueOf(http.ProxyFromEnvironment).Pointer() {
		t.Fatal("newHTTPClient transport must use http.ProxyFromEnvironment")
	}
}

func TestUATransportTagsUserAgent(t *testing.T) {
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

func TestUATransportEnforcesTimeout(t *testing.T) {
	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked
	}))
	defer srv.Close()
	defer close(blocked)

	client := newHTTPClient(false, 1)
	client.Transport.(*uaTransport).timeout = 50 * time.Millisecond

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("request against a stalled server succeeded, want deadline error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
}

// Agents stop early when the next push would overrun the window, but the
// level must still span its configured window: the server closes the live
// series when RunLevel returns, and summarize divides by the full duration.
func TestRunLevelSpansWindowWhenAgentsStopEarly(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		http.Error(w, "slow", http.StatusInternalServerError)
	}))
	defer srv.Close()
	r := &Runner{
		target:     Target{Repos: []string{"o/r"}},
		w:          Workload{Strategy: "branch", Concurrency: []int{1}, Duration: 500 * time.Millisecond, Commit: CommitConfig{FilesMin: 1, FilesMax: 1, FileSize: 1}},
		creds:      staticCreds{username: "token", password: "secret"},
		ep:         &endpoint{nodes: []string{srv.URL}, objFmt: formatcfg.SHA1},
		httpc:      srv.Client(),
		levelsLeft: 1,
	}
	start := time.Now()
	res, err := r.RunLevel(context.Background(), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Pushes != 1 {
		t.Fatalf("pushes = %d, want 1 (the second would overrun the window)", res.Pushes)
	}
	if elapsed := time.Since(start); elapsed < r.w.Duration {
		t.Fatalf("RunLevel returned after %s, before the %s window ended", elapsed, r.w.Duration)
	}
}

func TestRunLevelReportsCancelledOps(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(300 * time.Millisecond):
			http.Error(w, "slow", http.StatusInternalServerError)
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	r := &Runner{
		target:     Target{Repos: []string{"o/r"}},
		w:          Workload{Strategy: "branch", Concurrency: []int{2}, Duration: 10 * time.Second, Commit: CommitConfig{FilesMin: 1, FilesMax: 1, FileSize: 1}},
		creds:      staticCreds{username: "token", password: "secret"},
		ep:         &endpoint{nodes: []string{srv.URL}, objFmt: formatcfg.SHA1},
		httpc:      srv.Client(),
		levelsLeft: 1,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	res, err := r.RunLevel(ctx, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Cancelled != 2 {
		t.Fatalf("Cancelled = %d, want both agents' in-flight pushes", res.Cancelled)
	}
}
