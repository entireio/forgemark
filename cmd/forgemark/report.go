package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/entireio/forgemark/internal/bench"
	"github.com/entireio/forgemark/internal/results"
)

// runReport is `forgemark report`: it reduces a stored result doc to a single
// headline number for downstream consumers. `-emit monitor` prints the Entire
// runner contract — a `{"value","rationale"}` object as the last JSON line of
// stdout, where value is peak sustained throughput — so a trail runner or a CI
// step can gate on it without re-parsing the whole doc. `-emit text` prints a
// human summary of the same reduction.
func runReport(args []string) error {
	fs := flag.NewFlagSet("forgemark report", flag.ContinueOnError)
	emit := fs.String("emit", "monitor", "output format: monitor (Entire runner contract, JSON last line) | text (human summary)")
	target := fs.String("target", "", "for a multi-target (comparison) doc, the target name to report; default: the first target")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: forgemark report [-emit monitor|text] [-target NAME] <results.json>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("exactly one results file path is required")
	}
	if *emit != "monitor" && *emit != "text" {
		return fmt.Errorf("invalid -emit %q (monitor | text)", *emit)
	}

	doc, err := results.LoadPath(fs.Arg(0))
	if errors.Is(err, results.ErrNotFound) {
		return fmt.Errorf("no such results file: %s", fs.Arg(0))
	}
	if err != nil {
		return err
	}
	if len(doc.Targets) == 0 {
		return errors.New("results doc has no targets")
	}

	tr, err := selectTarget(doc.Targets, *target)
	if err != nil {
		return err
	}
	mon := monitorFor(tr)

	if *emit == "text" {
		printReportText(doc, tr, mon)
		return nil
	}
	// monitor: the last_json_line adapter reads the final JSON line of stdout,
	// so emit exactly the contract object and nothing else.
	b, err := json.Marshal(mon)
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

// monitorResult is the Entire runner "monitor" output contract: one numeric
// value plus a human rationale, emitted as the last JSON line of stdout.
type monitorResult struct {
	Value     float64 `json:"value"`
	Rationale string  `json:"rationale"`
}

// selectTarget resolves the -target selector against a doc's targets: an empty
// selector takes the first target (the common single-target case); a non-empty
// one matches by Name or Label.
func selectTarget(targets []results.TargetResult, name string) (results.TargetResult, error) {
	if name == "" {
		return targets[0], nil
	}
	for _, t := range targets {
		if t.Name == name || t.Label == name {
			return t, nil
		}
	}
	have := make([]string, len(targets))
	for i, t := range targets {
		have[i] = t.Name
	}
	return results.TargetResult{}, fmt.Errorf("no target %q in results (have: %s)", name, strings.Join(have, ", "))
}

// monitorFor reduces one target to its headline monitor: peak sustained
// throughput (OK ops/sec, the primary op for the strategy) across the sweep. A
// target that died or measured nothing reports value 0 so a gate treats it as a
// regression rather than a silent pass.
func monitorFor(tr results.TargetResult) monitorResult {
	label := opLabel(tr)
	if tr.Error != "" {
		return monitorResult{Value: 0, Rationale: fmt.Sprintf("target %q failed: %s", targetName(tr), tr.Error)}
	}
	if len(tr.Levels) == 0 {
		return monitorResult{Value: 0, Rationale: fmt.Sprintf("target %q has no measured levels", targetName(tr))}
	}

	var (
		peak     bench.LevelResult
		havePeak bool
		perLevel = make([]string, 0, len(tr.Levels))
		errs     int
	)
	for _, l := range tr.Levels {
		if !havePeak || l.OpsPerSec > peak.OpsPerSec {
			peak, havePeak = l, true
		}
		perLevel = append(perLevel, fmt.Sprintf("c=%d %s", l.Concurrency, round1(l.OpsPerSec)))
		errs += levelErrors(l, label)
	}

	rationale := fmt.Sprintf("peak %s %s ops/s @ c=%d | %s | p95 %dms | %d errors",
		label, round1(peak.OpsPerSec), peak.Concurrency,
		strings.Join(perLevel, ", "), int(math.Round(peakP95(peak, label))), errs)
	return monitorResult{Value: math.Round(peak.OpsPerSec*10) / 10, Rationale: rationale}
}

// opLabel names the primary operation being measured, so the rationale reads
// "push" for write strategies and "clone" for the read-only clone loop.
func opLabel(tr results.TargetResult) string {
	for _, l := range tr.Levels {
		if l.Strategy == "clone" {
			return "clone"
		}
	}
	return "push"
}

// levelErrors counts the failed primary operations in a level: CAS losses and
// other push errors on the write path, clone errors on the read path.
func levelErrors(l bench.LevelResult, label string) int {
	if label == "clone" {
		return l.CloneErrors
	}
	return l.CASFailures + l.OtherErrors
}

// peakP95 is the primary-op p95 at the peak level.
func peakP95(l bench.LevelResult, label string) float64 {
	if label == "clone" {
		return l.CloneP95ms
	}
	return l.P95ms
}

func targetName(tr results.TargetResult) string {
	if tr.Name != "" {
		return tr.Name
	}
	return tr.Label
}

// round1 rounds to one decimal place and renders without a trailing zero pile,
// so 42.70 prints as "42.7" and 8 as "8".
func round1(v float64) string {
	return fmt.Sprintf("%g", math.Round(v*10)/10)
}

func printReportText(doc results.Doc, tr results.TargetResult, mon monitorResult) {
	fmt.Printf("run %s  strategy=%s  duration=%s  warmup=%s\n", doc.RunID, doc.Strategy, doc.Duration, doc.Warmup)
	if len(doc.Targets) > 1 {
		fmt.Printf("target %s (of %d)\n", targetName(tr), len(doc.Targets))
	}
	label := opLabel(tr)
	for _, l := range tr.Levels {
		fmt.Printf("  c=%-4d %s/s=%-8s p50=%-7.1f p95=%-8.1f p99=%-8.1f err=%d\n",
			l.Concurrency, label, round1(l.OpsPerSec), l.P50ms, peakP95(l, label), l.P99ms, levelErrors(l, label))
	}
	fmt.Printf("\nmonitor: value=%s  %s\n", round1(mon.Value), mon.Rationale)
}
