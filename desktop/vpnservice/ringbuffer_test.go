package vpnservice

import (
	"strconv"
	"testing"
)

func TestRingBufferCapsAtCapacityNewestLast(t *testing.T) {
	r := newRingBuffer(80)
	for i := 0; i < 100; i++ {
		r.push(strconv.Itoa(i))
	}
	lines := r.snapshot()
	if len(lines) != 80 {
		t.Fatalf("expected 80 lines, got %d", len(lines))
	}
	// Oldest 20 dropped; newest is last.
	if lines[0] != "20" {
		t.Fatalf("oldest retained line = %q, want 20", lines[0])
	}
	if lines[len(lines)-1] != "99" {
		t.Fatalf("newest line = %q, want 99", lines[len(lines)-1])
	}
}

func TestRingBufferSnapshotIsCopy(t *testing.T) {
	r := newRingBuffer(4)
	r.push("a")
	snap := r.snapshot()
	r.push("b")
	if len(snap) != 1 {
		t.Fatalf("snapshot mutated after later push: %v", snap)
	}
}
