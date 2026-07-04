package punch

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func mustUDP(t *testing.T, host string) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(host), Port: 0})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func randomToken(t *testing.T) []byte {
	t.Helper()
	tok := make([]byte, tokenLen)
	if _, err := rand.Read(tok); err != nil {
		t.Fatalf("random token: %v", err)
	}
	return tok
}

func startEcho(t *testing.T) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port
}

// TestReflectorClassifyLogic exercises the NAT classification directly via the
// reflector's observation store, so it does not depend on the OS assigning a
// second loopback IP (macOS does not by default; Linux/CI does).
func TestReflectorClassifyLogic(t *testing.T) {
	r, err := NewReflector([]string{"127.0.0.1:0"}, discardLogger())
	if err != nil {
		t.Fatalf("new reflector: %v", err)
	}
	defer r.Close()

	// Same reflexive port seen from two reflector IPs => endpoint-independent.
	r.record("nonce-eim", "203.0.113.1", Endpoint{IP: "198.51.100.9", Port: 40000, Kind: KindSrflx})
	r.record("nonce-eim", "203.0.113.2", Endpoint{IP: "198.51.100.9", Port: 40000, Kind: KindSrflx})
	if class, reflexive, ok := r.Classify("nonce-eim"); !ok || class != ClassEIM || len(reflexive) == 0 {
		t.Fatalf("eim classify = (%q, %v, %v), want eim", class, reflexive, ok)
	}

	// Different reflexive ports => symmetric.
	r.record("nonce-sym", "203.0.113.1", Endpoint{IP: "198.51.100.9", Port: 40000, Kind: KindSrflx})
	r.record("nonce-sym", "203.0.113.2", Endpoint{IP: "198.51.100.9", Port: 40001, Kind: KindSrflx})
	if class, _, ok := r.Classify("nonce-sym"); !ok || class != ClassSymmetric {
		t.Fatalf("symmetric classify = (%q, %v), want symmetric", class, ok)
	}

	// A single observation is inconclusive.
	r.record("nonce-one", "203.0.113.1", Endpoint{IP: "198.51.100.9", Port: 40000, Kind: KindSrflx})
	if class, _, ok := r.Classify("nonce-one"); !ok || class != ClassUnknown {
		t.Fatalf("single-observation classify = (%q, %v), want unknown", class, ok)
	}

	// Unknown nonce => not ok.
	if _, _, ok := r.Classify("never-seen"); ok {
		t.Fatal("classify reported ok for an unseen nonce")
	}
}

// TestGatherAgainstReflector checks end-to-end reflexive discovery over real UDP
// sockets. It uses two loopback IPs when the OS supports them (asserting EIM),
// otherwise a single reflector (asserting a reflexive endpoint is still learned).
func TestGatherAgainstReflector(t *testing.T) {
	addrs := []string{"127.0.0.1:0"}
	twoIP := loopbackAliasAvailable()
	if twoIP {
		addrs = append(addrs, "127.0.0.2:0")
	} else {
		t.Log("127.0.0.2 not bindable; running single-reflector gather (no EIM assertion)")
	}

	r, err := NewReflector(addrs, discardLogger())
	if err != nil {
		t.Fatalf("new reflector: %v", err)
	}
	defer r.Close()

	sock := mustUDP(t, "0.0.0.0")
	_, nonce, err := GenerateNonce()
	if err != nil {
		t.Fatalf("nonce: %v", err)
	}
	reflexive, class, err := Gather(context.Background(), sock, r.Addrs(), nonce)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if len(reflexive) == 0 {
		t.Fatal("no reflexive endpoints gathered")
	}
	if hubClass, _, ok := r.Classify(string(nonce)); !ok {
		t.Fatal("hub recorded no observation for the client nonce")
	} else if twoIP && (class != ClassEIM || hubClass != ClassEIM) {
		t.Fatalf("class client=%q hub=%q, want eim", class, hubClass)
	}
}

func loopbackAliasAvailable() bool {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.2"), Port: 0})
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func TestReflectorDropsShortRequest(t *testing.T) {
	r, err := NewReflector([]string{"127.0.0.1:0"}, discardLogger())
	if err != nil {
		t.Fatalf("new reflector: %v", err)
	}
	defer r.Close()

	sock := mustUDP(t, "127.0.0.1")
	ra, err := net.ResolveUDPAddr("udp", r.Addrs()[0])
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// A sub-floor datagram must be dropped with no reply (amplification guard).
	if _, err := sock.WriteToUDP([]byte(reflectMagicRequest+"short"), ra); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = sock.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, 1500)
	if _, _, err := sock.ReadFromUDP(buf); err == nil {
		t.Fatal("reflector replied to a sub-floor request (amplification risk)")
	}
}

// punchPair runs Attempt on both sockets concurrently and returns the confirmed
// peer addresses.
func punchPair(t *testing.T, a, b *net.UDPConn, aToken, bToken []byte, deadline time.Time) (aConfirmed, bConfirmed *net.UDPAddr, aErr, bErr error) {
	t.Helper()
	sess := "session-xyz"
	aPeer := []Endpoint{endpointFromUDP(b.LocalAddr().(*net.UDPAddr), KindSrflx)}
	bPeer := []Endpoint{endpointFromUDP(a.LocalAddr().(*net.UDPAddr), KindSrflx)}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		aConfirmed, aErr = Attempt(context.Background(), a, aPeer, sess, aToken, deadline)
	}()
	go func() {
		defer wg.Done()
		bConfirmed, bErr = Attempt(context.Background(), b, bPeer, sess, bToken, deadline)
	}()
	wg.Wait()
	return
}

