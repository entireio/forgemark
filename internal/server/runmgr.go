package server

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/entireio/forgemark/internal/bench"
	"github.com/entireio/forgemark/internal/results"
)

// maxTargets bounds one comparison run. Eight matches the UI's fixed target
// palette, and more than eight concurrent load generators from one laptop
// would perturb each other anyway.
const maxTargets = 8

// keepFinishedRuns is how many completed runs stay resident (with their full
// event logs) for the UI session; older ones live on only as result docs.
const keepFinishedRuns = 20

// levelRunner is what the coordinator drives, one per target: bench.Runner for
// a real forge, demoRunner for the synthetic offline target.
type levelRunner interface {
	Label() string
	Nodes() int
	ObjectFormat() string
	RunLevel(ctx context.Context, concurrency int, barrier *bench.StartBarrier) (bench.LevelResult, error)
}

// WorkloadSpec is the wire shape of a workload. The numeric fields are
// pointers so an ABSENT field (nil → the CLI flag default, a minimal curl
// body works) is distinguishable from an EXPLICIT value — an explicit 0 is
// meaningful for warmup_sec (no warm-up) and clone_depth (full history), and
// an explicit invalid 0 (files_min, duration_sec) must reach Validate and be
// rejected exactly like the CLI rejects it, not silently defaulted.
type WorkloadSpec struct {
	Strategy       string   `json:"strategy"`
	Concurrency    []int    `json:"concurrency"`
	DurationSec    *float64 `json:"duration_sec"`
	WarmupSec      *float64 `json:"warmup_sec"`
	FilesMin       *int     `json:"files_min"`
	FilesMax       *int     `json:"files_max"`
	FileSize       *int     `json:"file_size"`
	BranchPrefix   string   `json:"branch_prefix"`
	SessionCommits *int     `json:"session_commits"`
	CloneDepth     *int     `json:"clone_depth"`
	BaseRef        string   `json:"base_ref"`
}

// orDefault fills an absent (nil) field with its CLI flag default. Explicit
// values — including zeros — pass through untouched for Validate to judge.
func orDefault[T any](p *T, def T) *T {
	if p == nil {
		return &def
	}
	return p
}

// withDefaults fills absent fields with the CLI flag defaults. Only nil means
// absent: an explicit `"concurrency": []` stays empty and fails Validate, and
// explicit zeros keep their documented meanings (warmup 0 = none, clone_depth
// 0 = full history) or their documented errors (files_min/duration).
func (ws WorkloadSpec) withDefaults() WorkloadSpec {
	if ws.Strategy == "" {
		ws.Strategy = "branch"
	}
	if ws.Concurrency == nil {
		ws.Concurrency = []int{1, 8, 32, 128}
	}
	ws.DurationSec = orDefault(ws.DurationSec, 60)
	ws.WarmupSec = orDefault(ws.WarmupSec, 10)
	ws.FilesMin = orDefault(ws.FilesMin, 1)
	ws.FilesMax = orDefault(ws.FilesMax, 10)
	ws.FileSize = orDefault(ws.FileSize, 2048)
	ws.SessionCommits = orDefault(ws.SessionCommits, 5)
	ws.CloneDepth = orDefault(ws.CloneDepth, 1)
	return ws
}

// toWorkload assumes withDefaults already ran, so every pointer is non-nil.
func (ws WorkloadSpec) toWorkload(runID string) bench.Workload {
	return bench.Workload{
		RunID:          runID,
		Strategy:       ws.Strategy,
		BranchPrefix:   ws.BranchPrefix,
		Concurrency:    ws.Concurrency,
		Duration:       time.Duration(*ws.DurationSec * float64(time.Second)),
		Warmup:         time.Duration(*ws.WarmupSec * float64(time.Second)),
		Commit:         bench.CommitConfig{FilesMin: *ws.FilesMin, FilesMax: *ws.FilesMax, FileSize: *ws.FileSize},
		SessionCommits: *ws.SessionCommits,
		CloneDepth:     *ws.CloneDepth,
		BaseRef:        ws.BaseRef,
	}
}

