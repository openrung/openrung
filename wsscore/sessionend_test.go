package wsscore

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// holdingEdge upgrades one WSS session, holds it, and then ends it the way the
// named mode says. It mirrors the relay sidecar: an idle expiry there closes
// the yamux session and the WebSocket, which emits an orderly close frame.
func holdingEdge(t *testing.T, hold time.Duration, mode string) *localEdge {
	t.Helper()
	upgrader := websocket.Upgrader{
		Subprotocols: []string{Subprotocol}, EnableCompression: false,
		CheckOrigin: func(*http.Request) bool { return true },
	}
	return newLocalEdge(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		conn, err := NewWebSocketConn(ws, DefaultWebSocketReadMax)
		if err != nil {
			return
		}
		session, err := NewServerSession(conn)
		if err != nil {
			return
		}
		if mode == "hold" {
			<-r.Context().Done()
			_ = session.Close()
			_ = conn.Close()
			return
		}
		time.Sleep(hold)
		switch mode {
		case "orderly":
			_ = session.Close()
			_ = conn.Close()
		case "abrupt":
			// No close frame: the shape of a censored or dropped path.
			_ = ws.UnderlyingConn().Close()
		}
	}))
}

func dialHeldClient(t *testing.T, edge *localEdge, lifecycle LifecycleOptions) *Client {
	t.Helper()
	if lifecycle.SessionLifetime == 0 {
		lifecycle.SessionLifetime = time.Minute
	}
	client, err := DialClient(context.Background(), ClientOptions{
		URL: "wss://example.com" + BridgePath, Ticket: "opaque-ticket",
		WebSocketDialer: edge.dialer, PingInterval: -1, Lifecycle: lifecycle,
	})
	if err != nil {
		t.Fatalf("DialClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestSessionEndSeparatesOrderlyCloseFromPathLoss is the regression this
// package was missing: Serve returns a nil error for BOTH an orderly peer
// shutdown and a lost path, so a caller that infers loss from Serve returning
// reports every orderly end as a broken path.
func TestSessionEndSeparatesOrderlyCloseFromPathLoss(t *testing.T) {
	for name, expected := range map[string]struct {
		mode     string
		end      SessionEnd
		graceful bool
	}{
		"orderly peer close": {mode: "orderly", end: SessionEndRemote, graceful: true},
		"lost path":          {mode: "abrupt", end: SessionEndTransport, graceful: false},
	} {
		t.Run(name, func(t *testing.T) {
			client := dialHeldClient(t, holdingEdge(t, 150*time.Millisecond, expected.mode), LifecycleOptions{})
			serveErr := make(chan error, 1)
			go func() { serveErr <- client.Serve(context.Background()) }()

			select {
			case err := <-serveErr:
				if err != nil {
					t.Fatalf("Serve returned %v, want nil for every session end", err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("Serve did not return after the session ended")
			}
			if got := client.SessionEnd(); got != expected.end {
				t.Fatalf("SessionEnd = %v, want %v", got, expected.end)
			}
			if got := client.SessionEnd().Graceful(); got != expected.graceful {
				t.Fatalf("Graceful = %t, want %t", got, expected.graceful)
			}
		})
	}
}

// TestSessionEndReportsABlackholedPathAsLoss keeps the fix from suppressing what
// it exists to expose. A censor usually drops packets rather than closing
// anything, so no close frame ever arrives and only yamux's keepalive notices.
// That must still be reported as loss, not as an orderly end.
func TestSessionEndReportsABlackholedPathAsLoss(t *testing.T) {
	if testing.Short() {
		// Detection waits on yamux's fixed 15s keepalive; there is no faster
		// signal, because a blackhole produces no signal at all.
		t.Skip("blackhole detection waits on the yamux keepalive interval")
	}
	edge := holdingEdge(t, 0, "hold")
	blackhole := newBlackholeProxy(t, edge.server.Listener.Addr().String())
	dialer := *edge.dialer
	dialer.NetDialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, blackhole.address())
	}

	client, err := DialClient(context.Background(), ClientOptions{
		URL: "wss://example.com" + BridgePath, Ticket: "opaque-ticket",
		WebSocketDialer: &dialer, PingInterval: -1,
		Lifecycle: LifecycleOptions{SessionLifetime: 10 * time.Minute},
	})
	if err != nil {
		t.Fatalf("DialClient: %v", err)
	}
	defer client.Close()
	serveErr := make(chan error, 1)
	go func() { serveErr <- client.Serve(context.Background()) }()

	// Stop forwarding in both directions while leaving both sockets open.
	blackhole.drop()

	select {
	case <-serveErr:
	case <-time.After(90 * time.Second):
		t.Fatal("a blackholed path never ended the session")
	}
	if got := client.SessionEnd(); got != SessionEndTransport {
		t.Fatalf("SessionEnd = %v, want %v", got, SessionEndTransport)
	}
	if client.SessionEnd().Graceful() {
		t.Fatal("a blackholed path was reported as an orderly end")
	}
	if code := client.RemoteCloseCode(); code == websocket.CloseNormalClosure {
		t.Fatalf("a blackholed path reported an orderly close code %d", code)
	}
}

// blackholeProxy forwards TCP until drop, then silently discards everything
// while holding both sockets open — the shape of a censored path.
type blackholeProxy struct {
	listener net.Listener
	dropped  chan struct{}
	once     sync.Once
}

func newBlackholeProxy(t *testing.T, upstream string) *blackholeProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &blackholeProxy{listener: listener, dropped: make(chan struct{})}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			downstream, err := listener.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", upstream)
			if err != nil {
				_ = downstream.Close()
				continue
			}
			go proxy.forward(downstream, up)
			go proxy.forward(up, downstream)
		}
	}()
	return proxy
}

