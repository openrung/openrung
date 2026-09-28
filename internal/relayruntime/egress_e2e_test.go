package relayruntime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// End to end against the real xray, with a second xray as a genuine
// VLESS+Reality+Vision client (an HTTP proxy inbound in front of it): the
// relay's egress guard must stop a client from reaching the relay host's own
// loopback and, above all, the management API, while the same credential
// still carries ordinary traffic. Runs only where xray is installed.

type e2eRelay struct {
	listenPort int
	apiPort    int
	publicKey  string
	api        *XrayAPI
	stop       func()
	// log collects the relay xray's output, including the access log line
	// that names the outbound each connection was routed to.
	log *lockedBuffer
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startE2ERelay renders and starts a relay xray whose Reality dest is a local
// TLS server (so the handshake needs no internet) and installs one credential.
func startE2ERelay(t *testing.T, xrayPath string, guard bool, credential Credential) *e2eRelay {
	t.Helper()
	dest := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "dest") }))
	dest.StartTLS()
	t.Cleanup(dest.Close)
	destURL, _ := url.Parse(dest.URL)

	keys, err := GenerateRealityKeyPair(xrayPath)
	if err != nil {
		t.Fatalf("generate Reality keys: %v", err)
	}
	relay := &e2eRelay{listenPort: freeLoopbackPort(t), apiPort: freeLoopbackPort(t), publicKey: keys.PublicKey, log: &lockedBuffer{}}
	config, err := BuildXrayConfig(XrayConfigInput{
		ListenHost:         "127.0.0.1",
		ListenPort:         relay.listenPort,
		APIPort:            relay.apiPort,
		Flow:               "xtls-rprx-vision",
		Dest:               destURL.Host,
		ServerName:         "www.cloudflare.com",
		RealityPrivateKey:  keys.PrivateKey,
		ShortID:            "0123456789abcdef",
		disableEgressGuard: !guard,
	})
	if err != nil {
		t.Fatalf("build relay config: %v", err)
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "relay.json")
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatalf("write relay config: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := NewXrayCommand(ctx, xrayPath, "run", "-config", configPath)
	cmd.Stdout, cmd.Stderr = relay.log, relay.log
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start relay xray: %v", err)
	}
	relay.stop = func() { cancel(); _ = cmd.Wait() }
	t.Cleanup(relay.stop)

	relay.api = &XrayAPI{Path: xrayPath, Addr: net.JoinHostPort("127.0.0.1", fmt.Sprint(relay.apiPort)), Flow: "xtls-rprx-vision", Dir: dir, Timeout: 3 * time.Second}
	deadline := time.Now().Add(10 * time.Second)
	for {
		err = relay.api.AddUser(ctx, credential)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("install credential: %v", err)
	}
	return relay
}