// TargetSpec is the wire shape of one target as POSTed. Secret exists ONLY
// here: it is consumed into the bench.Target and never stored on the run, so
// no status/list/event payload can leak it. SecretSource ("gh" | "entire")
// makes the server pull the credential from the operator's CLI at run start
// instead — see local.go — so the browser never holds the token at all.
type TargetSpec struct {
	Name         string   `json:"name"`
	Remote       string   `json:"remote"`
	Repos        []string `json:"repos"`
	User         string   `json:"user"`
	Secret       string   `json:"secret,omitempty"`
	SecretSource string   `json:"secret_source,omitempty"`
	Insecure     bool     `json:"insecure"`
	ObjectFormat string   `json:"object_format"`
	TokenURL     string   `json:"token_url"`
	Jurisdiction string   `json:"jurisdiction"`
	ClientID     string   `json:"client_id"`
}

func (ts TargetSpec) toTarget() bench.Target {
	return bench.Target{
		Name: ts.Name, Remote: ts.Remote, Repos: ts.Repos, User: ts.User,
		Secret: ts.Secret, Insecure: ts.Insecure, ObjectFmt: ts.ObjectFormat,
		TokenURL: ts.TokenURL, Jurisdiction: ts.Jurisdiction, ClientID: ts.ClientID,
	}
}

// TargetInfo is the redacted, stable identity of a target within a run.
type TargetInfo struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Remote string `json:"remote"`
}

type startRequest struct {
	ConfirmAuthorized bool         `json:"confirm_authorized"`
	Workload          WorkloadSpec `json:"workload"`
	Targets           []TargetSpec `json:"targets"`
}

// targetState is one target's live state inside a run. dead/fatalErr/results
// are guarded by the run's mutex; the collector has its own.
type targetState struct {
	id     int
	name   string
	spec   bench.Target // Secret is zeroed as soon as the runner is built
	runner levelRunner
	col    *collector

	label    string
	dead     bool
	fatalErr string
	results  []bench.LevelResult
	series   []results.SeriesPoint // 1s buckets, retained for the result doc
}

// Run is one comparison run: N targets driven through one workload with
// level-aligned starts.
type Run struct {
	ID        string
	StartedAt time.Time
	log       *eventLog
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{} // closed when the coordinator goroutine returns

	workload WorkloadSpec // normalized, as accepted
	bw       bench.Workload

	mu           sync.Mutex
	state        string // starting | running | done | cancelled | failed
	targets      []*targetState
	levelIdx     int
	levelConc    int
	measuring    bool      // a level's timed window is open; gates periodic bucket emission
	windowStart  time.Time // when the current level's timed window opened (barrier fire)
	lastBucketAt time.Time // wall clock of the last bucket emission, for the per-bucket duration
	resultsFile  string
}

var errRunActive = errors.New("a benchmark run is already active; cancel it or wait for it to finish")

// RunManager owns every run of a server session and enforces the
// one-active-run-at-a-time rule: overlapping load generators would corrupt
// each other's numbers.
type RunManager struct {
	mu         sync.Mutex
	runs       map[string]*Run
	order      []string // creation order, for pruning
	active     *Run
	resultsDir string

	// baseCtx is the parent of every run's context and of credential
	// resolution. beginShutdown cancels it, so a start already resolving
	// credentials when shutdown begins is rejected rather than launching a
	// benchmark on a background context that outlives the server.
	baseCtx    context.Context
	baseCancel context.CancelFunc
}

func newRunManager(resultsDir string) *RunManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &RunManager{runs: make(map[string]*Run), resultsDir: resultsDir, baseCtx: ctx, baseCancel: cancel}
}

// beginShutdown cancels the base context so no new run can start (or keep
// resolving credentials) once the server is stopping. Idempotent.
func (m *RunManager) beginShutdown() { m.baseCancel() }

var errShuttingDown = errors.New("server is shutting down")

func (m *RunManager) get(id string) *Run {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runs[id]
}

func (m *RunManager) list() []*Run {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Run, 0, len(m.order))
	for i := len(m.order) - 1; i >= 0; i-- { // newest first
		out = append(out, m.runs[m.order[i]])
	}
	return out
}

