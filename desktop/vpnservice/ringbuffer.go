package vpnservice

// ringBuffer holds the most recent log lines, newest last, capped at capacity —
// matching the NativeVpnState.logLines contract (cap 80, newest last). It is not
// safe for concurrent use; the Service holds its mutex around every access.
type ringBuffer struct {
	lines    []string
	capacity int
}

func newRingBuffer(capacity int) *ringBuffer {
	return &ringBuffer{capacity: capacity}
}

func (r *ringBuffer) push(line string) {
	r.lines = append(r.lines, line)
	if len(r.lines) > r.capacity {
		r.lines = r.lines[len(r.lines)-r.capacity:]
	}
}

// snapshot returns a copy safe to hand out beyond the lock.
func (r *ringBuffer) snapshot() []string {
	out := make([]string, len(r.lines))
	copy(out, r.lines)
	return out
}
