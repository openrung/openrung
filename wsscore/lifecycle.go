package wsscore

import (
	"sync"
	"time"
)

// IdleGuard expires a session only while it has no active streams. It is safe
// for concurrent stream start/end calls and owns at most one timer.
type IdleGuard struct {
	mu         sync.Mutex
	timeout    time.Duration
	active     int
	expired    bool
	closed     bool
	generation uint64
	timer      *time.Timer
	onExpire   func()
}

func NewIdleGuard(timeout time.Duration, onExpire func()) (*IdleGuard, error) {
	bounded, err := boundedDuration(timeout, DefaultNoStreamIdleTimeout, MaxSessionLifetime, "no-stream idle timeout")
	if err != nil {
		return nil, err
	}
	guard := &IdleGuard{timeout: bounded, onExpire: onExpire}
	guard.scheduleLocked()
	return guard, nil
}

// Start marks one stream active. It returns false once the guard has expired
// or closed, so callers can reject a stream racing session teardown.
func (g *IdleGuard) Start() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.expired {
		return false
	}
	if g.active == 0 {
		g.generation++
		if g.timer != nil {
			g.timer.Stop()
		}
	}
	g.active++
	return true
}

// Done marks one stream complete and restarts the idle window after the last
// active stream exits.
func (g *IdleGuard) Done() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.active == 0 {
		return
	}
	g.active--
	if g.active == 0 {
		g.scheduleLocked()
	}
}

func (g *IdleGuard) Close() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	g.generation++
	if g.timer != nil {
		g.timer.Stop()
	}
}

func (g *IdleGuard) scheduleLocked() {
	g.generation++
	generation := g.generation
	g.timer = time.AfterFunc(g.timeout, func() { g.expire(generation) })
}

func (g *IdleGuard) expire(generation uint64) {
	g.mu.Lock()
	if g.closed || g.expired || g.active != 0 || generation != g.generation {
		g.mu.Unlock()
		return
	}
	g.expired = true
	onExpire := g.onExpire
	g.mu.Unlock()
	if onExpire != nil {
		onExpire()
	}
}
