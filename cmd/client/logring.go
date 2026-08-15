package main

import "sync"

// logRingCapacity bounds the Logs view's scrollback. Larger than the desktop
// webview's 80-line ring because a scrollable terminal view is the log's
// primary surface here, not a debug strip.
const logRingCapacity = 500

// logRing is the bounded engine-log buffer shared between the engine's sink
// (engine goroutines) and the TUI's coalescing tick (the Bubble Tea loop). The
// sink only appends; the model flushes snapshots at tick rate, so a chatty
// sing-box burst costs one redraw per tick instead of one event per line.
type logRing struct {
	mu    sync.Mutex
	lines []string
	max   int
	dirty bool
}

func newLogRing(capacity int) *logRing {
	return &logRing{max: capacity}
}

func (r *logRing) push(line string) {
	r.mu.Lock()
	r.lines = append(r.lines, line)
	if len(r.lines) > r.max {
		r.lines = r.lines[len(r.lines)-r.max:]
	}
	r.dirty = true
	r.mu.Unlock()
}

// snapshotIfDirty returns a copy of the lines when new ones arrived since the
// last call, clearing the dirty mark. The copy keeps the Bubble Tea loop free
// of aliasing with the sink's append.
func (r *logRing) snapshotIfDirty() ([]string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.dirty {
		return nil, false
	}
	r.dirty = false
	return append([]string(nil), r.lines...), true
}
