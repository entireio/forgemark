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
