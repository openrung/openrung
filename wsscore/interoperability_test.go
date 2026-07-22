package wsscore

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type localEdge struct {
	server *httptest.Server
	dialer *websocket.Dialer
}

func newLocalEdge(t *testing.T, handler http.Handler) *localEdge {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.StartTLS()
	certificate := server.Certificate()
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	edge := &localEdge{server: server}
	edge.dialer = &websocket.Dialer{
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		// Map a canonical production hostname to the local TLS test edge while
		// retaining the ordinary verified TLS handshake.
		NetDialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		},
	}
	t.Cleanup(func() {
		server.Close()
	})
	return edge
}

func TestClientServerInteroperabilityPreservesOpaqueBytes(t *testing.T) {
	const ticket = "opaque-single-use-ticket"
	payload := make([]byte, 96<<10)
	for i := range payload {
		payload[i] = byte((i*37 + 11) % 256)
	}
	serverResult := make(chan error, 1)
	upgrader := websocket.Upgrader{
		Subprotocols: []string{Subprotocol}, EnableCompression: false,
		CheckOrigin: func(*http.Request) bool { return true },
	}
	edge := newLocalEdge(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() != BridgePath || r.Header.Get(TicketAuthorizationHeader) != TicketBearerPrefix+ticket {
			http.Error(w, "bad request", http.StatusUnauthorized)
			serverResult <- errors.New("ticket was not carried only in Authorization")
			return
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			serverResult <- err
			return
		}
		conn, err := NewWebSocketConn(ws, DefaultWebSocketReadMax)
		if err != nil {
			serverResult <- err
			return
		}
		defer conn.Close()
		session, err := NewServerSession(conn)
		if err != nil {
			serverResult <- err
			return
		}
		defer session.Close()
		stream, err := session.Accept()
		if err != nil {
			serverResult <- err
			return
		}
		defer stream.Close()
		got := make([]byte, len(payload))
		if _, err := io.ReadFull(stream, got); err != nil {
			serverResult <- err
			return
		}
		if !bytes.Equal(got, payload) {
			serverResult <- errors.New("opaque payload changed in transit")
			return
		}
		if _, err = writeAll(stream, got); err == nil {
			var acknowledgement [1]byte
			_, err = io.ReadFull(stream, acknowledgement[:])
			if err == nil && acknowledgement[0] != 0xa5 {
				err = errors.New("bad interoperability acknowledgement")
			}
		}
		serverResult <- err
	}))

	client, err := DialClient(t.Context(), ClientOptions{
		URL: "wss://example.com" + BridgePath, Ticket: ticket,
		WebSocketDialer: edge.dialer, PingInterval: -1,
		Lifecycle: LifecycleOptions{
			SessionLifetime: time.Minute, NoStreamIdleTimeout: time.Minute,
			StreamIdleTimeout: time.Minute, MaxConcurrentStreams: 4,
		},
	})
	if err != nil {
		t.Fatalf("DialClient: %v", err)
	}
	defer client.Close()
	serveCtx, serveCancel := context.WithCancel(context.Background())
	defer serveCancel()
	serveResult := make(chan error, 1)
	go func() { serveResult <- client.Serve(serveCtx) }()

	host, port := client.Endpoint()
	local, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), time.Second)
	if err != nil {
		t.Fatalf("dial client loopback endpoint: %v", err)
	}
	if _, err := local.Write(payload); err != nil {
		t.Fatalf("write opaque payload: %v", err)
	}
	echo := make([]byte, len(payload))
	if _, err := io.ReadFull(local, echo); err != nil {
		t.Fatalf("read opaque echo: %v", err)
	}
	if !bytes.Equal(echo, payload) {
		t.Fatal("loopback adapter changed opaque bytes")
	}
	if _, err := local.Write([]byte{0xa5}); err != nil {
		t.Fatalf("write interoperability acknowledgement: %v", err)
	}
	_ = local.Close()

	select {
	case err := <-serverResult:
		if err != nil {
			t.Fatalf("server interoperability: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server interoperability timed out")
	}
	serveCancel()
	_ = client.Close()
	select {
	case err := <-serveResult:
		if err != nil {
			t.Fatalf("Serve cleanup: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client Serve did not clean up")
	}
}

func TestWebSocketConnRejectsTextMessages(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	edge := newLocalEdge(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		_ = ws.WriteMessage(websocket.TextMessage, []byte("must not enter opaque stream"))
	}))
	ws, _, err := edge.dialer.DialContext(t.Context(), "wss://example.com/text", nil)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := NewWebSocketConn(ws, DefaultWebSocketReadMax)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	buffer := make([]byte, 32)
	if _, err := conn.Read(buffer); !errors.Is(err, ErrNonBinaryMessage) {
		t.Fatalf("text read error = %v, want ErrNonBinaryMessage", err)
	}
}