// startE2EClient starts a client xray: an HTTP proxy inbound whose only
// outbound is VLESS+Reality+Vision to the relay with the given credential.
// It returns the proxy address.
func startE2EClient(t *testing.T, xrayPath string, relay *e2eRelay, clientID string) string {
	t.Helper()
	proxyPort := freeLoopbackPort(t)
	config, err := json.Marshal(map[string]any{
		"log": map[string]any{"loglevel": "warning"},
		"inbounds": []any{map[string]any{
			"tag": "http-in", "listen": "127.0.0.1", "port": proxyPort, "protocol": "http",
		}},
		"outbounds": []any{map[string]any{
			"protocol": "vless",
			"settings": map[string]any{"vnext": []any{map[string]any{
				"address": "127.0.0.1", "port": relay.listenPort,
				"users": []any{map[string]any{"id": clientID, "encryption": "none", "flow": "xtls-rprx-vision"}},
			}}},
			"streamSettings": map[string]any{
				"network": "tcp", "security": "reality",
				"realitySettings": map[string]any{
					"serverName": "www.cloudflare.com", "fingerprint": "chrome",
					"publicKey": relay.publicKey, "shortId": "0123456789abcdef",
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("marshal client config: %v", err)
	}
	configPath := filepath.Join(t.TempDir(), "client.json")
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatalf("write client config: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := NewXrayCommand(ctx, xrayPath, "run", "-config", configPath)
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start client xray: %v", err)
	}
	t.Cleanup(func() { cancel(); _ = cmd.Wait() })
	proxy := net.JoinHostPort("127.0.0.1", fmt.Sprint(proxyPort))
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", proxy, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return proxy
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("client xray proxy never came up")
	return ""
}

// fetchThrough issues GET url through the client proxy, i.e. through the
// relay, and returns the body or an error.
func fetchThrough(proxy, target string) (string, error) {
	proxyURL, _ := url.Parse("http://" + proxy)
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true}}
	resp, err := client.Get(target)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	return string(body), nil
}

// grpcReachableThrough opens a CONNECT tunnel to hostPort through the client
// proxy and speaks the HTTP/2 client preface into it. xray's gRPC management
// server answers with a SETTINGS frame; anything else (EOF, timeout, garbage)
// means the destination was not reachable through the relay.
func grpcReachableThrough(proxy, hostPort string) bool {
	conn, err := net.DialTimeout("tcp", proxy, 2*time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", hostPort, hostPort); err != nil {
		return false
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, " 200 ") {
		return false
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return false
		}
		if line == "\r\n" {
			break
		}
	}
	// HTTP/2 connection preface followed by an empty SETTINGS frame.
	if _, err := conn.Write([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n\x00\x00\x00\x04\x00\x00\x00\x00\x00")); err != nil {
		return false
	}
	frame := make([]byte, 9)
	if _, err := io.ReadFull(reader, frame); err != nil {
		return false
	}
	return frame[3] == 0x04 // a SETTINGS frame: the gRPC server is talking to us
}

func TestEgressGuardKeepsClientsOffTheManagementAPI(t *testing.T) {
	xrayPath := requireXray(t)
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "victim") }))
	defer victim.Close()
	credential := Credential{ID: "11111111-2222-4333-8444-555555555555", Email: "cred-20260917T140000Z"}

	// Control: without the guard the tunnel works, a wrong credential is
	// refused, and — the reported attack — a client reaches the relay's own
	// loopback and drives its management API.
	unguarded := startE2ERelay(t, xrayPath, false, credential)
	proxy := startE2EClient(t, xrayPath, unguarded, credential.ID)
	body, err := fetchThrough(proxy, victim.URL)
	if err != nil || body != "victim" {
		t.Fatalf("valid credential through an unguarded relay: body %q, err %v; want the victim's reply (the tunnel itself must work)", body, err)
	}
	if !grpcReachableThrough(proxy, unguarded.api.Addr) {
		t.Fatal("control failed: the management API was expected to be reachable through an unguarded relay")
	}
	wrong := startE2EClient(t, xrayPath, unguarded, "99999999-2222-4333-8444-555555555555")
	if body, err := fetchThrough(wrong, victim.URL); err == nil {
		t.Fatalf("a credential xray does not accept still reached the victim: %q", body)
	}

	// The shipped config: same credential, same client — loopback and the
	// management port are refused.
	guarded := startE2ERelay(t, xrayPath, true, credential)
	proxy = startE2EClient(t, xrayPath, guarded, credential.ID)
	if body, err := fetchThrough(proxy, victim.URL); err == nil {
		t.Fatalf("guarded relay let a client reach its loopback: %q", body)
	}
	if grpcReachableThrough(proxy, guarded.api.Addr) {
		t.Fatal("guarded relay let a client reach its management API")
	}
	if grpcReachableThrough(proxy, net.JoinHostPort("localhost", fmt.Sprint(guarded.apiPort))) {
		t.Fatal("guarded relay let a client reach its management API by name")
	}
	// The API itself is still there for the relay process.
	users, err := guarded.api.ListUsers(context.Background())
	if err != nil || len(users) != 1 {
		t.Fatalf("management API unusable by the relay itself after the guard: users %v, err %v", users, err)
	}
}

// echoThrough opens a CONNECT tunnel to hostPort through the client proxy,
// writes payload and returns whatever comes back before the deadline. A
// destination the relay routes to its blackhole returns nothing.
func echoThrough(proxy, hostPort string, payload []byte) []byte {
	conn, err := net.DialTimeout("tcp", proxy, 2*time.Second)
	if err != nil {
		return nil
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", hostPort, hostPort); err != nil {
		return nil
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, " 200 ") {
		return nil
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil
		}
		if line == "\r\n" {
			break
		}
	}
	if _, err := conn.Write(payload); err != nil {
		return nil
	}
	reply := make([]byte, len(payload))
	n, _ := io.ReadFull(reader, reply)
	return reply[:n]
}

