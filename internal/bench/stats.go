package bench

import (
	"math"
	"sort"
	"time"
)

// Outcome classifies a single push attempt.
type Outcome int

const (
	OutcomeOK  Outcome = iota // push landed
	OutcomeCAS                // rejected by ref CAS / non-fast-forward (contention)
	OutcomeErr                // any other failure (transport, auth, server error)
)

// OpKind distinguishes which operation a sample measured. Zero value is OpPush
// so existing push-only call sites need no change.
type OpKind int

const (
	OpPush  OpKind = iota // a receive-pack push (the write path)
	OpClone               // a clone (the read path; clone/session strategy)
)

// Sample is one recorded operation, kept in each agent's post-hoc log and
// delivered live to a Sink as the agent records it. Offset is measured from
// the start of the concurrency level so warm-up samples can be dropped
// post-hoc.
type Sample struct {
	Offset time.Duration // measured from level start; warm-up samples included
	Dur    time.Duration
	Res    Outcome
	Op     OpKind
	Msg    string // raw error text, only when Res == OutcomeErr
}

// Sink observes every sample live, warm-up included. OnSample is called
// concurrently from up to `concurrency` agent goroutines mid-measurement, so
// it must be cheap and non-blocking (a mutex-protected aggregator is fine; a
// slow sink would perturb the numbers being measured). A nil Sink disables
// observation — the CLI path.
type Sink interface {
	OnSample(s Sample)
}

// errMsg returns the message to retain on a sample. Only genuine errors carry
// text; OK and CAS outcomes stay message-free.
func errMsg(res Outcome, err error) string {
	if res != OutcomeErr || err == nil {
		return ""
	}
	return err.Error()
}

// LevelResult is the published summary for one concurrency level. JSON tags
// are the columns charts consume.
type LevelResult struct {
	Concurrency int     `json:"concurrency"`
	Strategy    string  `json:"strategy"`
	Repos       int     `json:"repos"`
	Nodes       int     `json:"nodes"`
	WindowSec   float64 `json:"window_sec"`
	Pushes      int     `json:"pushes"` // attempts in the measured window; clone strategy uses this as its primary op count
	OK          int     `json:"ok"`     // successful primary operations
	CASFailures int     `json:"cas_failures"`
	OtherErrors int     `json:"other_errors"`
	OpsPerSec   float64 `json:"ops_per_sec"` // OK / window
	P50ms       float64 `json:"p50_ms"`
	P95ms       float64 `json:"p95_ms"`
	P99ms       float64 `json:"p99_ms"`
	P999ms      float64 `json:"p999_ms"`
	Maxms       float64 `json:"max_ms"`
	CommitFiles string  `json:"commit_files"` // e.g. "1-10 x 2048B"

	// Clone/session-strategy read side (op==clone). Omitted in push/repo modes.
	Clones      int     `json:"clones,omitempty"`
	CloneOK     int     `json:"clone_ok,omitempty"`
	CloneErrors int     `json:"clone_errors,omitempty"`
	CloneP50ms  float64 `json:"clone_p50_ms,omitempty"`
	CloneP95ms  float64 `json:"clone_p95_ms,omitempty"`
	CloneP99ms  float64 `json:"clone_p99_ms,omitempty"`

	// Distinct error messages (normalized) with occurrence counts, for the err
	// bucket only. Omitted when there were no errors.
	ErrorMessages      []ErrGroup `json:"error_messages,omitempty"`
	CloneErrorMessages []ErrGroup `json:"clone_error_messages,omitempty"`
}

// ErrGroup is one distinct (normalized) error message and how often it occurred.
type ErrGroup struct {
	Message string `json:"message"`
	Count   int    `json:"count"`
}

// maxDistinctErrors caps how many distinct messages a level reports; the rest
// fold into an "(other errors)" bucket.
const maxDistinctErrors = 20

