// SPDX-License-Identifier: GPL-3.0-or-later

package tunnel

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

func TestYamuxConfigStreamWindow(t *testing.T) {
	cfg := yamuxConfig()
	if err := yamux.VerifyConfig(cfg); err != nil {
		t.Fatalf("tunnel yamux config is invalid: %v", err)
	}
	if cfg.MaxStreamWindowSize != tunnelStreamWindow {
		t.Fatalf("MaxStreamWindowSize = %d, want %d", cfg.MaxStreamWindowSize, tunnelStreamWindow)
	}
}

// latencyLink joins two connections through a one-way delay in each
// direction, standing in for the hub ↔ relay path. Bandwidth is unbounded, so
// a stream's throughput is bound only by its window and the round trip.
func latencyLink(tb testing.TB, oneWay time.Duration) (net.Conn, net.Conn) {
	tb.Helper()
	near, farIn := net.Pipe()
	farOut, far := net.Pipe()
	forward := func(src, dst net.Conn) {
		type chunk struct {
			data []byte
			at   time.Time
		}
		queue := make(chan chunk, 4096)
		go func() {
			defer close(queue)
			buf := make([]byte, 64<<10)
			for {
				n, err := src.Read(buf)
				if n > 0 {
					queue <- chunk{append([]byte(nil), buf[:n]...), time.Now().Add(oneWay)}
				}
				if err != nil {
					return
				}
			}
		}()
		go func() {
			defer dst.Close()
			for c := range queue {
				time.Sleep(time.Until(c.at))
				if _, err := dst.Write(c.data); err != nil {
					return
				}
			}
		}()
	}
	forward(farIn, farOut)
	forward(farOut, farIn)
	tb.Cleanup(func() {
		_ = near.Close()
		_ = far.Close()
	})
	return near, far
}

// BenchmarkTunnelStreamOverLatency reports one stream's download throughput
// (relay → hub, the direction the hub's window governs) at a 100 ms round
// trip, with both ends current and with a relay still on yamux's default.
func BenchmarkTunnelStreamOverLatency(b *testing.B) {
	legacy := yamuxConfig()
	legacy.MaxStreamWindowSize = yamux.DefaultConfig().MaxStreamWindowSize
	b.Run("current-relay", func(b *testing.B) { benchmarkTunnelStream(b, yamuxConfig()) })
	b.Run("legacy-relay", func(b *testing.B) { benchmarkTunnelStream(b, legacy) })
}

func benchmarkTunnelStream(b *testing.B, relayConfig *yamux.Config) {
	hubConn, relayConn := latencyLink(b, 50*time.Millisecond)
	hub, err := yamux.Server(hubConn, yamuxConfig())
	if err != nil {
		b.Fatal(err)
	}
	relaySide, err := yamux.Client(relayConn, relayConfig)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		_ = hub.Close()
		_ = relaySide.Close()
	})

	const transfer = 8 << 20
	payload := make([]byte, transfer)
	b.SetBytes(transfer)
	b.ResetTimer()
	for range b.N {
		accepted := make(chan net.Conn, 1)
		go func() {
			stream, err := relaySide.Accept()
			if err != nil {
				accepted <- nil
				return
			}
			accepted <- stream
		}()
		hubStream, err := hub.Open()
		if err != nil {
			b.Fatal(err)
		}
		if _, err := hubStream.Write([]byte{0}); err != nil {
			b.Fatal(err)
		}
		relayStream := <-accepted
		if relayStream == nil {
			b.Fatal("accept failed")
		}
		go func() {
			_, _ = relayStream.Write(payload)
			_ = relayStream.Close()
		}()
		if _, err := io.CopyN(io.Discard, hubStream, transfer); err != nil {
			b.Fatal(err)
		}
		_ = hubStream.Close()
	}
}
