package wsscore

import (
	"context"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

func yamuxConfig() *yamux.Config {
	return &yamux.Config{
		AcceptBacklog:          256,
		EnableKeepAlive:        true,
		KeepAliveInterval:      15 * time.Second,
		ConnectionWriteTimeout: 10 * time.Second,
		MaxStreamWindowSize:    256 << 10,
		StreamOpenTimeout:      10 * time.Second,
		StreamCloseTimeout:     5 * time.Minute,
		LogOutput:              io.Discard,
	}
}

// NewClientSession creates the client half of the one WSS yamux profile.
func NewClientSession(conn net.Conn) (*yamux.Session, error) {
	return yamux.Client(conn, yamuxConfig())
}

// NewServerSession creates the relay half of the one WSS yamux profile.
func NewServerSession(conn net.Conn) (*yamux.Session, error) {
	return yamux.Server(conn, yamuxConfig())
}

// CopyOpaque copies bytes in both directions without inspecting or logging
// them. When idle is positive, activity in either direction refreshes one
// shared deadline. Completion closes both connections.
func CopyOpaque(ctx context.Context, a, b net.Conn, idle time.Duration) {
	var deadlineMu sync.Mutex
	touch := func() {
		if idle <= 0 {
			return
		}
		deadlineMu.Lock()
		deadline := time.Now().Add(idle)
		_ = a.SetDeadline(deadline)
		_ = b.SetDeadline(deadline)
		deadlineMu.Unlock()
	}
	touch()

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = a.Close()
			_ = b.Close()
		case <-done:
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	copyOne := func(dst, src net.Conn) {
		defer wg.Done()
		buffer := make([]byte, 32<<10)
		for {
			n, readErr := src.Read(buffer)
			if n > 0 {
				touch()
				if _, writeErr := writeAll(dst, buffer[:n]); writeErr != nil {
					_ = dst.Close()
					_ = src.Close()
					return
				}
				touch()
			}
			if readErr != nil {
				if half, ok := dst.(interface{ CloseWrite() error }); ok {
					_ = half.CloseWrite()
				} else {
					_ = dst.Close()
				}
				return
			}
		}
	}
	go copyOne(a, b)
	go copyOne(b, a)
	wg.Wait()
	close(done)
	_ = a.Close()
	_ = b.Close()
}

func writeAll(w io.Writer, data []byte) (int, error) {
	written := 0
	for len(data) > 0 {
		n, err := w.Write(data)
		written += n
		data = data[n:]
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}