// start validates a request and launches its coordinator. It returns
// user-facing warnings (e.g. the github.com abuse note) alongside the run.
func (m *RunManager) start(req startRequest) (*Run, []string, error) {
	if !req.ConfirmAuthorized {
		return nil, nil, errors.New("confirm_authorized must be true: only benchmark infrastructure you own or are explicitly authorized to load-test")
	}
	if len(req.Targets) == 0 {
		return nil, nil, errors.New("at least one target is required")
	}
	if len(req.Targets) > maxTargets {
		return nil, nil, fmt.Errorf("at most %d targets per run", maxTargets)
	}

	ws := req.Workload.withDefaults()
	runID := bench.NewRunID()
	bw := ws.toWorkload(runID)
	if err := bw.Validate(); err != nil {
		return nil, nil, err
	}

	var warnings []string
	targets := make([]*targetState, len(req.Targets))
	for i, ts := range req.Targets {
		bt := ts.toTarget()
		name := ts.Name
		if name == "" {
			name = defaultTargetName(bt.Remote, i)
		}
		// Reject a remote that embeds credentials (https://user:token@host): the
		// remote is echoed verbatim in the hello event, status APIs, target
		// labels, and the persisted result doc, so userinfo would leak a secret
		// there despite Target.Secret being redacted everywhere. Credentials
		// belong in secret / secret_source, never the URL. A parse failure is
		// rejected too, not waved through as credential-free — a malformed remote
		// like https://user:token@host/%zz would otherwise skip this check and
		// still be emitted verbatim.
		u, err := url.Parse(bt.Remote)
		if err != nil {
			return nil, nil, fmt.Errorf("target %s: invalid remote URL %q: %w", name, bt.Remote, err)
		}
		if u.User != nil {
			return nil, nil, fmt.Errorf("target %s: remote must not embed credentials in the URL (user:...@); use secret or secret_source", name)
		}
		// A real target must be an absolute http(s) URL with a host, and carry no
		// query or fragment. url.Parse accepts relative URLs and other schemes, so
		// without the scheme/host check a remote like file:///repo would drive
		// go-git's local transport while posing as a smart-HTTP forge; and a
		// remote like https://host/path?access_token=… would serialize a
		// credential into the SSE/status/label data the Remote is echoed into.
		// (demo:// legitimately uses the query for its synthetic knobs.)
		if !isDemoRemote(bt.Remote) {
			switch {
			case (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
				return nil, nil, fmt.Errorf("target %s: remote must be an absolute http(s) URL with a host, got %q", name, bt.Remote)
			case u.RawQuery != "" || u.Fragment != "":
				return nil, nil, fmt.Errorf("target %s: remote must not carry a query or fragment (repos are appended to the base URL); got %q", name, bt.Remote)
			}
		}
		if ts.SecretSource != "" && ts.Secret != "" {
			return nil, nil, fmt.Errorf("target %s: pass secret or secret_source, not both", name)
		}
		// Pin each CLI-sourced credential to its issuer's audience before it is
		// ever materialized: otherwise a spec could pair "gh" with an attacker's
		// remote (leaking the GitHub token as Basic Auth) or "entire" with an
		// arbitrary token_url (leaking the subject token to the exchange). A
		// self-hosted forge should paste a token instead of naming a CLI source.
		if ts.SecretSource != "" {
			// Verified TLS is what makes the audience pinning below mean anything:
			// with insecure set, newHTTPClient skips certificate verification, so
			// the "github.com" the check approved could be answered by any on-path
			// endpoint — which would then receive the operator's CLI credential.
			if ts.Insecure {
				return nil, nil, fmt.Errorf("target %s: insecure cannot be combined with secret_source (unverified TLS would send the CLI credential to whoever answers for the host); paste a scoped secret instead", name)
			}
			if err := validateSecretAudience(ts.SecretSource, bt.Remote, bt.TokenURL, bt.Jurisdiction); err != nil {
				return nil, nil, fmt.Errorf("target %s: %w", name, err)
			}
		}
		// Fast-fail real targets at the request instead of letting them die
		// mid-run inside NewRunner (which the browser only learns from a
		// target_error event). demo:// targets need no repos or credential.
		if !isDemoRemote(bt.Remote) {
			if len(bt.Repos) == 0 {
				return nil, nil, fmt.Errorf("target %s: no repos", name)
			}
			if bw.Strategy == "repo" && len(bt.Repos) < 2 {
				return nil, nil, fmt.Errorf("target %s: strategy=repo needs >= 2 repos", name)
			}
			if ts.Secret == "" && ts.SecretSource == "" {
				return nil, nil, fmt.Errorf("target %s: no credential (pass secret or secret_source)", name)
			}
		}
		if bench.IsGitHubDotCom(bt.Remote) {
			if hi := slices.Max(bw.Concurrency); hi > 16 {
				warnings = append(warnings, fmt.Sprintf(
					"concurrency %d against github.com is likely to trip secondary rate limits / abuse detection — keep it low (e.g. 1,4)", hi))
			}
		}
		targets[i] = &targetState{id: i, name: name, spec: bt, col: &collector{}}
	}

	// Reject a second concurrent start before doing any credential work:
	// resolving a secret_source execs the operator's CLIs, which is wasted (and
	// needlessly materializes tokens) for a request we're about to refuse. The
	// authoritative check under m.mu below still guards the start/set race.
	m.mu.Lock()
	active := m.active != nil
	m.mu.Unlock()
	if active {
		return nil, nil, errRunActive
	}
	if m.baseCtx.Err() != nil {
		return nil, nil, errShuttingDown
	}

	// Resolve CLI-sourced secrets concurrently: each exec can block on a slow
	// CLI (15s timeout apiece), and serial resolution would stack that latency
	// into this synchronous POST handler. Failures still surface here as a 400
	// so a bad login is immediate feedback, not a mid-run target death.
	var (
		secWG   sync.WaitGroup
		secMu   sync.Mutex
		secErrs []error
	)
	for i, ts := range req.Targets {
		if ts.SecretSource == "" {
			continue
		}
		secWG.Add(1)
		go func(t *targetState, source string) {
			defer secWG.Done()
			user, secret, err := resolveSecretSource(m.baseCtx, source, t.spec.Remote)
			if err != nil {
				secMu.Lock()
				secErrs = append(secErrs, fmt.Errorf("target %s: %w", t.name, err))
				secMu.Unlock()
				return
			}
			t.spec.Secret = secret
			if user != "" && t.spec.User == "" {
				t.spec.User = user // e.g. glab → oauth2; explicit -user still wins
			}
		}(targets[i], ts.SecretSource)
	}
	secWG.Wait()
	if len(secErrs) > 0 {
		return nil, nil, errors.Join(secErrs...)
	}
	// Shutdown may have begun while credentials resolved: refuse to launch, so a
	// benchmark can't start (and write to remotes) after the server is stopping.
	if m.baseCtx.Err() != nil {
		return nil, nil, errShuttingDown
	}

	// Derive from the manager's base context so shutdown cancels this run even in
	// the window before it's registered active.
	ctx, cancel := context.WithCancel(m.baseCtx)
	run := &Run{
		ID: runID, StartedAt: time.Now(), log: newEventLog(),
		ctx: ctx, cancel: cancel, done: make(chan struct{}),
		workload: ws, bw: bw, state: "starting", targets: targets,
	}

	m.mu.Lock()
	if m.active != nil {
		m.mu.Unlock()
		cancel()
		return nil, nil, errRunActive
	}
	m.active = run
	m.runs[run.ID] = run
	m.order = append(m.order, run.ID)
	m.pruneLocked()
	m.mu.Unlock()

	run.log.emit("hello", run.helloEvent())
	go run.coordinate(m)
	return run, warnings, nil
}

// pruneLocked drops the oldest finished runs beyond keepFinishedRuns.
// Caller holds m.mu.
func (m *RunManager) pruneLocked() {
	finished := 0
	for i := len(m.order) - 1; i >= 0; i-- {
		r := m.runs[m.order[i]]
		if r == m.active {
			continue
		}
		finished++
		if finished > keepFinishedRuns {
			delete(m.runs, m.order[i])
			m.order = append(m.order[:i], m.order[i+1:]...)
		}
	}
}

func (m *RunManager) clearActive(r *Run) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == r {
		m.active = nil
	}
}

