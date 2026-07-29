package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// maxBufferedEvents caps a run's replayable event log. At ~1 bucket event per
// second this is over a day of run time — a backstop, not a working limit.
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
	mu     sync.Mutex
	events []event
	subs   map[chan event]struct{}
	closed bool
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
	// The cap bounds bulk events (buckets), but terminal events must always
	// land: a client only settles on run_done, so dropping it would leave every
	// viewer of a capped (~day-long) run reconnect-looping on a finished run.
	if l.closed || (len(l.events) >= maxBufferedEvents && name != "run_done") {
		return
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
