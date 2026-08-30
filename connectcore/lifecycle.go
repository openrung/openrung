package connectcore

import (
	"context"
	"time"
)

// This file owns the explicit lifecycle surface (ADR-003 A2): mobile hosts
// live inside OS-managed process lifecycles — iOS packet tunnels sleep, wake,
// and are jetsam-killed on a hard budget; Android services are killable — so
// the engine exposes Pause/Resume for suspension and Shutdown with a
// caller-owned flush budget for bounded teardown. Desktop and the TUI keep
// calling Stop, which is Shutdown with the engine's defaults.

// Pause suspends the engine's periodic, self-initiated activity: the
// mid-session health sweeps, the telemetry heartbeat, and the automatic
// recovery reactions (a failover trigger or network epoch that fires while
// paused is held, not lost, and handled on Resume). The tunnel run itself is
// left untouched — whether the data plane sleeps belongs to the host and the
// core. In-flight operations complete rather than being interrupted, and a
// connect or recovery pass already in flight runs to its own conclusion.
// Pause never blocks Disconnect, Stop, or Shutdown: teardown proceeds while
// paused. Idempotent.
func (s *Engine) Pause() {
	s.pauseMu.Lock()
	paused := s.resumedCh != nil
	if !paused {
		s.resumedCh = make(chan struct{})
	}
	s.pauseMu.Unlock()
	if !paused {
		s.appendLog("engine paused")
	}
}

// Resume lifts Pause. Anything the pause held is handled immediately — a due
// health sweep runs right away, a failover trigger or network epoch received
// mid-pause starts its recovery now — matching the mobile monitors' wake
// semantics: device wake only resumes the engine, recovery policy stays in
// the engine. Idempotent.
func (s *Engine) Resume() {
	s.pauseMu.Lock()
	resumed := s.resumedCh
	s.resumedCh = nil
	s.pauseMu.Unlock()
	if resumed != nil {
		close(resumed)
		s.appendLog("engine resumed")
	}
}

// awaitResumed blocks while the engine is paused, returning false when ctx
// ended first — which is how a teardown interrupts a paused wait.
func (s *Engine) awaitResumed(ctx context.Context) bool {
	for {
		s.pauseMu.Lock()
		resumed := s.resumedCh
		s.pauseMu.Unlock()
		if resumed == nil {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-resumed:
		}
	}
}

// Shutdown stops any live session the way Stop does, with the terminal
// telemetry flush bounded by flushBudget (zero or negative keeps the engine's
// 5-second default) and that flush's outcome reported — for hosts tearing
// down on a platform-owned budget (the iOS extension's jetsam window), where
// the flush is the one step worth spending remaining time on but never more.
//
// The guaranteed ordering, all before Shutdown returns:
//
//  1. The tunnel run is stopped through the TunnelRuntime seam and the OS
//     proxy is restored — user traffic is never left pointed at a dead
//     tunnel, whatever happens to telemetry.
//  2. The terminal state (disconnected) reaches the sink.
//  3. The session-end events are recorded and the outbox is flushed, bounded
//     by flushBudget. This is the only step the budget may cut short; events
//     that do not flush in time are dropped with the process (the bound
//     outbox's cancellation contract — its disk-backed durability arrives
//     with A3).
//
// The in-flight geo lookup is cancelled, never awaited — telemetry must not
// delay teardown. Shutdown works while paused (Pause never blocks teardown)
// and is safe with no live session, returning nil.
func (s *Engine) Shutdown(flushBudget time.Duration) error {
	s.connectMu.Lock()
	defer s.connectMu.Unlock()
	s.mu.Lock()
	conn := s.conn
	var inFlightFlush context.CancelFunc
	if conn != nil {
		conn.flushBudget = flushBudget
		inFlightFlush = conn.flushCancel
	}
	s.mu.Unlock()
	if conn == nil {
		return nil
	}
	if inFlightFlush != nil {
		// The terminal flush is already running — a Disconnect or a natural
		// failure started it with an earlier budget. Shorten it to ours:
		// from this call, the flush gets at most flushBudget more.
		budget := flushBudget
		if budget <= 0 {
			budget = defaultFlushBudget
		}
		shorten := time.AfterFunc(budget, inFlightFlush)
		defer shorten.Stop()
	}
	s.teardownExisting()
	s.mu.Lock()
	defer s.mu.Unlock()
	return conn.flushErr
}
