package tunnel

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
)

var errPortsExhausted = errors.New("no public ports available in configured range")

// PortAllocator hands out public TCP ports from an inclusive [start,end] range
// and tracks which are in use so they can be released on tunnel teardown.
type PortAllocator struct {
	mu    sync.Mutex
	start int
	end   int
	next  int
	inUse map[int]bool
}

// NewPortAllocator builds an allocator over the inclusive port range [start,end].
func NewPortAllocator(start, end int) (*PortAllocator, error) {
	if start < 1 || end > 65535 || start > end {
		return nil, fmt.Errorf("invalid port range %d-%d", start, end)
	}
	return &PortAllocator{
		start: start,
		end:   end,
		next:  start,
		inUse: make(map[int]bool),
	}, nil
}

// Allocate returns a free, bindable port from the range, or errPortsExhausted
// when every port in the range is in use. It confirms the port is bindable with
// a transient listen so the caller is unlikely to fail when it binds for real.
func (a *PortAllocator) Allocate() (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	total := a.end - a.start + 1
	for i := 0; i < total; i++ {
		port := a.next
		a.next++
		if a.next > a.end {
			a.next = a.start
		}
		if a.inUse[port] {
			continue
		}
		if !portBindable(port) {
			continue
		}
		a.inUse[port] = true
		return port, nil
	}
	return 0, errPortsExhausted
}

// Release returns a previously-allocated port to the pool.
func (a *PortAllocator) Release(port int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.inUse, port)
}

// InUse reports how many ports are currently allocated.
func (a *PortAllocator) InUse() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.inUse)
}

func portBindable(port int) bool {
	listener, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}
