package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/entireio/forgemark/internal/bench"
	"github.com/entireio/forgemark/internal/results"
)

// errThresholdBreached makes the CLI exit 2, distinct from 1 for operational
// errors, so CI can tell a regression from a broken run.
var errThresholdBreached = errors.New("threshold breached")

// thresholds are the optional per-level CI gates. Zero (or -1 for maxErrors)
// disables a gate.
type thresholds struct {
	minPushRate float64
	maxP95      time.Duration
	maxErrors   int
}

func (t thresholds) configured() bool {
	return t.minPushRate > 0 || t.maxP95 > 0 || t.maxErrors >= 0
}

func (t thresholds) validate() error {
	if t.minPushRate < 0 {
		return errors.New("-min-push-rate must be >= 0")
	}
	if t.maxP95 < 0 {
		return errors.New("-max-p95 must be >= 0")
	}
	if t.maxErrors < -1 {
		return errors.New("-max-errors must be >= 0 when set")
	}
	return nil
}

// check reports every configured threshold breach in sweep order.
func (t thresholds) check(levels []bench.LevelResult) []string {
	var breaches []string
	for _, r := range levels {
		if t.minPushRate > 0 && r.OpsPerSec < t.minPushRate {
			breaches = append(breaches, fmt.Sprintf("c=%d push/s=%.1f < min %.1f", r.Concurrency, r.OpsPerSec, t.minPushRate))
		}
		if t.maxP95 > 0 {
			maxP95ms := float64(t.maxP95) / float64(time.Millisecond)
			if r.P95ms > maxP95ms {
				breaches = append(breaches, fmt.Sprintf("c=%d p95=%.1fms > max %.1fms", r.Concurrency, r.P95ms, maxP95ms))
			}
		}
		if t.maxErrors >= 0 && r.OtherErrors > t.maxErrors {
			breaches = append(breaches, fmt.Sprintf("c=%d errors=%d > max %d", r.Concurrency, r.OtherErrors, t.maxErrors))
		}
	}
	return breaches
}

// annotate records the configured gates and their breaches on doc. It leaves
// doc untouched when no gate is set, so ungated runs keep the plain shape.
func (t thresholds) annotate(doc *results.Doc, levels []bench.LevelResult) {
	if !t.configured() {
		return
	}
	breaches := t.check(levels)
	if breaches == nil {
		breaches = []string{}
	}
	ok := len(breaches) == 0
	rec := &results.Threshold{MinPushRate: t.minPushRate, Breaches: breaches}
	if t.maxP95 > 0 {
		rec.MaxP95 = t.maxP95.String()
	}
	if t.maxErrors >= 0 {
		maxErrors := t.maxErrors
		rec.MaxErrors = &maxErrors
	}
	doc.ThresholdsOK = &ok
	doc.Thresholds = rec
}