func TestAttemptSucceedsOnLoopback(t *testing.T) {
	a := mustUDP(t, "127.0.0.1")
	b := mustUDP(t, "127.0.0.1")
	token := randomToken(t)
	aConfirmed, bConfirmed, aErr, bErr := punchPair(t, a, b, token, token, time.Now().Add(2*time.Second))
	if aErr != nil || bErr != nil {
		t.Fatalf("punch failed: a=%v b=%v", aErr, bErr)
	}
	if aConfirmed == nil || bConfirmed == nil {
		t.Fatal("punch did not confirm a peer")
	}
	if aConfirmed.Port != b.LocalAddr().(*net.UDPAddr).Port {
		t.Fatalf("a confirmed %v, want peer port %d", aConfirmed, b.LocalAddr().(*net.UDPAddr).Port)
	}
}

func TestAttemptRejectsTokenMismatch(t *testing.T) {
	a := mustUDP(t, "127.0.0.1")
	b := mustUDP(t, "127.0.0.1")
	_, _, aErr, bErr := punchPair(t, a, b, randomToken(t), randomToken(t), time.Now().Add(400*time.Millisecond))
	if aErr == nil || bErr == nil {
		t.Fatalf("expected both sides to time out on token mismatch: a=%v b=%v", aErr, bErr)
	}
}

func TestTransportBridgeEchoOverPunchedSocket(t *testing.T) {
	echoHost, echoPort := startEcho(t)
	vsock := mustUDP(t, "127.0.0.1")
	csock := mustUDP(t, "127.0.0.1")
	token := randomToken(t)

	_, cConfirmed, vErr, cErr := punchPair(t, vsock, csock, token, token, time.Now().Add(2*time.Second))
	if vErr != nil || cErr != nil {
		t.Fatalf("punch failed: volunteer=%v client=%v", vErr, cErr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Volunteer: QUIC server bridging to the echo (loopback "Xray").
	cert, fingerprint, err := GenerateSessionCert()
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	ln, err := ListenQUIC(vsock, cert)
	if err != nil {
		t.Fatalf("listen quic: %v", err)
	}
	go func() { _ = VolunteerBridge(ctx, ln, token, echoHost, echoPort, discardLogger()) }()

	// Client: QUIC dial + loopback bridge.
	dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
	defer dialCancel()
	conn, err := DialQUIC(dialCtx, csock, cConfirmed, fingerprint)
	if err != nil {
		t.Fatalf("dial quic: %v", err)
	}
	bridge, err := NewClientBridge(conn, token, discardLogger())
	if err != nil {
		t.Fatalf("client bridge: %v", err)
	}
	go func() { _ = bridge.Serve(ctx) }()

	host, port := bridge.Endpoint()
	if err := echoRoundTrip(host, port, []byte("hello-punched-path")); err != nil {
		t.Fatalf("echo round trip: %v", err)
	}

	// A second concurrent stream over the same punched connection.
	if err := echoRoundTrip(host, port, []byte("second-stream")); err != nil {
		t.Fatalf("second echo round trip: %v", err)
	}
}

func TestClientBridgeRejectsUnauthenticatedStream(t *testing.T) {
	echoHost, echoPort := startEcho(t)
	vsock := mustUDP(t, "127.0.0.1")
	csock := mustUDP(t, "127.0.0.1")
	token := randomToken(t)

	_, cConfirmed, vErr, cErr := punchPair(t, vsock, csock, token, token, time.Now().Add(2*time.Second))
	if vErr != nil || cErr != nil {
		t.Fatalf("punch failed: volunteer=%v client=%v", vErr, cErr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cert, fingerprint, _ := GenerateSessionCert()
	ln, err := ListenQUIC(vsock, cert)
	if err != nil {
		t.Fatalf("listen quic: %v", err)
	}
	// Volunteer verifies the real token; the client presents a WRONG one.
	go func() { _ = VolunteerBridge(ctx, ln, token, echoHost, echoPort, discardLogger()) }()

	dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
	defer dialCancel()
	conn, err := DialQUIC(dialCtx, csock, cConfirmed, fingerprint)
	if err != nil {
		t.Fatalf("dial quic: %v", err)
	}
	wrongBridge, err := NewClientBridge(conn, randomToken(t), discardLogger())
	if err != nil {
		t.Fatalf("client bridge: %v", err)
	}
	go func() { _ = wrongBridge.Serve(ctx) }()

	host, port := wrongBridge.Endpoint()
	// The volunteer should reject the stream after the bad token, so the echo
	// never completes within the window.
	if err := echoRoundTrip(host, port, []byte("should-not-pass")); err == nil {
		t.Fatal("expected bad-token stream to be rejected, but echo succeeded")
	}
}

func echoRoundTrip(host string, port int, msg []byte) error {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 2*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(msg); err != nil {
		return err
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		return err
	}
	if string(buf) != string(msg) {
		return io.ErrUnexpectedEOF
	}
	return nil
}
