package wsscore

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type recordingProtector struct {
	mu    sync.Mutex
	allow bool
	fds   []int32
}

func (p *recordingProtector) Protect(fd int32) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fds = append(p.fds, fd)
	return p.allow
}

type fakeRawConn struct {
	fd           uintptr
	controlCalls int
}

func (c *fakeRawConn) Control(fn func(uintptr)) error {
	c.controlCalls++
	fn(c.fd)
	return nil
}
func (*fakeRawConn) Read(func(uintptr) bool) error  { return syscall.EINVAL }
func (*fakeRawConn) Write(func(uintptr) bool) error { return syscall.EINVAL }

func TestSocketControlFailsClosed(t *testing.T) {
	protector := &recordingProtector{allow: true}
	raw := &fakeRawConn{fd: 42}
	if err := socketControl(protector)(t.Context(), "tcp", "edge.example:443", raw); err != nil {
		t.Fatalf("allowed protection failed: %v", err)
	}
	if raw.controlCalls != 1 || len(protector.fds) != 1 || protector.fds[0] != 42 {
		t.Fatalf("protection calls: raw=%d fds=%v", raw.controlCalls, protector.fds)
	}

	protector.allow = false
	raw = &fakeRawConn{fd: 43}
	if err := socketControl(protector)(t.Context(), "tcp", "edge.example:443", raw); !errors.Is(err, ErrSocketProtectionFailed) {
		t.Fatalf("denied protection error = %v", err)
	}

	before := len(protector.fds)
	raw = &fakeRawConn{fd: ^uintptr(0)}
	if err := socketControl(protector)(t.Context(), "tcp", "edge.example:443", raw); !errors.Is(err, ErrSocketProtectionFailed) {
		t.Fatalf("oversized descriptor error = %v", err)
	}
	if len(protector.fds) != before {
		t.Fatal("protector received a descriptor that cannot fit Android's int fd")
	}
}

func TestNetworkDialerProtectsActualSocketBeforeConnect(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	protector := &recordingProtector{allow: false}
	dialer := newNetworkDialer(time.Second, protector)
	conn, err := dialer.DialContext(t.Context(), "tcp", listener.Addr().String())
	if conn != nil {
		_ = conn.Close()
		t.Fatal("socket connected after protection was denied")
	}
	if !errors.Is(err, ErrSocketProtectionFailed) {
		t.Fatalf("dial error = %v, want ErrSocketProtectionFailed", err)
	}
	protector.mu.Lock()
	defer protector.mu.Unlock()
	if len(protector.fds) != 1 || protector.fds[0] < 0 {
		t.Fatalf("VpnService protector descriptors = %v", protector.fds)
	}
}

func TestDialClientRejectsProtectorWithCustomNetworkDialer(t *testing.T) {
	protector := &recordingProtector{allow: true}
	_, err := DialClient(t.Context(), ClientOptions{
		URL: "wss://edge.example" + BridgePath, Ticket: "ticket",
		SocketProtector: protector,
		WebSocketDialer: &websocket.Dialer{NetDialContext: func(context.Context, string, string) (net.Conn, error) {
			panic("must not dial")
		}},
	})
	if err == nil {
		t.Fatal("custom dial callback was allowed to bypass socket protection")
	}
	_, err = DialClient(t.Context(), ClientOptions{
		URL: "wss://edge.example" + BridgePath, Ticket: "ticket",
		WebSocketDialer: &websocket.Dialer{NetDialTLSContext: func(context.Context, string, string) (net.Conn, error) {
			panic("must not dial")
		}},
	})
	if err == nil {
		t.Fatal("custom TLS callback was allowed to bypass verified WSS TLS")
	}
}

func TestIdleGuardTracksActiveStreamsAndExpiresOnce(t *testing.T) {
	expired := make(chan struct{}, 1)
	guard, err := NewIdleGuard(25*time.Millisecond, func() { expired <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	if !guard.Start() {
		t.Fatal("fresh idle guard rejected stream")
	}
	select {
	case <-expired:
		t.Fatal("guard expired while a stream was active")
	case <-time.After(75 * time.Millisecond):
	}
	guard.Done()
	select {
	case <-expired:
	case <-time.After(time.Second):
		t.Fatal("guard did not expire after last stream ended")
	}
	if guard.Start() {
		t.Fatal("expired guard accepted a new stream")
	}
	select {
	case <-expired:
		t.Fatal("idle callback ran more than once")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestLifecycleAndClientBoundsRejectUnsafeValues(t *testing.T) {
	for name, opts := range map[string]LifecycleOptions{
		"too many streams":     {MaxConcurrentStreams: MaxConcurrentStreams + 1},
		"negative stream idle": {StreamIdleTimeout: -time.Second},
		"long no-stream idle":  {NoStreamIdleTimeout: MaxSessionLifetime + time.Second},
		"long lifetime":        {SessionLifetime: MaxSessionLifetime + time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NormalizeLifecycleOptions(opts); err == nil {
				t.Fatal("unsafe lifecycle options accepted")
			}
		})
	}
	if _, err := DialClient(t.Context(), ClientOptions{
		URL: "wss://edge.example" + BridgePath, Ticket: "ticket",
		HandshakeTimeout: MaxHandshakeTimeout + time.Second,
	}); err == nil {
		t.Fatal("unbounded handshake timeout accepted")
	}
	if _, err := DialClient(t.Context(), ClientOptions{
		URL: "wss://edge.example" + BridgePath + "?ticket=secret", Ticket: "ticket",
	}); err == nil {
		t.Fatal("ticket-bearing URL accepted")
	}
	if _, err := DialClient(t.Context(), ClientOptions{
		URL: "wss://edge.example" + BridgePath, Ticket: "ticket",
		TLSConfig: &tls.Config{InsecureSkipVerify: true}, // Deliberately prove fail-closed validation.
	}); err == nil {
		t.Fatal("disabled WSS TLS verification accepted")
	}
	if _, err := DialClient(t.Context(), ClientOptions{
		URL: "wss://edge.example" + BridgePath, Ticket: "ticket",
		TLSConfig: &tls.Config{ServerName: "other.example"},
	}); err == nil {
		t.Fatal("TLS server-name override accepted")
	}
	if _, err := DialClient(t.Context(), ClientOptions{
		URL: "wss://d111111abcdef8.cloudfront.net" + BridgePath, Ticket: "ticket",
		TLSConfig: &tls.Config{EncryptedClientHelloConfigList: []byte{1}}, CloudFrontNoSNI: true,
	}); err == nil {
		t.Fatal("encrypted client hello was accepted for CloudFront no-SNI mode")
	}
}
