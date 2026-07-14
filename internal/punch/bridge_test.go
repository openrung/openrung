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

	"github.com/openrung/openrung/punchcore"
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
	tok := make([]byte, punchcore.TokenLen)
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

// punchPair runs Attempt on both sockets concurrently and returns the confirmed
// peer addresses.
func punchPair(t *testing.T, a, b *net.UDPConn, aToken, bToken []byte, deadline time.Time) (aConfirmed, bConfirmed *net.UDPAddr, aErr, bErr error) {
	t.Helper()
	sess := "session-xyz"
	bAddr := b.LocalAddr().(*net.UDPAddr)
	aAddr := a.LocalAddr().(*net.UDPAddr)
	aPeer := []punchcore.Endpoint{{IP: bAddr.IP.String(), Port: bAddr.Port, Kind: punchcore.KindSrflx}}
	bPeer := []punchcore.Endpoint{{IP: aAddr.IP.String(), Port: aAddr.Port, Kind: punchcore.KindSrflx}}
	pol := punchcore.DesktopPolicy()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		aConfirmed, aErr = pol.Attempt(context.Background(), a, aPeer, sess, aToken, deadline)
	}()
	go func() {
		defer wg.Done()
		bConfirmed, bErr = pol.Attempt(context.Background(), b, bPeer, sess, bToken, deadline)
	}()
	wg.Wait()
	return
}

func TestTransportBridgeEchoOverPunchedSocket(t *testing.T) {
	echoHost, echoPort := startEcho(t)
	relaySock := mustUDP(t, "127.0.0.1")
	clientSock := mustUDP(t, "127.0.0.1")
	token := randomToken(t)

	_, clientConfirmed, relayErr, clientErr := punchPair(t, relaySock, clientSock, token, token, time.Now().Add(2*time.Second))
	if relayErr != nil || clientErr != nil {
		t.Fatalf("punch failed: relay=%v client=%v", relayErr, clientErr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Relay: QUIC server bridging to the echo (loopback "Xray").
	cert, fingerprint, err := GenerateSessionCert()
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	ln, err := ListenQUIC(relaySock, cert)
	if err != nil {
		t.Fatalf("listen quic: %v", err)
	}
	go func() { _ = RelayBridge(ctx, ln, token, echoHost, echoPort, discardLogger()) }()

	// Client: QUIC dial + loopback bridge.
	dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
	defer dialCancel()
	conn, err := DialQUIC(dialCtx, clientSock, clientConfirmed, fingerprint)
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
	relaySock := mustUDP(t, "127.0.0.1")
	clientSock := mustUDP(t, "127.0.0.1")
	token := randomToken(t)

	_, clientConfirmed, relayErr, clientErr := punchPair(t, relaySock, clientSock, token, token, time.Now().Add(2*time.Second))
	if relayErr != nil || clientErr != nil {
		t.Fatalf("punch failed: relay=%v client=%v", relayErr, clientErr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cert, fingerprint, _ := GenerateSessionCert()
	ln, err := ListenQUIC(relaySock, cert)
	if err != nil {
		t.Fatalf("listen quic: %v", err)
	}
	// Relay verifies the real token; the client presents a WRONG one.
	go func() { _ = RelayBridge(ctx, ln, token, echoHost, echoPort, discardLogger()) }()

	dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
	defer dialCancel()
	conn, err := DialQUIC(dialCtx, clientSock, clientConfirmed, fingerprint)
	if err != nil {
		t.Fatalf("dial quic: %v", err)
	}
	wrongBridge, err := NewClientBridge(conn, randomToken(t), discardLogger())
	if err != nil {
		t.Fatalf("client bridge: %v", err)
	}
	go func() { _ = wrongBridge.Serve(ctx) }()

	host, port := wrongBridge.Endpoint()
	// The relay should reject the stream after the bad token, so the echo
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
