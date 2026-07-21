package wssbridge

import (
	"context"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

func bridgeYamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = 15 * time.Second
	cfg.ConnectionWriteTimeout = 10 * time.Second
	cfg.StreamOpenTimeout = 10 * time.Second
	cfg.LogOutput = io.Discard
	return cfg
}

// copyOpaque copies bytes without inspecting or logging them. When idle is
// positive, activity in either direction refreshes one shared deadline.
func copyOpaque(ctx context.Context, a, b net.Conn, idle time.Duration) {
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
