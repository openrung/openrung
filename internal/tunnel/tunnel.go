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

// tunnelStreamWindow is each stream's receive window. A stream carries at
// most one window per round trip, so yamux's 256 KiB default caps a client
// connection at about 1.3 MB/s over a 200 ms hub ↔ relay path; 1 MiB lifts
// that fourfold. The receiver grants the window, so each end sets it for the
// data it receives and peers on the old default interoperate. The cost is
// buffering: a stream whose reader stalls holds up to one window, bounded per
// tunnel by maxConcurrentConns.
const tunnelStreamWindow = 1 << 20

// yamuxConfig returns the shared multiplexer configuration used by both ends.
// Keepalive lets either side detect a dead CGNAT tunnel without a separate
// application heartbeat on the wire.
func yamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = 15 * time.Second
	cfg.ConnectionWriteTimeout = 10 * time.Second
	cfg.MaxStreamWindowSize = tunnelStreamWindow
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
