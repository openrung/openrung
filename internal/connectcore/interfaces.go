package connectcore

import (
	"context"
	"time"
)

// This file owns the narrow interfaces the engine needs from its host
// platform (docs/adr/001 PR A1). The desktop app implements them over its
// webview event emission, internal/clientstate, and internal/proxymode; the
// TUI client will bring its own implementations. The engine treats every one of
// them as optional unless noted, so a partially wired host degrades the same
// way the desktop service always has (no persistence, no OS proxy control).

// EventSink receives the engine's typed events. StateChanged is invoked
// synchronously under the engine's state lock, so the last state writer is
// also the last delivered and a sink can never end on a state a later write
// already superseded. Log is invoked synchronously from the emitting
// goroutine, so a log line always reaches the sink before a state change that
// follows it on the same goroutine. Implementations must be fast and must not
// call back into the engine.
type EventSink interface {
	StateChanged(state State)
	Log(entry LogEntry)
}

// LogEntry is one engine console line, stamped when it was emitted. The line
// is unformatted; presentation (timestamps, ring buffering, coalescing) is the
// sink's concern.
type LogEntry struct {
	Time time.Time
	Line string
}

// Persistence stores the small pieces of client state the engine reads and
// writes across sessions: the "recents" row and, while connected, the OS
// proxy snapshot used to recover after a crash. A nil Persistence disables
// persistence; the engine still runs (recents are session-local, crash
// recovery is skipped).
type Persistence interface {
	// LoadRecents returns the persisted recents (newest first), or an empty
	// slice when none are stored or the data is unreadable — recents are a
	// convenience, never a hard dependency.
	LoadRecents() []RecentNode
	// SaveRecents persists the recents list (best-effort).
	SaveRecents(recents []RecentNode) error
	// SaveProxySnapshot persists the OS proxy snapshot captured before a
	// connect, so a crash mid-session can be cleaned up on the next launch.
	SaveProxySnapshot(snap OSProxySnapshot) error
	// LoadProxySnapshot returns the persisted snapshot and whether one existed.
	// A present snapshot on startup means a prior session did not restore
	// cleanly.
	LoadProxySnapshot() (OSProxySnapshot, bool)
	// ClearProxySnapshot removes the snapshot after a clean restore.
	ClearProxySnapshot() error
}

// OSProxySnapshot is an opaque restorable capture of the platform's proxy
// state. The engine never inspects it — it only moves it between OSProxy and
// Persistence, so each platform keeps its own snapshot shape and on-disk
// format.
type OSProxySnapshot = any

// OSProxy sets and restores the OS system proxy for the engine's default
// (zero-privilege) proxy mode. The engine owns the policy — when to snapshot,
// set, release during a recovery gap, and restore on exit — while the
// implementation owns the platform mechanics. A nil OSProxy behaves like an
// unsupported platform: the engine advertises the loopback endpoint for
// manual configuration instead.
type OSProxy interface {
	// Supported reports whether OS proxy control works here (platform +
	// desktop environment).
	Supported() bool
	// Snapshot captures the current settings for a later Restore.
	Snapshot() (OSProxySnapshot, error)
	// Set points the OS proxy at host:port (the local mixed inbound).
	Set(host string, port int) error
	// Restore reverts to a previously captured snapshot.
	Restore(snap OSProxySnapshot) error
}

// Elevation acquires the OS privileges TUN mode needs (client.ModeTUN).
// Proxy mode never invokes it; the hook exists now so the TUN rollout
// (docs/adr/001 Track B3) lands as wiring behind Prepare rather than as an
// interface change. A nil Elevation means TUN cannot be prepared.
type Elevation interface {
	// Elevate ensures the process may create a TUN device, prompting the user
	// if the platform requires it.
	Elevate(ctx context.Context) error
}
