package connectcore

import (
	"context"
	"time"
)

// This file owns the narrow interfaces the engine needs from its host
// platform (ADR-001 PR A1). The desktop app implements them over its
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

// NoticeSink is an optional extension of EventSink for hosts that surface the
// engine's mid-flow events as structured UI state rather than parsing log
// lines. A sink that does not implement it (the desktop webview sink) simply
// never receives notices; the same log lines still describe every one of
// them. Notice is invoked synchronously from the emitting goroutine under the
// same rules as Log: be fast, never call back into the engine.
type NoticeSink interface {
	Notice(notice Notice)
}

// NoticeKind names one typed engine event (see Notice for the fields each
// kind fills in).
type NoticeKind string

const (
	// NoticeFailoverStarted: the live tunnel was lost and the automatic
	// recovery re-ladder is running. FromRelayID is the relay that carried the
	// session; Reason says what triggered the failover.
	NoticeFailoverStarted NoticeKind = "failover_started"
	// NoticeFailoverCompleted: the recovery promoted a new relay. RelayID is
	// the winner, FromRelayID the relay it replaced, and FrontID is set when
	// the new path runs through a WSS front.
	NoticeFailoverCompleted NoticeKind = "failover_completed"
	// NoticeWSSFallback: the direct path to RelayID failed and its signed WSS
	// front FrontID is being attempted. Reason carries the direct failure.
	NoticeWSSFallback NoticeKind = "wss_fallback"
	// NoticeWSSTicketRetry: every eligible broker front rate-limited the WSS
	// session ticket; the one bounded retry is waiting Wait.
	NoticeWSSTicketRetry NoticeKind = "wss_ticket_retry"
	// NoticePunchOutcome: the NAT punch attempt against RelayID finished.
	// Reason is the human-readable outcome (success includes the NAT class,
	// failure the reason and the hub fallback).
	NoticePunchOutcome NoticeKind = "punch_outcome"
	// NoticeHealthProbe: one mid-session health sweep finished. Failures is
	// the consecutive-failure count (0 means the sweep passed) out of
	// Threshold; Reason is set when a failed sweep did not trigger a failover
	// because the local network itself looks down.
	NoticeHealthProbe NoticeKind = "health_probe"
)

// Notice is one typed mid-flow engine event for host status UIs. Only the
// fields the Kind documents are meaningful; everything else is zero.
type Notice struct {
	Kind        NoticeKind
	RelayID     string
	FromRelayID string
	FrontID     string
	Reason      string
	Wait        time.Duration
	Failures    int
	Threshold   int
}

// Persistence stores the small pieces of client state the engine reads and
// writes across sessions: the "recents" row, the stable local proxy port, and,
// while connected, the OS proxy snapshot used to recover after a crash. A nil
// Persistence disables persistence; the engine still runs (recents are
// session-local, crash recovery is skipped, and the local endpoint is picked
// fresh each launch with a warning).
type Persistence interface {
	// LoadRecents returns the persisted recents (newest first), or an empty
	// slice when none are stored or the data is unreadable — recents are a
	// convenience, never a hard dependency.
	LoadRecents() []RecentNode
	// SaveRecents persists the recents list (best-effort).
	SaveRecents(recents []RecentNode) error
	LoadProxyPort() (int, bool)
	// LoadOrSaveProxyPort persists candidate and returns the port that won —
	// another process's choice when two first launches race, so every process
	// agrees on the endpoint.
	LoadOrSaveProxyPort(candidate int) (int, error)
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

// Elevation acquires the OS privileges TUN mode needs (see Mode). Proxy mode
// never invokes it. A nil Elevation means TUN cannot be prepared: the engine
// refuses the connect rather than let sing-box fail opaquely after the ladder
// has already dialed relays.
//
// A host that cannot acquire privileges it was not started with — the terminal
// client, which would have to re-exec itself under sudo and detach the tunnel
// from the terminal that owns it — returns an error naming the way to rerun.
// That error is what the user sees, so it carries the guidance.
//
// This is also where a host declares TUN mode unavailable for reasons other
// than privilege. A platform on which the host cannot stop the tunnel process
// gracefully must refuse here: a forced stop leaves the routes and DNS
// sing-box installed pointing at a device that is gone, which is worse than
// never having offered the mode (see TUNKillGrace).
type Elevation interface {
	// Elevate ensures the process may create a TUN device, prompting the user
	// if the platform requires it.
	Elevate(ctx context.Context) error
}
