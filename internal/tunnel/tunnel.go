// Package tunnel implements a minimal reverse tunnel that lets a relay
// behind CGNAT serve client traffic without any inbound port.
//
// A relay Client dials a publicly reachable relay hub (Hub) over a single
// outbound TLS connection, authenticates, and announces its relay metadata. The
// hub allocates a public TCP port, registers the relay with the broker, and then
// multiplexes inbound client connections over the same connection using yamux:
// for each client connection on the public port the hub opens a stream that the
// relay pipes to its loopback Xray listener.
//
// The hub only copies opaque bytes between the public connection and the stream;
// it never holds the Reality private key, so it cannot decrypt the end-to-end
// VLESS Reality traffic.
package tunnel

import (
	"io"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// yamuxConfig returns the shared multiplexer configuration used by both ends.
// Keepalive lets either side detect a dead CGNAT tunnel without a separate
// application heartbeat on the wire.
func yamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = 15 * time.Second
	cfg.ConnectionWriteTimeout = 10 * time.Second
	cfg.LogOutput = io.Discard
	return cfg
}

// pipe copies bytes in both directions between two connections until either side
// closes, then closes both. Mirrors the forwarding pattern in
// internal/relayruntime/connection_observer.go.
func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = copyHalf(a, b)
		_ = a.Close()
	}()
	go func() {
		defer wg.Done()
		_, _ = copyHalf(b, a)
		_ = b.Close()
	}()
	wg.Wait()
}

// copyBuffers recycles the 32 KiB buffers the tunnel copies through. Every
// forwarded connection has one yamux side, which offers neither ReaderFrom
// nor WriterTo, so io.Copy would otherwise allocate a fresh buffer for each
// direction of each connection — most of a short connection's garbage.
var copyBuffers = sync.Pool{New: func() any {
	buf := make([]byte, 32<<10)
	return &buf
}}

// copyHalf copies src to dst through a pooled buffer. The wrappers hide the
// TCP side's ReaderFrom/WriterTo: with a yamux stream on the other side they
// cannot splice, and would fall back to allocating a buffer of their own.
func copyHalf(dst io.Writer, src io.Reader) (int64, error) {
	buf := copyBuffers.Get().(*[]byte)
	defer copyBuffers.Put(buf)
	return io.CopyBuffer(struct{ io.Writer }{dst}, struct{ io.Reader }{src}, *buf)
}