// stopActive cancels the active run (if any) and waits for its coordinator to
// finish generating load, persist its results, and clean up — bounded by
// timeout so shutdown can't hang. Called on server shutdown so an embedded
// caller doesn't return while remote load continues and results are unwritten.
func (m *RunManager) stopActive(timeout time.Duration) {
	m.mu.Lock()
	run := m.active
	m.mu.Unlock()
	if run == nil {
		return
	}
	run.cancel()
	select {
	case <-run.done:
	case <-time.After(timeout):
	}
}

// coordinate is the run's driver goroutine: build runners, then walk the
// concurrency sweep with a barrier per level so every target starts each
// level at the same instant (the whole point of a comparison run).
func (r *Run) coordinate(m *RunManager) {
	defer close(r.done) // let a shutdown waiter know the run has fully finished
	defer r.cancel()

	// Resolve every target concurrently — entiredb setup probes the cluster.
	// A target that fails setup is dead but doesn't kill the comparison.
	var wg sync.WaitGroup
	for _, t := range r.targets {
		wg.Add(1)
		go func(t *targetState) {
			defer wg.Done()
			var (
				lr  levelRunner
				err error
			)
			// Give each target its own ref namespace: agents derive destination
			// refs from the workload RunID, so two targets pointing at the same
			// repository would otherwise push (and session-delete) identical refs
			// and collide, manufacturing CAS failures and voiding the comparison.
			// The run's own ID (r.ID) still identifies the run for reporting.
			bwT := r.bw
			bwT.RunID = fmt.Sprintf("%s-t%d", r.bw.RunID, t.id)
			if isDemoRemote(t.spec.Remote) {
				lr, err = newDemoRunner(t.spec.Remote, bwT, t.col)
			} else {
				lr, err = bench.NewRunner(r.ctx, t.spec, bwT, t.col)
			}
			t.spec.Secret = "" // consumed; nothing on the run retains it
			if err != nil {
				r.markDead(t, -1, err)
				return
			}
			r.mu.Lock()
			t.runner = lr
			t.label = lr.Label()
			r.mu.Unlock()
			r.log.emit("target_ready", map[string]any{
				"target": t.id, "label": lr.Label(), "nodes": lr.Nodes(), "object_format": lr.ObjectFormat(),
			})
		}(t)
	}
	wg.Wait()

	if r.aliveTargets() == nil {
		// If the context was cancelled during setup, NewRunner fails on every
		// target and they all mark dead — but that's a shutdown/user cancel, not
		// a forge failure, so publish it as cancelled.
		state := "failed"
		if r.ctx.Err() != nil {
			state = "cancelled"
		}
		r.finish(m, state)
		return
	}
	r.setState("running")

	// One ticker per run: a single bucket event per second carries every
	// target's stats, so chart columns align by construction.
	tickerDone := make(chan struct{})
	tickerStopped := make(chan struct{})
	go func() {
		defer close(tickerStopped)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				r.emitBucket(false)
			case <-tickerDone:
				return
			}
		}
	}()

	for li, c := range r.bw.Concurrency {
		if r.ctx.Err() != nil {
			break
		}
		alive := r.aliveTargets()
		if alive == nil {
			break
		}

		// Transition atomically under r.mu: reset every collector (fresh
		// rolling window + warm-up boundary) and advance the level index
		// together, so a bucket tick can never attribute the previous level's
		// leftover samples to the new index. The prior level's agents have all
		// returned (lwg.Wait below), so nothing races this reset.
		r.mu.Lock()
		for _, t := range alive {
			t.col.reset(r.bw.Warmup)
		}
		r.levelIdx = li
		r.levelConc = c
		r.mu.Unlock()

		// Real start barrier: every target builds its agents, then all are
		// released together, so target-dependent setup time doesn't shift the
		// measured windows.
		barrier := bench.NewStartBarrier(len(alive))
		var lwg sync.WaitGroup
		for _, t := range alive {
			lwg.Add(1)
			go func(t *targetState) {
				defer lwg.Done()
				res, err := t.runner.RunLevel(r.ctx, c, barrier)
				if err != nil {
					r.markDead(t, li, err)
					return
				}
				r.mu.Lock()
				// A cancelled RunLevel returns a partial summary (nil error) whose
				// throughput denominator is still the full configured duration, so
				// it reads as a completed low-throughput level. Don't publish it —
				// the run is ending as cancelled anyway.
				if r.ctx.Err() == nil {
					t.results = append(t.results, res)
				}
				r.mu.Unlock()
			}(t)
		}
		barrier.Await() // all targets have built their agents (or failed and arrived)
		barrier.Fire()  // release them to start the timed window together
		// The timed window is now open: let the ticker emit buckets. Before this
		// (per-target setup) and after lwg.Wait (session-ref cleanup, between
		// levels) the collectors produce only zero/stale buckets that would
		// pollute the live and persisted timelines, so emission is gated to here.
		r.mu.Lock()
		r.measuring = true
		// Use the barrier's shared fire instant, the same origin RunLevel gives the
		// sample clock, so dt_ms and warm-up shading line up with the samples
		// instead of drifting by post-release scheduler delay.
		r.windowStart = barrier.FiredAt()
		r.lastBucketAt = r.windowStart
		r.mu.Unlock()
		// Emit level_start only now, at the instant the timed window opens. The
		// agents' measured clocks start at Fire, so an "at" stamped before the
		// per-target build (which the barrier absorbs) would make the UI's warm-up
		// shading and countdown lead the real window by the setup time.
		r.log.emit("level_start", map[string]any{
			"level_index": li, "concurrency": c,
			"warmup_sec": r.bw.Warmup.Seconds(), "duration_sec": r.bw.Duration.Seconds(),
			"at": time.Now().UTC(),
		})
		lwg.Wait()
		r.mu.Lock()
		r.measuring = false
		r.mu.Unlock()

		// A cancelled level ran only a partial window; don't flush its tail bucket
		// or publish a level_result for it. The loop exits at the top-of-loop
		// ctx check anyway; break now so nothing partial is emitted.
		if r.ctx.Err() != nil {
			break
		}

		// Flush the level's final partial second before the next level resets
		// the collectors (or the run ends): the ticker only fires on whole
		// seconds, so without this the tail — and every sample of a sub-second
		// level — would be dropped from the live and persisted series even
		// though the authoritative LevelResult counts it. force=true because the
		// window just closed (measuring is false) but this remainder is real.
		r.emitBucket(true)

		results := map[string]bench.LevelResult{}
		r.mu.Lock()
		for _, t := range r.targets {
			if len(t.results) > li {
				results[strconv.Itoa(t.id)] = t.results[li]
			}
		}
		r.mu.Unlock()
		r.log.emit("level_result", map[string]any{
			"level_index": li, "concurrency": c, "targets": results,
		})
	}

	close(tickerDone)
	<-tickerStopped

	state := "done"
	if r.ctx.Err() != nil {
		state = "cancelled"
	} else if r.aliveTargets() == nil {
		state = "failed"
	}
	r.finish(m, state)
}

