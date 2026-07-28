package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testSecret = "s3cr3t-token-must-never-leak"

func newTestServer(t *testing.T) (*Server, *httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	s := New("127.0.0.1:0", dir)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts, dir
}

func startDemoRun(t *testing.T, ts *httptest.Server, body string) string {
	t.Helper()
	res, err := http.Post(ts.URL+"/api/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var out struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("start run: HTTP %d: %s", res.StatusCode, out.Error)
	}
	return out.ID
}

// demoBody runs one short level against a demo target carrying a secret that
// the demo path never needs — perfect bait for redaction checks.
func demoBody(durationSec float64, levels string) string {
	return fmt.Sprintf(`{
		"confirm_authorized": true,
		"workload": {"strategy":"branch","concurrency":[%s],"duration_sec":%g,"warmup_sec":0.2},
		"targets": [{"name":"d1","remote":"demo://one?p50=10ms","secret":%q}]
	}`, levels, durationSec, testSecret)
}

// collectSSE reads the run's event stream until run_done (or the deadline),
// returning the ordered event names and the full raw text.
func collectSSE(t *testing.T, ts *httptest.Server, runID, lastEventID string) ([]string, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/runs/"+runID+"/events", nil)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()

	var names []string
	var raw strings.Builder
	done := false
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		raw.WriteString(line + "\n")
		if name, ok := strings.CutPrefix(line, "event: "); ok {
			names = append(names, name)
			done = name == "run_done"
		} else if done && strings.HasPrefix(line, "data: ") {
			break // run_done's payload read; the stream is complete
		}
	}
	return names, raw.String()
}

func TestStartRunValidation(t *testing.T) {
	_, ts, _ := newTestServer(t)

	post := func(body string) (int, string) {
		res, err := http.Post(ts.URL+"/api/runs", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		msg, _ := out["error"].(string)
		return res.StatusCode, msg
	}

	if code, msg := post(`{"targets":[{"remote":"demo://x"}]}`); code != http.StatusBadRequest || !strings.Contains(msg, "confirm_authorized") {
		t.Fatalf("unconfirmed POST = %d %q, want 400 confirm_authorized", code, msg)
	}
	if code, msg := post(`{"confirm_authorized":true,"targets":[]}`); code != http.StatusBadRequest || !strings.Contains(msg, "target") {
		t.Fatalf("no-targets POST = %d %q, want 400", code, msg)
	}
	if code, msg := post(`{"confirm_authorized":true,"workload":{"strategy":"bogus"},"targets":[{"remote":"demo://x"}]}`); code != http.StatusBadRequest || !strings.Contains(msg, "strategy") {
		t.Fatalf("bad-strategy POST = %d %q, want 400", code, msg)
	}
	if code, msg := post(`{"confirm_authorized":true,"targets":[{"remote":"demo://x","secret":"tok","secret_source":"gh"}]}`); code != http.StatusBadRequest || !strings.Contains(msg, "not both") {
		t.Fatalf("secret+source POST = %d %q, want 400 not-both", code, msg)
	}
	if code, msg := post(`{"confirm_authorized":true,"targets":[{"remote":"demo://x","secret_source":"bogus"}]}`); code != http.StatusBadRequest || !strings.Contains(msg, "secret_source") {
		t.Fatalf("unknown-source POST = %d %q, want 400 secret_source", code, msg)
	}
	// Real (non-demo) targets are fast-failed at the request, not left to die
	// mid-run inside NewRunner. demo:// targets skip these checks.
	if code, msg := post(`{"confirm_authorized":true,"targets":[{"remote":"https://git.example","secret":"tok"}]}`); code != http.StatusBadRequest || !strings.Contains(msg, "no repos") {
		t.Fatalf("no-repos target = %d %q, want 400 no repos", code, msg)
	}
	if code, msg := post(`{"confirm_authorized":true,"targets":[{"remote":"https://git.example","repos":["a/b"]}]}`); code != http.StatusBadRequest || !strings.Contains(msg, "no credential") {
		t.Fatalf("no-credential target = %d %q, want 400 no credential", code, msg)
	}
	if code, msg := post(`{"confirm_authorized":true,"workload":{"strategy":"repo"},"targets":[{"remote":"https://git.example","repos":["a/b"],"secret":"t"}]}`); code != http.StatusBadRequest || !strings.Contains(msg, ">= 2 repos") {
		t.Fatalf("repo-strategy one-repo target = %d %q, want 400 >= 2 repos", code, msg)
	}
}

func TestRunLifecycleSSEAndRedaction(t *testing.T) {
	_, ts, dir := newTestServer(t)
	runID := startDemoRun(t, ts, demoBody(1.2, "1,2"))

	// A second run while one is active is a conflict.
	res, err := http.Post(ts.URL+"/api/runs", "application/json", strings.NewReader(demoBody(1, "1")))
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("concurrent start = HTTP %d, want 409", res.StatusCode)
	}

	names, raw := collectSSE(t, ts, runID, "")
	if len(names) == 0 || names[0] != "hello" {
		t.Fatalf("first event = %v, want hello", names)
	}
	if names[len(names)-1] != "run_done" {
		t.Fatalf("last event = %s, want run_done", names[len(names)-1])
	}
	count := func(name string) int {
		n := 0
		for _, x := range names {
			if x == name {
				n++
			}
		}
		return n
	}
	if count("level_start") != 2 || count("level_result") != 2 {
		t.Fatalf("level events = %v, want 2 level_start + 2 level_result", names)
	}
	if count("bucket") == 0 {
		t.Fatalf("no bucket events in %v", names)
	}
	if strings.Contains(raw, testSecret) {
		t.Fatal("secret leaked into the SSE stream")
	}

	// Redaction is structural: no API response may carry the secret.
	for _, path := range []string{"/api/runs", "/api/runs/" + runID, "/api/history"} {
		res, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b := make([]byte, 1<<20)
		n, _ := res.Body.Read(b)
		_ = res.Body.Close()
		if strings.Contains(string(b[:n]), testSecret) {
			t.Fatalf("secret leaked via GET %s", path)
		}
	}

	// The run persisted a format-2 doc into the results dir.
	files, _ := filepath.Glob(filepath.Join(dir, "forgemark-*.json"))
	if len(files) != 1 {
		t.Fatalf("results dir has %d docs, want 1", len(files))
	}
	if b, _ := os.ReadFile(files[0]); strings.Contains(string(b), testSecret) {
		t.Fatal("secret leaked into the persisted result doc")
	}

	// Replay: with Last-Event-ID at the end, nothing but nothing comes back;
	// from 0 the whole history replays instantly (run is finished).
	replayNames, _ := collectSSE(t, ts, runID, "0")
	if len(replayNames) != len(names) {
		t.Fatalf("full replay = %d events, want %d", len(replayNames), len(names))
	}
}