func TestDialClientRejectsUnsolicitedWebSocketExtensions(t *testing.T) {
	edge := newLocalEdge(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
			return
		}
		conn, buffered, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		accept := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		_, _ = fmt.Fprintf(buffered,
			"HTTP/1.1 101 Switching Protocols\r\n"+
				"Upgrade: websocket\r\n"+
				"Connection: Upgrade\r\n"+
				"Sec-WebSocket-Accept: %s\r\n"+
				"Sec-WebSocket-Protocol: %s\r\n"+
				"Sec-WebSocket-Extensions: permessage-deflate; server_no_context_takeover; client_no_context_takeover\r\n\r\n",
			base64.StdEncoding.EncodeToString(accept[:]), Subprotocol)
		_ = buffered.Flush()
	}))

	client, err := DialClient(t.Context(), ClientOptions{
		URL: "wss://example.com" + BridgePath, Ticket: "extension-ticket",
		WebSocketDialer: edge.dialer, PingInterval: -1,
	})
	if client != nil {
		_ = client.Close()
		t.Fatal("client accepted an unsolicited WebSocket extension")
	}
	if err == nil || err.Error() != "WSS extensions were unexpectedly negotiated" {
		t.Fatalf("DialClient error = %v, want unsolicited-extension rejection", err)
	}
}

func TestWebSocketConnSerializesWritesAndWriteDeadlines(t *testing.T) {
	const messageCount = 64
	serverResult := make(chan error, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	edge := newLocalEdge(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			serverResult <- err
			return
		}
		defer ws.Close()
		for range messageCount {
			messageType, _, err := ws.ReadMessage()
			if err != nil {
				serverResult <- err
				return
			}
			if messageType != websocket.BinaryMessage {
				serverResult <- ErrNonBinaryMessage
				return
			}
		}
		serverResult <- nil
	}))
	ws, _, err := edge.dialer.DialContext(t.Context(), "wss://example.com/deadline", nil)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := NewWebSocketConn(ws, DefaultWebSocketReadMax)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range messageCount {
			if _, err := conn.Write([]byte("opaque")); err != nil {
				errs <- err
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range messageCount {
			if err := conn.SetWriteDeadline(time.Now().Add(time.Minute)); err != nil {
				errs <- err
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent write/deadline operation: %v", err)
	}
	select {
	case err := <-serverResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not receive serialized binary writes")
	}
}

func TestClientLifecycleClosesAndReleasesLoopbackListener(t *testing.T) {
	for name, lifecycle := range map[string]LifecycleOptions{
		"no-stream idle": {
			MaxConcurrentStreams: 2, StreamIdleTimeout: time.Minute,
			NoStreamIdleTimeout: 50 * time.Millisecond, SessionLifetime: time.Minute,
		},
		"session lifetime": {
			MaxConcurrentStreams: 2, StreamIdleTimeout: time.Minute,
			NoStreamIdleTimeout: time.Minute, SessionLifetime: 50 * time.Millisecond,
		},
	} {
		t.Run(name, func(t *testing.T) {
			upgrader := websocket.Upgrader{
				Subprotocols: []string{Subprotocol},
				CheckOrigin:  func(*http.Request) bool { return true },
			}
			serverClosed := make(chan struct{})
			edge := newLocalEdge(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				conn, err := NewWebSocketConn(ws, DefaultWebSocketReadMax)
				if err != nil {
					return
				}
				defer conn.Close()
				session, err := NewServerSession(conn)
				if err != nil {
					return
				}
				defer session.Close()
				<-session.CloseChan()
				close(serverClosed)
			}))
			client, err := DialClient(t.Context(), ClientOptions{
				URL: "wss://example.com" + BridgePath, Ticket: "lifecycle-ticket",
				WebSocketDialer: edge.dialer, PingInterval: -1, Lifecycle: lifecycle,
			})
			if err != nil {
				t.Fatal(err)
			}
			host, port := client.Endpoint()
			serveResult := make(chan error, 1)
			go func() { serveResult <- client.Serve(context.Background()) }()
			select {
			case err := <-serveResult:
				if err != nil {
					t.Fatalf("Serve lifecycle result: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("client lifecycle did not close Serve")
			}
			if conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 100*time.Millisecond); err == nil {
				_ = conn.Close()
				t.Fatal("loopback listener remained reachable after lifecycle close")
			}
			select {
			case <-serverClosed:
			case <-time.After(5 * time.Second):
				t.Fatal("server session was not released after client lifecycle close")
			}
			_ = client.Close()
		})
	}
}

func TestCopyOpaqueStopsOnCancellation(t *testing.T) {
	a1, a2 := net.Pipe()
	b1, b2 := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		CopyOpaque(ctx, a1, b1, time.Minute)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("opaque copy did not stop after cancellation")
	}
	_ = a2.Close()
	_ = b2.Close()
}