func (r *Run) finish(m *RunManager, state string) {
	r.setState(state)
	resultsFile := r.persistResults(m.resultsDir)
	r.mu.Lock()
	r.resultsFile = resultsFile
	// Release each runner now the run is over: a finished run stays resident for
	// the history UI (keepFinishedRuns), and its runner still holds the forge
	// credential (a PAT, or an Entire subject/access token). Close drops the
	// connections and the credential reference; dropping t.runner lets the whole
	// runner be collected. Nothing after finish reads t.runner.
	for _, t := range r.targets {
		if c, ok := t.runner.(interface{ Close() }); ok {
			c.Close()
		}
		t.runner = nil
	}
	r.mu.Unlock()
	r.log.emit("run_done", map[string]any{
		"state": state, "results_file": resultsFile, "at": time.Now().UTC(),
	})
	r.log.close()
	m.clearActive(r)
}

// persistResults writes the run's format-2 result doc and returns its bare
// filename ("" on failure — the run still finishes; persistence is
// best-effort).
func (r *Run) persistResults(dir string) string {
	r.mu.Lock()
	doc := results.Doc{
		Format:   2,
		RunID:    r.ID,
		Strategy: r.workload.Strategy,
		Duration: r.bw.Duration.String(),
		Warmup:   r.bw.Warmup.String(),
		Commit:   r.bw.CommitDesc(),
		State:    r.state,
	}
	for _, t := range r.targets {
		doc.Targets = append(doc.Targets, results.TargetResult{
			Name: t.name, Label: t.label, Error: t.fatalErr,
			Levels: slices.Clone(t.results), Series: slices.Clone(t.series),
		})
	}
	r.mu.Unlock()

	file := "forgemark-" + r.ID + ".json"
	if err := results.Write(dir, file, doc); err != nil {
		r.log.emit("target_error", map[string]any{
			"target": -1, "level_index": -1, "fatal": false,
			"message": "persist results: " + err.Error(),
		})
		return ""
	}
	return file
}