// errGroups folds raw error messages into a deterministic, bounded breakdown:
// normalize each distinct raw once (collapsing this run's volatile URLs/refs),
// tally by the resulting key, then keep the maxDistinctErrors most frequent
// (count desc, message asc) and fold the remainder into "(other errors)".
// Returns nil when there were no errors, so the JSON field is omitted.
func errGroups(raw map[string]int) []ErrGroup {
	if len(raw) == 0 {
		return nil
	}
	// Normalize once per distinct raw message, not once per occurrence — an
	// outage repeating one error tens of thousands of times normalizes it once.
	counts := make(map[string]int, len(raw))
	for msg, n := range raw {
		counts[normalizeErr(msg)] += n
	}
	out := make([]ErrGroup, 0, len(counts))
	for msg, n := range counts {
		out = append(out, ErrGroup{Message: msg, Count: n})
	}
	byFreq := func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Message < out[j].Message
	}
	sort.Slice(out, byFreq)
	if len(out) > maxDistinctErrors {
		other := 0
		for _, g := range out[maxDistinctErrors:] {
			other += g.Count
		}
		out = append(out[:maxDistinctErrors], ErrGroup{Message: "(other errors)", Count: other})
		sort.Slice(out, byFreq) // the overflow bucket may outrank the kept singletons
	}
	return out
}

// summarize folds raw samples into a LevelResult, dropping anything inside the
// warm-up window and computing exact percentiles over the OK samples.
func summarize(samples []Sample, concurrency int, strategy string, repos, nodes int,
	warmup, window time.Duration, commitFiles string) LevelResult {
	r := LevelResult{
		Concurrency: concurrency,
		Strategy:    strategy,
		Repos:       repos,
		Nodes:       nodes,
		WindowSec:   window.Seconds(),
		CommitFiles: commitFiles,
	}

	var okDurs, cloneDurs []float64
	pushErrs := map[string]int{}
	cloneErrs := map[string]int{}
	for _, s := range samples {
		if s.Offset < warmup {
			continue // warm-up: excluded from every statistic
		}
		if s.Op == OpClone {
			r.Clones++
			if s.Res == OutcomeOK {
				r.CloneOK++
				cloneDurs = append(cloneDurs, float64(s.Dur)/float64(time.Millisecond))
			} else {
				r.CloneErrors++
				cloneErrs[s.Msg]++
			}
			continue
		}
		r.Pushes++
		switch s.Res {
		case OutcomeOK:
			r.OK++
			okDurs = append(okDurs, float64(s.Dur)/float64(time.Millisecond))
		case OutcomeCAS:
			r.CASFailures++
		case OutcomeErr:
			r.OtherErrors++
			pushErrs[s.Msg]++
		}
	}
	r.ErrorMessages = errGroups(pushErrs)
	r.CloneErrorMessages = errGroups(cloneErrs)

	if len(okDurs) > 0 {
		sort.Float64s(okDurs)
		r.P50ms = percentile(okDurs, 50)
		r.P95ms = percentile(okDurs, 95)
		r.P99ms = percentile(okDurs, 99)
		r.P999ms = percentile(okDurs, 99.9)
		r.Maxms = okDurs[len(okDurs)-1]
	}
	if len(cloneDurs) > 0 {
		sort.Float64s(cloneDurs)
		r.CloneP50ms = percentile(cloneDurs, 50)
		r.CloneP95ms = percentile(cloneDurs, 95)
		r.CloneP99ms = percentile(cloneDurs, 99)
	}
	if strategy == "clone" {
		r.Pushes = r.Clones
		r.OK = r.CloneOK
		r.OtherErrors = r.CloneErrors
		r.CASFailures = 0
		r.ErrorMessages = r.CloneErrorMessages
		r.P50ms = r.CloneP50ms
		r.P95ms = r.CloneP95ms
		r.P99ms = r.CloneP99ms
		if len(cloneDurs) > 0 {
			r.P999ms = percentile(cloneDurs, 99.9)
			r.Maxms = cloneDurs[len(cloneDurs)-1]
		}
	}
	if window > 0 {
		r.OpsPerSec = float64(r.OK) / window.Seconds()
	}
	return r
}

// percentile returns the nearest-rank percentile of an already-sorted slice.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	rank = max(rank, 0)
	rank = min(rank, len(sorted)-1)
	return sorted[rank]
}
