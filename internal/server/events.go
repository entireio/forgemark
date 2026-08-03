package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// maxBufferedEvents caps a run's replayable event log. Validated workloads can
// legitimately outlast it (16 levels × 6h measured + 1h warm-up ≈ 400k bucket
// seconds), so hitting the cap is announced with a series_truncated event
// rather than treated as unreachable.
const maxBufferedEvents = 100_000

// event is one serialized SSE frame: a 1-based sequence number (the SSE id,
// which browsers echo back as Last-Event-ID on reconnect), a type, and its
// marshaled JSON payload.
type event struct {
	seq  int64
	name string
	data []byte
}

// eventLog is a run's append-only event history plus live fanout. Every event
// is retained (bounded by maxBufferedEvents), so a subscriber at any point —
// first open, late join, browser reconnect — is handled the same way: replay
// everything after its Last-Event-ID, then stream. That makes dropping a slow
// subscriber lossless: it reconnects and replays what it missed.
type eventLog struct {
	mu        sync.Mutex
	events    []event
	subs      map[chan event]struct{}
	closed    bool
	truncated bool // a bucket event has been dropped at the cap (announced once)
}

func newEventLog() *eventLog {
	return &eventLog{subs: make(map[chan event]struct{})}
}

// emit marshals v, appends the event, and fans it out. A subscriber whose
// buffer is full is dropped (its channel closed) rather than blocking the
// coordinator; the SSE handler ends that response and the browser reconnects
// into a replay.
func (l *eventLog) emit(name string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		// A marshal failure is a programming error in an event payload; surface
		// it in-band rather than silently dropping the event.
		data = fmt.Appendf(nil, `{"error":"marshal: %s"}`, err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// The cap bounds ONLY the bulk stream (one bucket event per second);
	// lifecycle events always land. They are O(levels + targets) — a handful
	// past the cap at most — and every consumer's correctness hangs on them:
	// dropping a level_start/level_result would let the race podium crown a
	// winner from an earlier level's data, and dropping run_done would leave
	// every viewer of a capped run reconnect-looping forever. Bucket loss
	// degrades gracefully (a gap in the replayed charts) — but never silently:
	// the first dropped bucket becomes a one-time series_truncated marker so
	// live viewers see WHY their charts froze instead of a run that looks
	// stalled, and replays know the timeline is incomplete.
	if l.closed {
		return
	}
	if len(l.events) >= maxBufferedEvents && name == "bucket" {
		if l.truncated {
			return
		}
		l.truncated = true
		name = "series_truncated"
		data = fmt.Appendf(nil, `{"cap":%d}`, maxBufferedEvents)
	}
	e := event{seq: int64(len(l.events)) + 1, name: name, data: data}
	l.events = append(l.events, e)
	for ch := range l.subs {
		select {
		case ch <- e:
		default:
			delete(l.subs, ch)
			close(ch)
		}
	}
}

// subscribe returns the replay of every event after seq `after`, plus a live
// channel (nil if the log is closed — the replay then already ends in the
// terminal event) and a cancel func.
func (l *eventLog) subscribe(after int64) ([]event, chan event, func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if after < 0 {
		after = 0 // a malformed/negative Last-Event-ID must not index the slice negatively
	}
	var replay []event
	if after < int64(len(l.events)) {
		replay = append(replay, l.events[after:]...)
	}
	if l.closed {
		return replay, nil, func() {}
	}
	ch := make(chan event, 256)
	l.subs[ch] = struct{}{}
	cancel := func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if _, ok := l.subs[ch]; ok {
			delete(l.subs, ch)
			close(ch)
		}
	}
	return replay, ch, cancel
}

// close ends the log after the terminal event has been emitted: live
// subscribers are closed out (their replay already delivered everything) and
// future subscribers get replay-only.
func (l *eventLog) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	for ch := range l.subs {
		delete(l.subs, ch)
		close(ch)
	}
}

// serveSSE streams an eventLog as a Server-Sent-Events response: replay from
// the client's Last-Event-ID (or 0), then live events, with a comment
// heartbeat so proxies and the browser keep the connection alive.
func serveSSE(w http.ResponseWriter, r *http.Request, l *eventLog) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	var after int64
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		after, _ = strconv.ParseInt(v, 10, 64)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	replay, ch, cancel := l.subscribe(after)
	defer cancel()
	for _, e := range replay {
		writeSSE(w, e)
	}
	fl.Flush()
	if ch == nil {
		return // run already finished; the replay ended in run_done
	}

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return // log closed (run finished) or we were dropped as slow
			}
			writeSSE(w, e)
			fl.Flush()
		case <-heartbeat.C:
			_, _ = fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func writeSSE(w http.ResponseWriter, e event) {
	_, _ = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.seq, e.name, e.data)
}