// listenEchoAt echoes every connection on addr until the test ends.
func listenEchoAt(t *testing.T, addr string) (string, error) {
	t.Helper()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	return listener.Addr().String(), nil
}

// waitForRoute waits for the relay's access log to record hostPort routed to
// outbound at least n times. xray writes ">>" for the default outbound and
// "->" for one a routing rule chose.
func waitForRoute(relay *e2eRelay, hostPort, outbound string, n int) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		log := relay.log.String()
		count := 0
		for _, arrow := range []string{">>", "->"} {
			count += strings.Count(log, fmt.Sprintf("tcp:%s [%s %s %s]", hostPort, XrayInboundTag, arrow, outbound))
		}
		if count >= n {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// The shipped peer-to-peer block, against the real xray: ordinary traffic to
// a destination passes, while a BitTorrent handshake to the same destination,
// any connection to a BitTorrent port, and any connection to a Xunlei domain
// go to the blackhole. The host guard is off so the destinations can be local;
// the peer-to-peer rules do not depend on it.
func TestRelayBlocksPeerToPeerTraffic(t *testing.T) {
	xrayPath := requireXray(t)
	credential := Credential{ID: "11111111-2222-4333-8444-555555555555", Email: "cred-20260917T140000Z"}
	relay := startE2ERelay(t, xrayPath, false, credential)
	proxy := startE2EClient(t, xrayPath, relay, credential.ID)

	echo, err := listenEchoAt(t, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start echo server: %v", err)
	}
	hello := []byte("hello")
	if got := echoThrough(proxy, echo, hello); !bytes.Equal(got, hello) {
		t.Fatalf("ordinary traffic through the relay: got %q, want %q (the tunnel itself must work)", got, hello)
	}
	if !waitForRoute(relay, echo, "direct", 1) {
		t.Fatalf("ordinary traffic was not routed direct; relay log:\n%s", relay.log)
	}

	// A BitTorrent peer-wire handshake: length-prefixed protocol name,
	// reserved bytes, info hash and peer ID.
	handshake := append([]byte("\x13BitTorrent protocol"), make([]byte, 8)...)
	handshake = append(handshake, bytes.Repeat([]byte{0xab}, 20)...)
	handshake = append(handshake, []byte("-qB4650-000000000000")...)
	if got := echoThrough(proxy, echo, handshake); len(got) != 0 {
		t.Fatalf("a BitTorrent handshake reached the destination: echoed %d bytes", len(got))
	}
	if !waitForRoute(relay, echo, xrayBlockOutboundTag, 1) {
		t.Fatalf("a BitTorrent handshake was not routed to the blackhole; relay log:\n%s", relay.log)
	}

	var btPort string
	for port := 6881; port <= 6889 && btPort == ""; port++ {
		if addr, err := listenEchoAt(t, fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
			btPort = addr
		}
	}
	if btPort == "" {
		t.Fatal("no free port in 6881-6889 for the BitTorrent port check")
	}
	if got := echoThrough(proxy, btPort, hello); len(got) != 0 {
		t.Fatalf("ordinary bytes to BitTorrent port %s reached the destination: %q", btPort, got)
	}
	if !waitForRoute(relay, btPort, xrayBlockOutboundTag, 1) {
		t.Fatalf("BitTorrent port %s was not routed to the blackhole; relay log:\n%s", btPort, relay.log)
	}

	// The domain rule matches before any resolution, so the log line is the
	// proof; the destination port is the local echo server's.
	_, port, _ := net.SplitHostPort(echo)
	xunlei := net.JoinHostPort("stat.download.xunlei.com", port)
	if got := echoThrough(proxy, xunlei, hello); len(got) != 0 {
		t.Fatalf("a Xunlei domain reached a destination: %q", got)
	}
	if !waitForRoute(relay, xunlei, xrayBlockOutboundTag, 1) {
		t.Fatalf("Xunlei domain was not routed to the blackhole; relay log:\n%s", relay.log)
	}
}