// emitBucket snapshots every collector and emits/persists one bucket. The
// periodic ticker calls it with force=false, which is a no-op unless a level's
// timed window is open (r.measuring) — so setup, cleanup, and between-level idle
// don't append zero/stale buckets to the timeline. The end-of-level flush calls
// it with force=true to emit the window's final partial second even though the
// window has just closed.
func (r *Run) emitBucket(force bool) {
	now := time.Now().Unix()
	// Hold r.mu across the whole tick so a level transition (which resets the
	// collectors and advances levelIdx under the same lock) can't interleave:
	// otherwise a snapshot could carry the previous level's samples under the
	// new level index. The snapshot and series append are cheap and run once
	// per second. Lock order is always r.mu → collector.mu, never reversed.
	r.mu.Lock()
	defer r.mu.Unlock()
	// A bucket's OK count spans the interval since the previous bucket, which is
	// only ~1s for a steady tick but is fractional for the first tick after the
	// warm-up boundary and for the forced tail flush. Carry the bucket's actual
	// measured duration so consumers plot count/duration (a true rate) instead of
	// treating the raw count as ops/s. dt is the overlap of (lastBucketAt, now]
	// with the measured window [windowStart+warmup, +duration], so warm-up-only
	// buckets get 0 and the tail gets only its real remainder.
	wallNow := time.Now()
	measStart := r.windowStart.Add(r.bw.Warmup)
	measEnd := measStart.Add(r.bw.Duration)
	// A periodic tick emits only inside an open window. measuring stays true
	// through RunLevel's post-window session-ref cleanup (up to 30s), so also
	// gate on the deadline: past measEnd the ticks are cleanup-time buckets with
	// zero duration and stale percentiles, which consumers would misread. The
	// forced tail flush (force=true) still emits the window's real remainder.
	if !force && (!r.measuring || wallNow.After(measEnd)) {
		return
	}
	lo, hi := r.lastBucketAt, wallNow
	if lo.Before(measStart) {
		lo = measStart
	}
	if hi.After(measEnd) {
		hi = measEnd
	}
	// Round a positive overlap UP to a whole millisecond, never truncate: every
	// consumer reads dt_ms 0/absent as the ~1s legacy fallback, so a real
	// sub-millisecond overlap truncated to 0 would understate that bucket's
	// rate by up to 1000×. Ceiling instead overstates the sliver by <1ms.
	dtMs := int64(0)
	if d := hi.Sub(lo); d > 0 {
		dtMs = int64((d + time.Millisecond - 1) / time.Millisecond)
	}
	r.lastBucketAt = wallNow
	if dtMs == 0 {
		// Zero overlap means nothing measured happened in this tick — a warm-up
		// second, typically. dt_ms 0 is dropped by omitempty on persist, and every
		// consumer reads absent as a legacy ~1s bucket, so emitting it would add a
		// phantom second to any (t, level) slot the replay merges it into and
		// understate the replayed rate. Skip periodic ticks outright; a forced
		// tail flush may still carry boundary-sliver completions, so emit those
		// under a 1ms floor rather than an ambiguous zero.
		if !force {
			return
		}
		dtMs = 1
	}

	li, c := r.levelIdx, r.levelConc
	stats := make(map[string]BucketStats, len(r.targets))
	for _, t := range r.targets {
		if t.dead {
			continue
		}
		st := t.col.snapshot()
		stats[strconv.Itoa(t.id)] = st
		if len(t.series) < maxBufferedEvents {
			t.series = append(t.series, results.SeriesPoint{
				T: now, DtMs: dtMs, Level: li, OK: st.OK, CAS: st.CAS, Err: st.Err,
				P50: st.P50, P95: st.P95, P99: st.P99,
				CloneOK: st.CloneOK, CloneErr: st.CloneErr,
				CloneP50: st.CloneP50, CloneP95: st.CloneP95, CloneP99: st.CloneP99,
			})
		}
	}
	if len(stats) == 0 {
		return
	}
	r.log.emit("bucket", map[string]any{
		"level_index": li, "concurrency": c, "t": now, "dt_ms": dtMs, "targets": stats,
	})
}

