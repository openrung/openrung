package punchcore

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
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
	tok := make([]byte, TokenLen)
	if _, err := rand.Read(tok); err != nil {
		t.Fatalf("random token: %v", err)
	}
	return tok
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
	reflexive, class, err := DesktopPolicy().Gather(context.Background(), sock, r.Addrs(), nonce)
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
		aConfirmed, aErr = DesktopPolicy().Attempt(context.Background(), a, aPeer, sess, aToken, deadline)
	}()
	go func() {
		defer wg.Done()
		bConfirmed, bErr = DesktopPolicy().Attempt(context.Background(), b, bPeer, sess, bToken, deadline)
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

// TestMobileAttemptSucceedsOnLoopbackHosts is the punchcore home of the Android
// punchbridge's Attempt test: loopback "host" candidates survive the mobile
// sanitize profile (hosts must NOT be globally routable; the srflx-only rules
// do not apply to them), so a bidirectional authenticated punch completes under
// MobilePolicy exactly as it did in the hand-mirrored mobile implementation.
func TestMobileAttemptSucceedsOnLoopbackHosts(t *testing.T) {
	a := mustUDP(t, "127.0.0.1")
	b := mustUDP(t, "127.0.0.1")
	token := randomToken(t)
	deadline := time.Now().Add(2 * time.Second)
	sess := "session-mobile"
	aPeer := []Endpoint{endpointFromUDP(b.LocalAddr().(*net.UDPAddr), KindHost)}
	bPeer := []Endpoint{endpointFromUDP(a.LocalAddr().(*net.UDPAddr), KindHost)}
	var aConfirmed, bConfirmed *net.UDPAddr
	var aErr, bErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		aConfirmed, aErr = MobilePolicy().Attempt(context.Background(), a, aPeer, sess, token, deadline)
	}()
	go func() {
		defer wg.Done()
		bConfirmed, bErr = MobilePolicy().Attempt(context.Background(), b, bPeer, sess, token, deadline)
	}()
	wg.Wait()
	if aErr != nil || bErr != nil || aConfirmed == nil || bConfirmed == nil {
		t.Fatalf("mobile punch failed: a=%v/%v b=%v/%v", aConfirmed, aErr, bConfirmed, bErr)
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

// TestMobileGatherRejectsNonLiteralReflectorAddrs asserts MobilePolicy's strict
// reflector-address filter (StrictReflectorAddrs): DNS names, non-global
// literals, loopback, IPv6, and out-of-range ports are all skipped without DNS
// resolution, so a list with no conforming entry fails immediately.
func TestMobileGatherRejectsNonLiteralReflectorAddrs(t *testing.T) {
	sock := mustUDP(t, "127.0.0.1")
	_, nonce, err := GenerateNonce()
	if err != nil {
		t.Fatalf("nonce: %v", err)
	}
	addrs := []string{
		"reflector.example.com:3478", // DNS name → skipped without resolution
		"10.0.0.1:3478",              // private literal → skipped
		"127.0.0.1:3478",             // loopback → skipped
		"[2001:db8::1]:3478",         // IPv6 literal → skipped (socket is udp4)
		"203.0.113.1:0",              // out-of-range port → skipped
		"203.0.113.1",                // no port → skipped
	}
	_, class, err := MobilePolicy().Gather(context.Background(), sock, addrs, nonce)
	if err == nil || err.Error() != "no resolvable reflector addresses" {
		t.Fatalf("err = %v, want no resolvable reflector addresses", err)
	}
	if class != ClassUnknown {
		t.Fatalf("class = %q, want %q", class, ClassUnknown)
	}
}

// TestGatherCancelBehaviorByPolicy asserts the FailGatherOnCancel split: with a
// cancelled context, MobilePolicy surfaces ctx.Err() while DesktopPolicy
// classifies the (empty) partial observations instead — the behavior
// RespondToDirective relies on. The reflector address is a TEST-NET-3 literal;
// both paths bail out before any datagram is sent.
func TestGatherCancelBehaviorByPolicy(t *testing.T) {
	sock := mustUDP(t, "127.0.0.1")
	_, nonce, err := GenerateNonce()
	if err != nil {
		t.Fatalf("nonce: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	addrs := []string{"203.0.113.1:3478"}

	if _, _, err := MobilePolicy().Gather(ctx, sock, addrs, nonce); !errors.Is(err, context.Canceled) {
		t.Fatalf("mobile gather err = %v, want context.Canceled", err)
	}
	_, class, err := DesktopPolicy().Gather(ctx, sock, addrs, nonce)
	if errors.Is(err, context.Canceled) {
		t.Fatalf("desktop gather surfaced cancellation: %v", err)
	}
	if err == nil || err.Error() != "reflector did not observe any endpoint" {
		t.Fatalf("desktop gather err = %v, want reflector did not observe any endpoint", err)
	}
	if class != ClassUnknown {
		t.Fatalf("desktop gather class = %q, want %q", class, ClassUnknown)
	}
}
