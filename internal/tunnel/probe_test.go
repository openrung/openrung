package tunnel

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// serveNonceListener starts a listener that writes the probe nonce line to every
// accepted connection, standing in for a relay's temporary probe listener.
func serveNonceListener(t *testing.T, nonce string) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
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
				_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
				_, _ = c.Write([]byte(ProbeLinePrefix + nonce + "\n"))
			}(c)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestReachabilityProberReachable(t *testing.T) {
	port := serveNonceListener(t, "nonce-ok")
	p := NewReachabilityProber("", discardLogger())
	resp := p.dialAndVerify("127.0.0.1", port, "nonce-ok")
	if !resp.Reachable {
		t.Fatalf("expected reachable, got %+v", resp)
	}
	if resp.ObservedHost != "127.0.0.1" {
		t.Fatalf("observed host = %q, want 127.0.0.1", resp.ObservedHost)
	}
}

func TestReachabilityProberNoListener(t *testing.T) {
	port := freePort(t) // reserved then released → nothing listening
	p := NewReachabilityProber("", discardLogger())
	if resp := p.dialAndVerify("127.0.0.1", port, "x"); resp.Reachable {
		t.Fatalf("expected unreachable for a closed port, got %+v", resp)
	}
}

func TestReachabilityProberNonceMismatch(t *testing.T) {
	port := serveNonceListener(t, "server-nonce")
	p := NewReachabilityProber("", discardLogger())
	if resp := p.dialAndVerify("127.0.0.1", port, "client-nonce"); resp.Reachable {
		t.Fatalf("expected unreachable on nonce mismatch, got %+v", resp)
	}
}

func TestReachabilityProberHTTPRequiresTokenAndDialsSource(t *testing.T) {
	port := serveNonceListener(t, "http-nonce")
	p := NewReachabilityProber("secret", discardLogger())
	mux := http.NewServeMux()
	p.Register(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body, _ := json.Marshal(ProbeRequest{Port: port, Nonce: "http-nonce"})

	// No token → 401.
	resp, err := http.Post(ts.URL+PathProbe, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status without token = %d, want 401", resp.StatusCode)
	}

	// With token → reachable, observed host is the request source (127.0.0.1),
	// confirming the prober dials the caller's own IP (no SSRF to a chosen host).
	req, _ := http.NewRequest(http.MethodPost, ts.URL+PathProbe, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	resp, err = ts.Client().Do(req)
	if err != nil {
		t.Fatalf("authed post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authed status = %d, want 200", resp.StatusCode)
	}
	var out ProbeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Reachable || out.ObservedHost != "127.0.0.1" {
		t.Fatalf("unexpected probe result: %+v", out)
	}
}