func (r *Run) markDead(t *targetState, levelIdx int, err error) {
	r.mu.Lock()
	t.dead = true
	t.fatalErr = err.Error()
	r.mu.Unlock()
	r.log.emit("target_error", map[string]any{
		"target": t.id, "level_index": levelIdx, "message": err.Error(), "fatal": true,
	})
}

// aliveTargets returns the targets still participating, or nil when none are.
func (r *Run) aliveTargets() []*targetState {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*targetState
	for _, t := range r.targets {
		if !t.dead {
			out = append(out, t)
		}
	}
	return out
}

func (r *Run) setState(s string) {
	r.mu.Lock()
	r.state = s
	r.mu.Unlock()
}

func (r *Run) helloEvent() map[string]any {
	infos := make([]TargetInfo, len(r.targets))
	for i, t := range r.targets {
		infos[i] = TargetInfo{ID: t.id, Name: t.name, Remote: t.spec.Remote}
	}
	return map[string]any{
		"run_id":     r.ID,
		"state":      "starting",
		"started_at": r.StartedAt.UTC(),
		"workload":   r.workload,
		"targets":    infos,
		"levels":     r.bw.Concurrency,
	}
}

// targetStatus is the redacted per-target view in GET responses.
type targetStatus struct {
	TargetInfo
	Label   string              `json:"label,omitempty"`
	Dead    bool                `json:"dead,omitempty"`
	Error   string              `json:"error,omitempty"`
	Results []bench.LevelResult `json:"results,omitempty"`
}

