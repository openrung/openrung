package tunnel

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"openrung/internal/relay"
)

// Hub is the public, relay-side of the reverse tunnel. It accepts outbound
// control connections from CGNAT volunteers, authenticates them, allocates a
// public TCP port per volunteer, registers the relay with the broker, and pipes
// inbound client connections through yamux streams to the volunteer.
//
// The hub forwards opaque bytes only; it never holds the Reality private key and
// cannot decrypt the end-to-end VLESS Reality traffic.
type Hub struct {
	// ControlListener accepts volunteer control connections. In production this
	// is a TLS listener (see crypto/tls.NewListener); tests may use plain TCP.
	ControlListener net.Listener
	// PublicHost is advertised to clients as the relay's public host.
	PublicHost string
	// PublicBindHost is the interface the per-tunnel public listeners bind to.
	// Empty means all interfaces.
	PublicBindHost string
	// Allocator hands out public ports.
	Allocator *PortAllocator
	// Registrar registers and keeps-alive relays with the broker.
	Registrar Registrar
	// Token, when non-empty, is required (constant-time) in each HELLO frame.
	Token string
	// HeartbeatInterval is how often the hub re-heartbeats each live relay so
	// the broker descriptor stays within its lease TTL. Defaults to 30s.
	HeartbeatInterval time.Duration
	// HandshakeTimeout bounds the HELLO/HELLO_ACK exchange. Defaults to 10s.
	HandshakeTimeout time.Duration
	// ReflectorAddrs are the hub's UDP reflector endpoints advertised to punch-
	// capable volunteers (in HELLO_ACK and each PunchDirective). Empty means the
	// hub offers no punch coordination, so no relay is advertised punch-capable.
	ReflectorAddrs []string
	// PunchEndpoint is the hub's punch coordinator HTTP(S) base URL advertised to
	// clients in the relay descriptor (e.g. "https://203.0.113.1:9444"), so the
	// client hits the right scheme/host/port instead of deriving one.
	PunchEndpoint string
	// Logger defaults to slog.Default().
	Logger *slog.Logger

	// registry maps a live relay ID to its tunnel so the punch coordinator can
	// push a directive over the existing control connection. Guarded by
	// registryMu; entries use compare-and-delete so a fast reconnect (which gets
	// a fresh relay ID) never lets an old teardown evict the new live tunnel.
	registryMu sync.RWMutex
	registry   map[string]*tunnel
}

// Serve accepts control connections until ctx is cancelled or the listener fails.
func (h *Hub) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = h.ControlListener.Close()
	}()
	for {
		conn, err := h.ControlListener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept control connection: %w", err)
		}
		go h.handleControl(ctx, conn)
	}
}

func (h *Hub) handleControl(ctx context.Context, conn net.Conn) {
	logger := h.logger()
	remote := conn.RemoteAddr().String()

	_ = conn.SetDeadline(time.Now().Add(h.handshakeTimeout()))
	var hello HelloFrame
	if err := readFrame(conn, &hello); err != nil {
		logger.Warn("tunnel handshake read failed", "remote", remote, "error", err)
		_ = conn.Close()
		return
	}

	if hello.ProtocolVersion != ProtocolVersion {
		h.rejectAndClose(conn, errProtocolMismatch.Error())
		logger.Warn("tunnel protocol mismatch", "remote", remote, "version", hello.ProtocolVersion)
		return
	}
	if !h.authorized(hello.Token) {
		h.rejectAndClose(conn, "invalid tunnel token")
		logger.Warn("tunnel auth failed", "remote", remote)
		return
	}

	port, err := h.Allocator.Allocate()
	if err != nil {
		h.rejectAndClose(conn, "no public ports available")
		logger.Warn("tunnel port allocation failed", "remote", remote, "error", err)
		return
	}

	// Open the public listener before registering so we never advertise an
	// endpoint we cannot actually serve.
	publicListener, err := net.Listen("tcp", net.JoinHostPort(h.PublicBindHost, strconv.Itoa(port)))
	if err != nil {
		h.Allocator.Release(port)
		h.rejectAndClose(conn, "could not open public port")
		logger.Warn("tunnel public listen failed", "remote", remote, "port", port, "error", err)
		return
	}

	registration, err := h.Registrar.Register(ctx, h.registerRequest(hello, port))
	if err != nil {
		_ = publicListener.Close()
		h.Allocator.Release(port)
		h.rejectAndClose(conn, "broker registration failed")
		logger.Warn("tunnel broker registration failed", "remote", remote, "port", port, "error", err)
		return
	}

	// Negotiate stream typing: the hub supports it, so it is on iff the volunteer
	// asked for it. Only a typed session can carry punch-control streams.
	streamTyping := hello.StreamTyping
	ack := HelloAckFrame{
		OK:           true,
		PublicHost:   h.PublicHost,
		PublicPort:   port,
		RelayID:      registration.RelayID,
		StreamTyping: streamTyping,
	}
	if streamTyping {
		ack.ReflectorAddrs = h.ReflectorAddrs
	}
	if err := writeFrame(conn, ack); err != nil {
		_ = publicListener.Close()
		h.Allocator.Release(port)
		logger.Warn("tunnel hello ack failed", "remote", remote, "error", err)
		_ = conn.Close()
		return
	}
	_ = conn.SetDeadline(time.Time{})

	session, err := yamux.Server(conn, yamuxConfig())
	if err != nil {
		_ = publicListener.Close()
		h.Allocator.Release(port)
		logger.Warn("tunnel yamux server failed", "remote", remote, "error", err)
		_ = conn.Close()
		return
	}

	t := &tunnel{
		hub:            h,
		session:        session,
		publicListener: publicListener,
		port:           port,
		relayID:        registration.RelayID,
		streamTyping:   streamTyping,
		logger:         logger.With("relay_id", registration.RelayID, "public_port", port, "remote", remote),
	}
	// Publish before the blocking accept loop so the punch coordinator can find
	// this volunteer as soon as it is registered.
	h.addTunnel(registration.RelayID, t)
	defer h.removeTunnel(registration.RelayID, t)
	t.run(ctx)
}

