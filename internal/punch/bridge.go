package punch

import (
	"context"
	"crypto/subtle"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/openrung/openrung/punchcore"
)

// streamAuthTimeout bounds how long the relay waits for a stream's token
// prefix before dropping it.
const streamAuthTimeout = 5 * time.Second

// RelayBridge accepts the client's QUIC connection on the punched socket and
// bridges each stream to the relay's loopback Xray listener. Every stream is
// prefixed by the punch token, verified constant-time, as defence-in-depth over
// the pinned certificate. It returns when ctx is done or the connection drops;
// the listener (and therefore the punched socket) is closed on return so a failed
// or finished punch never leaks the socket.
func RelayBridge(ctx context.Context, ln *quic.Listener, token []byte, targetHost string, targetPort int, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	defer ln.Close()

	conn, err := ln.Accept(ctx)
	if err != nil {
		return err
	}
	defer conn.CloseWithError(0, "")

	var wg sync.WaitGroup
	go func() {
		<-ctx.Done()
		_ = conn.CloseWithError(0, "")
	}()

	for {
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			wg.Wait()
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			handleRelayStream(conn, stream, token, targetHost, targetPort, logger)
		}()
	}
}

func handleRelayStream(conn *quic.Conn, stream *quic.Stream, token []byte, targetHost string, targetPort int, logger *slog.Logger) {
	defer stream.Close()

	_ = stream.SetReadDeadline(time.Now().Add(streamAuthTimeout))
	hdr := make([]byte, punchcore.TokenLen)
	if _, err := io.ReadFull(stream, hdr); err != nil {
		return
	}
	if subtle.ConstantTimeCompare(hdr, token) != 1 {
		logger.Warn("punch stream rejected: bad token")
		stream.CancelRead(0)
		return
	}
	_ = stream.SetReadDeadline(time.Time{})

	target, err := (&net.Dialer{}).Dial("tcp", net.JoinHostPort(targetHost, strconv.Itoa(targetPort)))
	if err != nil {
		logger.Warn("punch bridge dial xray failed", "error", err)
		return
	}
	defer target.Close()

	sc := &quicStreamConn{Stream: stream, local: conn.LocalAddr(), remote: conn.RemoteAddr()}
	pipe(sc, target)
}

// ClientBridge exposes a loopback TCP listener that sing-box/Xray dials in place
// of the relay endpoint. Each accepted connection becomes a QUIC stream over the
// punched connection.
type ClientBridge struct {
	conn   *quic.Conn
	token  []byte
	ln     net.Listener
	logger *slog.Logger
}

// NewClientBridge binds a loopback TCP listener and returns a bridge over conn.
func NewClientBridge(conn *quic.Conn, token []byte, logger *slog.Logger) (*ClientBridge, error) {
	if logger == nil {
		logger = slog.Default()
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	return &ClientBridge{conn: conn, token: token, ln: ln, logger: logger}, nil
}

// Endpoint returns the loopback host and port sing-box should dial.
func (b *ClientBridge) Endpoint() (host string, port int) {
	addr := b.ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port
}

// Serve accepts loopback connections until ctx is cancelled or the listener
// closes.
func (b *ClientBridge) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = b.ln.Close()
	}()
	for {
		c, err := b.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go b.handle(ctx, c)
	}
}

func (b *ClientBridge) handle(ctx context.Context, c net.Conn) {
	defer c.Close()
	stream, err := b.conn.OpenStreamSync(ctx)
	if err != nil {
		b.logger.Warn("punch bridge open stream failed", "error", err)
		return
	}
	defer stream.Close()
	if _, err := stream.Write(b.token); err != nil {
		return
	}
	sc := &quicStreamConn{Stream: stream, local: b.conn.LocalAddr(), remote: b.conn.RemoteAddr()}
	pipe(c, sc)
}

// Close shuts down the loopback listener and the punched QUIC connection.
func (b *ClientBridge) Close() error {
	_ = b.ln.Close()
	return b.conn.CloseWithError(0, "")
}

// pipe copies bytes in both directions until either side closes, then closes
// both. Local copy of the internal/tunnel helper: punchcore is a standalone
// module (internal packages of the parent module are not importable from a
// published module), so this package stays self-contained too rather than
// reaching into internal/tunnel.
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
