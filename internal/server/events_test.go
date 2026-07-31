package server

import "testing"

// A malformed Last-Event-ID can parse to a negative number; subscribe must not
// index the event slice negatively (which would panic the SSE handler).
func TestSubscribeNegativeAfter(t *testing.T) {
	l := newEventLog()
	l.emit("hello", nil)
	l.emit("bucket", nil)

	replay, ch, cancel := l.subscribe(-5) // negative clamps to 0: full replay
	defer cancel()
	if len(replay) != 2 {
		t.Fatalf("negative after replayed %d events, want 2 (clamped to 0)", len(replay))
	}
	if ch == nil {
		t.Fatal("open log should return a live channel")
	}
}

// The buffer cap may drop only bulk bucket events. Lifecycle events (they are
// O(levels+targets)) must always land even past the cap: a dropped
// level_result would let the race podium score an earlier level as final, and
// a dropped run_done would strand every viewer reconnect-looping.
func TestEventLogCapDropsOnlyBuckets(t *testing.T) {
	l := newEventLog()
	for range maxBufferedEvents {
		l.emit("bucket", 1)
	}
	l.emit("bucket", 2) // over the cap: dropped
	l.emit("level_start", map[string]any{"level_index": 4})
	l.emit("level_result", map[string]any{"level_index": 4})
	l.emit("run_done", map[string]any{"state": "done"})

	replay, _, cancel := l.subscribe(int64(maxBufferedEvents))
	defer cancel()
	var names []string
	for _, e := range replay {
		names = append(names, e.name)
	}
	want := []string{"level_start", "level_result", "run_done"}
	if len(names) != len(want) {
		t.Fatalf("events past the cap = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("events past the cap = %v, want %v", names, want)
		}
	}
}