func (h *Hub) registerRequest(hello HelloFrame, port int) relay.RegisterRequest {
	punchOn := h.punchAvailable() && hello.StreamTyping && hello.PunchCapable
	punchEndpoint := ""
	if punchOn {
		punchEndpoint = h.PunchEndpoint
	}
	return relay.RegisterRequest{
		PublicHost:       h.PublicHost,
		PublicPort:       port,
		Protocol:         relay.ProtocolVLESSRealityVision,
		ClientID:         hello.ClientID,
		RealityPublicKey: hello.RealityPublicKey,
		ShortID:          hello.ShortID,
		ServerName:       hello.ServerName,
		Flow:             hello.Flow,
		ExitMode:         hello.ExitMode,
		MaxSessions:      hello.MaxSessions,
		MaxMbps:          hello.MaxMbps,
		VolunteerVersion: hello.VolunteerVersion,
		Label:            hello.Label,
		Transport:        relay.TransportTunnel,
		PunchCapable:     punchOn,
		PunchEndpoint:    punchEndpoint,
	}
}

// punchAvailable reports whether this hub is configured to coordinate punches.
func (h *Hub) punchAvailable() bool {
	return len(h.ReflectorAddrs) > 0
}

func (h *Hub) authorized(token string) bool {
	if h.Token == "" {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(h.Token)) == 1
}

func (h *Hub) rejectAndClose(conn net.Conn, message string) {
	_ = writeFrame(conn, HelloAckFrame{OK: false, Error: message})
	_ = conn.Close()
}

func (h *Hub) logger() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

func (h *Hub) handshakeTimeout() time.Duration {
	if h.HandshakeTimeout > 0 {
		return h.HandshakeTimeout
	}
	return 10 * time.Second
}

func (h *Hub) heartbeatInterval() time.Duration {
	if h.HeartbeatInterval > 0 {
		return h.HeartbeatInterval
	}
	return 30 * time.Second
}

// tunnel is one live volunteer connection: a yamux session plus the public
// listener whose inbound connections are multiplexed to the volunteer.
type tunnel struct {
	hub            *Hub
	session        *yamux.Session
	publicListener net.Listener
	port           int
	relayID        string
	streamTyping   bool
	logger         *slog.Logger
}

func (t *tunnel) run(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		t.heartbeatLoop(ctx)
	}()

	// Tear down when the yamux session dies (volunteer disconnected) or the
	// parent context is cancelled (hub shutting down).
	go func() {
		select {
		case <-ctx.Done():
		case <-t.session.CloseChan():
		}
		cancel()
		_ = t.publicListener.Close()
		_ = t.session.Close()
	}()

	t.logger.Info("tunnel ready")
	for {
		clientConn, err := t.publicListener.Accept()
		if err != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			t.handleClient(clientConn)
		}()
	}

	cancel()
	_ = t.session.Close()
	wg.Wait()
	t.hub.Allocator.Release(t.port)
	t.logger.Info("tunnel closed")
}

func (t *tunnel) handleClient(clientConn net.Conn) {
	defer clientConn.Close()
	stream, err := t.session.Open()
	if err != nil {
		t.logger.Warn("open tunnel stream failed", "error", err)
		return
	}
	defer stream.Close()
	// With stream typing negotiated, prefix client-data streams with the data
	// discriminator so the volunteer can distinguish them from punch-control
	// streams. Untyped sessions (old volunteers) get raw bytes as before.
	if t.streamTyping {
		if _, err := stream.Write([]byte{StreamTypeData}); err != nil {
			t.logger.Warn("write stream type failed", "error", err)
			return
		}
	}
	pipe(clientConn, stream)
}

func (t *tunnel) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(t.hub.heartbeatInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hbCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := t.hub.Registrar.Heartbeat(hbCtx, t.relayID)
			cancel()
			if err != nil {
				t.logger.Warn("relay heartbeat failed", "error", err)
			}
		}
	}
}