// runStatus is the redacted run view for GET /api/runs and /api/runs/{id}.
type runStatus struct {
	ID          string         `json:"id"`
	State       string         `json:"state"`
	StartedAt   time.Time      `json:"started_at"`
	Workload    WorkloadSpec   `json:"workload"`
	Levels      []int          `json:"levels"`
	LevelIndex  int            `json:"level_index"`
	Targets     []targetStatus `json:"targets"`
	ResultsFile string         `json:"results_file,omitempty"`
}

func (r *Run) status() runStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := runStatus{
		ID: r.ID, State: r.state, StartedAt: r.StartedAt.UTC(),
		Workload: r.workload, Levels: r.bw.Concurrency, LevelIndex: r.levelIdx,
		ResultsFile: r.resultsFile,
	}
	for _, t := range r.targets {
		st.Targets = append(st.Targets, targetStatus{
			TargetInfo: TargetInfo{ID: t.id, Name: t.name, Remote: t.spec.Remote},
			Label:      t.label, Dead: t.dead, Error: t.fatalErr,
			Results: slices.Clone(t.results),
		})
	}
	return st
}

// defaultTargetName labels an unnamed target by its remote host.
func defaultTargetName(remote string, i int) string {
	if u, err := url.Parse(remote); err == nil && u.Host != "" {
		return u.Host
	}
	return fmt.Sprintf("target-%d", i+1)
}