func (p *blackholeProxy) forward(dst, src net.Conn) {
	buffer := make([]byte, 32<<10)
	for {
		n, err := src.Read(buffer)
		select {
		case <-p.dropped:
			// Keep both sockets open and read on, discarding everything.
			if err != nil {
				return
			}
			continue
		default:
		}
		if n > 0 {
			if _, writeErr := dst.Write(buffer[:n]); writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (p *blackholeProxy) address() string { return p.listener.Addr().String() }

func (p *blackholeProxy) drop() { p.once.Do(func() { close(p.dropped) }) }

// TestClientLeavesNoStreamIdleGuardDisarmedByDefault pins the fix for a client
// closing its own idle session and reporting it as an unexpected loss. A VPN
// holds an idle tunnel open; nothing may arm that guard unless a caller asks.
func TestClientLeavesNoStreamIdleGuardDisarmedByDefault(t *testing.T) {
	client := dialHeldClient(t, holdingEdge(t, 0, "hold"), LifecycleOptions{})
	if client.idle != nil {
		t.Fatal("client armed a no-stream idle guard without being asked")
	}

	lifecycle, err := NormalizeLifecycleOptions(LifecycleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if lifecycle.NoStreamIdleTimeout != 0 {
		t.Fatalf("default no-stream idle timeout = %s, want disabled", lifecycle.NoStreamIdleTimeout)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- client.Serve(context.Background()) }()
	select {
	case err := <-serveErr:
		t.Fatalf("idle session ended on its own: %v (end=%v)", err, client.SessionEnd())
	case <-time.After(2 * time.Second):
	}
	if got := client.SessionEnd(); got != SessionEndNone {
		t.Fatalf("SessionEnd = %v while the session is still running", got)
	}
}

// TestClientIdleGuardEndsSessionGracefully covers the opt-in guard: a caller
// that asks for one still must not see its own shutdown as path loss.
func TestClientIdleGuardEndsSessionGracefully(t *testing.T) {
	client := dialHeldClient(t, holdingEdge(t, 0, "hold"), LifecycleOptions{
		NoStreamIdleTimeout: 100 * time.Millisecond,
	})
	if client.idle == nil {
		t.Fatal("an explicitly requested no-stream idle guard was not armed")
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- client.Serve(context.Background()) }()

	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the armed idle guard never ended the session")
	}
	if got := client.SessionEnd(); got != SessionEndIdle {
		t.Fatalf("SessionEnd = %v, want %v", got, SessionEndIdle)
	}
	if !client.SessionEnd().Graceful() {
		t.Fatal("a self-initiated idle shutdown was not reported as graceful")
	}
}

// TestSessionEndAttributesTheCallersOwnClose keeps a caller-initiated Close
// from being attributed to the peer or the path.
func TestSessionEndAttributesTheCallersOwnClose(t *testing.T) {
	client := dialHeldClient(t, holdingEdge(t, 0, "hold"), LifecycleOptions{})
	serveErr := make(chan error, 1)
	go func() { serveErr <- client.Serve(context.Background()) }()
	time.Sleep(100 * time.Millisecond)
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-serveErr:
	case <-time.After(20 * time.Second):
		t.Fatal("Serve did not return after Close")
	}
	if got := client.SessionEnd(); got != SessionEndLocal {
		t.Fatalf("SessionEnd = %v, want %v", got, SessionEndLocal)
	}
}

func TestSessionEndTokensAndGracefulness(t *testing.T) {
	for end, expected := range map[SessionEnd]struct {
		token    string
		graceful bool
	}{
		SessionEndNone:      {"none", false},
		SessionEndLocal:     {"local", true},
		SessionEndIdle:      {"idle", true},
		SessionEndLifetime:  {"lifetime", true},
		SessionEndRemote:    {"remote", true},
		SessionEndTransport: {"transport", false},
	} {
		if end.String() != expected.token || end.Graceful() != expected.graceful {
			t.Errorf("SessionEnd(%d) = %q/%t, want %q/%t",
				end, end.String(), end.Graceful(), expected.token, expected.graceful)
		}
	}
}

func TestIdleGuardDisarmStopsExpiryAndStillAdmitsStreams(t *testing.T) {
	expired := make(chan struct{})
	guard, err := NewIdleGuard(60*time.Millisecond, func() { close(expired) })
	if err != nil {
		t.Fatal(err)
	}
	if !guard.Start() {
		t.Fatal("first stream was rejected")
	}
	guard.Disarm()
	guard.Done()

	select {
	case <-expired:
		t.Fatal("a disarmed guard expired after its session went quiet")
	case <-time.After(300 * time.Millisecond):
	}
	if !guard.Start() {
		t.Fatal("a disarmed guard rejected a later stream")
	}
	guard.Done()
	guard.Close()
	if guard.Start() {
		t.Fatal("a closed guard admitted a stream")
	}
}