func TestCancelRun(t *testing.T) {
	_, ts, _ := newTestServer(t)
	runID := startDemoRun(t, ts, demoBody(30, "1")) // would run 30s if not cancelled

	time.Sleep(300 * time.Millisecond)
	res, err := http.Post(ts.URL+"/api/runs/"+runID+"/cancel", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()

	names, raw := collectSSE(t, ts, runID, "")
	if names[len(names)-1] != "run_done" {
		t.Fatalf("cancelled run events = %v, want trailing run_done", names)
	}
	if !strings.Contains(raw, `"state":"cancelled"`) {
		t.Fatal("run_done should carry state=cancelled")
	}
}

func TestForbiddenOrigin(t *testing.T) {
	_, ts, _ := newTestServer(t)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/runs", strings.NewReader(demoBody(1, "1")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin POST = HTTP %d, want 403", res.StatusCode)
	}
}

func TestHistoryServesLegacyDocs(t *testing.T) {
	_, ts, dir := newTestServer(t)
	legacy := `{
		"run_id": "fmlegacy", "target": "https://git.example", "strategy": "branch",
		"duration": "1m0s", "warmup": "10s", "repo_count": 1, "commit": "1-10 x 2048B",
		"levels": [{"concurrency": 4, "ok": 100, "ops_per_sec": 25.0, "p95_ms": 80}]
	}`
	if err := os.WriteFile(filepath.Join(dir, "forgemark-fmlegacy.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := http.Get(ts.URL + "/api/history/forgemark-fmlegacy.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var doc struct {
		Targets []struct {
			Name   string `json:"name"`
			Levels []struct {
				Concurrency int `json:"concurrency"`
			} `json:"levels"`
		} `json:"targets"`
	}
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Targets) != 1 || doc.Targets[0].Name != "https://git.example" || len(doc.Targets[0].Levels) != 1 {
		t.Fatalf("legacy doc not normalized: %+v", doc)
	}

	// Path traversal shapes are rejected before touching the filesystem.
	for _, bad := range []string{"..%2Fsecrets.json", "notforgemark.json"} {
		res, err := http.Get(ts.URL + "/api/history/" + bad)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode == http.StatusOK {
			t.Fatalf("GET history/%s succeeded, want rejection", bad)
		}
	}
}
