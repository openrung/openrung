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
// internal/volunteer/connection_observer.go.
func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(a, b)
		_ = a.Close()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(b, a)
		_ = b.Close()
	}()
	wg.Wait()
}
