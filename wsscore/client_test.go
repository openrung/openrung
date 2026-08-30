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
	if SocketControl(nil) != nil {
		t.Fatal("a nil protector must yield a nil control, not a closure that panics")
	}
	protector := &recordingProtector{allow: true}
	raw := &fakeRawConn{fd: 42}
	if err := SocketControl(protector)("tcp", "edge.example:443", raw); err != nil {
		t.Fatalf("allowed protection failed: %v", err)
	}
	if raw.controlCalls != 1 || len(protector.fds) != 1 || protector.fds[0] != 42 {
		t.Fatalf("protection calls: raw=%d fds=%v", raw.controlCalls, protector.fds)
	}

	protector.allow = false
	raw = &fakeRawConn{fd: 43}
	if err := SocketControl(protector)("tcp", "edge.example:443", raw); !errors.Is(err, ErrSocketProtectionFailed) {
		t.Fatalf("denied protection error = %v", err)
	}

	before := len(protector.fds)
	raw = &fakeRawConn{fd: ^uintptr(0)}
	if err := SocketControl(protector)("tcp", "edge.example:443", raw); !errors.Is(err, ErrSocketProtectionFailed) {
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
	dialer := newNetworkDialer(time.Second, protector, nil, nil)
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
		// Zero disables the client guard; a negative value is still a mistake.
		"negative no-stream idle": {NoStreamIdleTimeout: -time.Second},
		"long lifetime":           {SessionLifetime: MaxSessionLifetime + time.Second},
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
		TLSConfig: &tls.Config{EncryptedClientHelloConfigList: []byte{1}}, NativeFrontNoSNI: true,
	}); err == nil {
		t.Fatal("encrypted client hello was accepted for CloudFront no-SNI mode")
	}
}

// The resolver's own query sockets must be protected too: Dialer.Control
// covers only the final connection socket, so without a protected pure-Go
// resolver every DNS query for a front hostname would bypass the protector
// (or leave the process entirely via getaddrinfo) and blackhole into the
// device-wide tunnel the dial is trying to escape. The protected resolver
// exists only when the host supplies nameservers: PreferGo without a query
// target breaks resolution outright on Android, which has no resolv.conf.
func TestProtectedResolverRoutesQuerySocketsThroughTheProtector(t *testing.T) {
	protector := &recordingProtector{allow: true}
	if resolver := ProtectedResolver(nil, []string{"127.0.0.1"}); resolver != nil {
		t.Fatalf("nil protector should keep the platform resolver, got %+v", resolver)
	}
	if resolver := ProtectedResolver(protector, nil); resolver != nil {
		t.Fatalf("no nameservers should keep the platform resolver (PreferGo cannot resolve without them on Android), got %+v", resolver)
	}
	resolver := ProtectedResolver(protector, []string{"127.0.0.1"}) // bare IP: port 53 default
	if resolver == nil || !resolver.PreferGo || resolver.Dial == nil {
		t.Fatalf("protected resolver not fully wired: %+v", resolver)
	}
	// A UDP dial creates the query socket immediately, with no server needed
	// on the other end; the resolver-derived address is ignored in favor of
	// the host-supplied nameserver.
	conn, err := resolver.Dial(t.Context(), "udp", "192.0.2.1:53")
	if err != nil {
		t.Fatalf("resolver query dial: %v", err)
	}
	if got := conn.RemoteAddr().String(); got != "127.0.0.1:53" {
		_ = conn.Close()
		t.Fatalf("query went to %s; want the host-supplied nameserver", got)
	}
	_ = conn.Close()
	protector.mu.Lock()
	defer protector.mu.Unlock()
	if len(protector.fds) != 1 {
		t.Fatalf("resolver query socket never reached the protector: %v", protector.fds)
	}

	if dialer := newNetworkDialer(time.Second, protector, nil, []string{"127.0.0.1:53"}); dialer.Resolver == nil || !dialer.Resolver.PreferGo {
		t.Fatal("the WSS network dialer does not carry the protected resolver")
	}
	if dialer := newNetworkDialer(time.Second, protector, nil, nil); dialer.Resolver != nil {
		t.Fatal("without nameservers the dialer must keep the platform resolver")
	}
}

// The refusal sentinel must survive a hostname dial: when the protector
// refuses, the failure fires first on the protected resolver's DNS socket and
// the net package stringifies it into *net.DNSError.Err — severing the
// errors.Is chain. IsSocketProtectionFailed must recognize the refusal across
// that boundary, or every hostname-front classification fails open.
func TestIsSocketProtectionFailedSurvivesTheDNSErrorBoundary(t *testing.T) {
	protector := &recordingProtector{allow: false}
	dialer := newNetworkDialer(2*time.Second, protector, nil, []string{"127.0.0.1:53"})
	_, err := dialer.DialContext(t.Context(), "tcp", "openrung-protection-test.invalid:443")
	if err == nil {
		t.Fatal("a refused hostname dial somehow connected")
	}
	if errors.Is(err, ErrSocketProtectionFailed) {
		t.Logf("errors.Is survived directly (resolver short-circuit): %v", err)
	}
	if !IsSocketProtectionFailed(err) {
		t.Fatalf("the refusal was lost across the resolver boundary: %v", err)
	}
}
