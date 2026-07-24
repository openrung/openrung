// SPDX-License-Identifier: GPL-3.0-or-later

package client

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"openrung/internal/relay"
)

const testECHPublicName = "cloudflare-ech.com"

func TestCloudflareBrokerAddress(t *testing.T) {
	for _, tc := range []struct {
		address string
		want    bool
	}{
		{"broker.openrung.org:443", true},
		{"BROKER.OPENRUNG.ORG:443", true},
		{"broker.openrung.org.:443", true},
		{"broker.openrung.org:8443", false},
		{"sub.broker.openrung.org:443", false},
		{"broker.openrung.org.example:443", false},
		{"d2r7mdpyevvs1m.cloudfront.net:443", false},
		{"127.0.0.1:443", false},
		{"not-an-address", false},
	} {
		t.Run(tc.address, func(t *testing.T) {
			if got := isCloudflareBrokerAddress(tc.address); got != tc.want {
				t.Fatalf("isCloudflareBrokerAddress(%q) = %t, want %t", tc.address, got, tc.want)
			}
		})
	}
}

func TestBrokerECHDialerRefreshesFromAuthenticatedRetryConfig(t *testing.T) {
	certificate, roots := testBrokerCertificate(t)
	_, oldList, _ := testECHConfig(t, 17, testECHPublicName)
	currentConfig, currentList, currentKey := testECHConfig(t, 29, testECHPublicName)
	serverConfig := testBrokerServerConfig(certificate)
	serverConfig.EncryptedClientHelloKeys = []tls.EncryptedClientHelloKey{{
		Config:      currentConfig,
		PrivateKey:  currentKey.Bytes(),
		SendAsRetry: true,
	}}

	networkDial, results, calls := testTLSPipeDialer(serverConfig)
	state := newBrokerECHConfigState(oldList)
	dialer := testBrokerECHDialer(networkDial, roots, state)

	conn, err := dialer.dialTLSContext(t.Context(), "tcp", cloudflareBrokerHost+":443")
	if err != nil {
		t.Fatalf("dial with stale ECH config: %v", err)
	}
	closeTLSConn(conn)

	gotList, generation := state.snapshot()
	attemptCount := int(calls.Load())
	if attemptCount != 2 {
		t.Fatalf("initial TLS connection attempts = %d, want stale + retry = 2", attemptCount)
	}
	first := readTLSServerResults(t, results, attemptCount)
	if !bytes.Equal(gotList, currentList) || generation != 1 {
		t.Fatalf("retry config was not promoted: generation=%d config=%x, want generation=1 config=%x", generation, gotList, currentList)
	}

	if first[0].state.ECHAccepted {
		t.Fatal("server accepted the stale ECH config")
	}
	if first[0].err != nil {
		t.Fatalf("stale ECH outer handshake failed before authenticated rejection: %v", first[0].err)
	}
	if first[1].err != nil || !first[1].state.ECHAccepted {
		t.Fatalf("retry handshake = err %v, ECHAccepted %t", first[1].err, first[1].state.ECHAccepted)
	}

	// A later connection must start with the refreshed list, rather than pay
	// the stale-config rejection again.
	conn, err = dialer.dialTLSContext(t.Context(), "tcp", cloudflareBrokerHost+":443")
	if err != nil {
		t.Fatalf("dial with refreshed ECH config: %v", err)
	}
	closeTLSConn(conn)
	later := readTLSServerResults(t, results, 1)[0]
	if later.err != nil || !later.state.ECHAccepted {
		t.Fatalf("later handshake = err %v, ECHAccepted %t", later.err, later.state.ECHAccepted)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("TLS connection attempts = %d, want stale + retry + one refreshed = 3", got)
	}
}

func TestBrokerECHDialerFallsBackToPlainTLS(t *testing.T) {
	certificate, roots := testBrokerCertificate(t)
	serverConfig := testBrokerServerConfig(certificate)
	networkDial, results, calls := testTLSPipeDialer(serverConfig)
	state := newBrokerECHConfigState(embeddedCloudflareECHConfigList)
	dialer := testBrokerECHDialer(networkDial, roots, state)

	conn, err := dialer.dialTLSContext(t.Context(), "tcp", cloudflareBrokerHost+":443")
	if err != nil {
		t.Fatalf("ECH rejection did not fall back to plain TLS: %v", err)
	}
	closeTLSConn(conn)

	handshakes := readTLSServerResults(t, results, 2)
	if handshakes[0].err != nil || handshakes[0].state.ECHAccepted {
		t.Fatalf("embedded ECH outer handshake = err %v, ECHAccepted %t; want an authenticated rejection", handshakes[0].err, handshakes[0].state.ECHAccepted)
	}
	if handshakes[1].err != nil || handshakes[1].state.ECHAccepted {
		t.Fatalf("plain fallback = err %v, ECHAccepted %t", handshakes[1].err, handshakes[1].state.ECHAccepted)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("TLS connection attempts = %d, want ECH + plain = 2", got)
	}
	gotList, generation := state.snapshot()
	if !bytes.Equal(gotList, embeddedCloudflareECHConfigList) || generation != 0 {
		t.Fatal("an empty retry config changed the shared ECH config")
	}
}

func TestBrokerECHDialerBlackholeFallsBackWithinBudget(t *testing.T) {
	certificate, roots := testBrokerCertificate(t)
	serverConfig := testBrokerServerConfig(certificate)
	plainDial, results, _ := testTLSPipeDialer(serverConfig)

	var calls atomic.Int64
	var blackhole net.Conn
	networkDial := func(ctx context.Context, network, address string) (net.Conn, error) {
		if calls.Add(1) == 1 {
			clientSide, serverSide := net.Pipe()
			blackhole = serverSide
			return clientSide, nil
		}
		return plainDial(ctx, network, address)
	}
	t.Cleanup(func() {
		if blackhole != nil {
			_ = blackhole.Close()
		}
	})

	state := newBrokerECHConfigState(embeddedCloudflareECHConfigList)
	dialer := testBrokerECHDialer(networkDial, roots, state)
	dialer.echTimeout = 40 * time.Millisecond

	started := time.Now()
	conn, err := dialer.dialTLSContext(t.Context(), "tcp", cloudflareBrokerHost+":443")
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("blackholed ECH did not fall back: %v", err)
	}
	closeTLSConn(conn)
	if elapsed > time.Second {
		t.Fatalf("blackholed ECH fallback took %v, want well under 1s in this test", elapsed)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("network dials = %d, want blackholed ECH + plain = 2", got)
	}
	plain := readTLSServerResults(t, results, 1)[0]
	if plain.err != nil || plain.state.ECHAccepted {
		t.Fatalf("plain fallback = err %v, ECHAccepted %t", plain.err, plain.state.ECHAccepted)
	}
}

func TestBrokerECHDialerCancellationDoesNotLeakPlainSNI(t *testing.T) {
	_, roots := testBrokerCertificate(t)
	started := make(chan struct{})
	var calls atomic.Int64
	var blackhole net.Conn
	networkDial := func(context.Context, string, string) (net.Conn, error) {
		calls.Add(1)
		clientSide, serverSide := net.Pipe()
		blackhole = serverSide
		close(started)
		return clientSide, nil
	}
	t.Cleanup(func() {
		if blackhole != nil {
			_ = blackhole.Close()
		}
	})

	state := newBrokerECHConfigState(embeddedCloudflareECHConfigList)
	dialer := testBrokerECHDialer(networkDial, roots, state)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := dialer.dialTLSContext(ctx, "tcp", cloudflareBrokerHost+":443")
		done <- err
	}()
	<-started
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("dial error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled ECH handshake did not return")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("network dials after cancellation = %d, want only the ECH attempt", got)
	}
}

func TestBrokerECHDialerPreservesCertificateVerification(t *testing.T) {
	certificate, _ := testBrokerCertificate(t)
	_, oldList, _ := testECHConfig(t, 41, testECHPublicName)
	currentConfig, _, currentKey := testECHConfig(t, 42, testECHPublicName)
	serverConfig := testBrokerServerConfig(certificate)
	serverConfig.EncryptedClientHelloKeys = []tls.EncryptedClientHelloKey{{
		Config:      currentConfig,
		PrivateKey:  currentKey.Bytes(),
		SendAsRetry: true,
	}}
	networkDial, results, calls := testTLSPipeDialer(serverConfig)

	// Deliberately omit the test certificate from RootCAs. Both the outer ECH
	// authentication and the ordinary broker certificate check must fail, and
	// the unauthenticated retry list must not enter shared state.
	state := newBrokerECHConfigState(oldList)
	dialer := testBrokerECHDialer(networkDial, x509.NewCertPool(), state)
	conn, err := dialer.dialTLSContext(t.Context(), "tcp", cloudflareBrokerHost+":443")
	if conn != nil {
		closeTLSConn(conn)
	}
	if err == nil {
		t.Fatal("untrusted broker certificate was accepted")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("TLS connection attempts = %d, want failed ECH + failed plain = 2", got)
	}
	_ = readTLSServerResults(t, results, 2)
	gotList, generation := state.snapshot()
	if !bytes.Equal(gotList, oldList) || generation != 0 {
		t.Fatal("retry config from an untrusted outer certificate changed shared state")
	}
}

func TestBrokerECHDialerLeavesCloudFrontPlain(t *testing.T) {
	certificate, roots := testBrokerCertificate(t)
	serverConfig := testBrokerServerConfig(certificate)
	networkDial, results, calls := testTLSPipeDialer(serverConfig)
	dialer := testBrokerECHDialer(networkDial, roots, newBrokerECHConfigState(embeddedCloudflareECHConfigList))

	conn, err := dialer.dialTLSContext(t.Context(), "tcp", "d2r7mdpyevvs1m.cloudfront.net:443")
	if err != nil {
		t.Fatalf("CloudFront plain TLS dial: %v", err)
	}
	closeTLSConn(conn)
	result := readTLSServerResults(t, results, 1)[0]
	if result.err != nil || result.state.ECHAccepted {
		t.Fatalf("CloudFront handshake = err %v, ECHAccepted %t", result.err, result.state.ECHAccepted)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("CloudFront TLS connection attempts = %d, want one plain attempt", got)
	}
}

func TestBrokerECHDialerDoesNotForceHTTP2ALPN(t *testing.T) {
	certificate, roots := testBrokerCertificate(t)
	serverConfig := testBrokerServerConfig(certificate)
	serverConfig.NextProtos = []string{"h2", "http/1.1"}
	networkDial, results, _ := testTLSPipeDialer(serverConfig)
	dialer := testBrokerECHDialer(networkDial, roots, newBrokerECHConfigState(embeddedCloudflareECHConfigList))
	dialer.baseTLSConfig.NextProtos = nil

	conn, err := dialer.dialTLSContext(t.Context(), "tcp", "d2r7mdpyevvs1m.cloudfront.net:443")
	if err != nil {
		t.Fatalf("plain TLS dial without ALPN: %v", err)
	}
	closeTLSConn(conn)
	result := readTLSServerResults(t, results, 1)[0]
	if result.err != nil {
		t.Fatalf("server handshake: %v", result.err)
	}
	if got := result.state.NegotiatedProtocol; got != "" {
		t.Fatalf("negotiated protocol = %q, want none when the base TLS policy advertises none", got)
	}
}

func TestBrokerECHBlackholeFallsBackAtHTTPLevel(t *testing.T) {
	certificate, roots := testBrokerCertificate(t)
	listener := newPipeListener()
	tlsListener := tls.NewListener(listener, testBrokerServerConfig(certificate))
	var handlerCalls atomic.Int64
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlerCalls.Add(1)
			if r.Method != http.MethodGet || r.URL.Path != "/blackhole-fallback" {
				t.Errorf("request = %s %s", r.Method, r.URL.Path)
			}
			w.WriteHeader(http.StatusNoContent)
		}),
		ErrorLog: log.New(io.Discard, "", 0),
	}
	serveDone := make(chan struct{})
	go func() {
		_ = server.Serve(tlsListener)
		close(serveDone)
	}()

	var dials atomic.Int64
	var blackhole net.Conn
	networkDial := func(ctx context.Context, network, address string) (net.Conn, error) {
		if dials.Add(1) == 1 {
			clientSide, serverSide := net.Pipe()
			blackhole = serverSide
			return clientSide, nil
		}
		return listener.DialContext(ctx, network, address)
	}
	t.Cleanup(func() {
		if blackhole != nil {
			_ = blackhole.Close()
		}
		_ = server.Close()
		_ = listener.Close()
		select {
		case <-serveDone:
		case <-time.After(time.Second):
			t.Error("HTTP server did not stop")
		}
	})

	baseTransport := http.DefaultTransport.(*http.Transport).Clone()
	baseTransport.Proxy = nil
	baseTransport.DialContext = networkDial
	baseTransport.TLSClientConfig = &tls.Config{
		RootCAs:    roots,
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{"http/1.1"},
	}
	baseTransport.TLSHandshakeTimeout = time.Second
	baseTransport.ForceAttemptHTTP2 = false

	httpClient := &http.Client{
		Transport: newBrokerTransport(
			baseTransport,
			newBrokerECHConfigState(embeddedCloudflareECHConfigList),
			20*time.Millisecond,
		),
		Timeout: 2 * time.Second,
	}
	req, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		"https://"+cloudflareBrokerHost+"/blackhole-fallback",
		nil,
	)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("GET through blackholed ECH fallback: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	if got := dials.Load(); got != 2 {
		t.Fatalf("TLS connection attempts = %d, want blackholed ECH + plain fallback = 2", got)
	}
	if got := handlerCalls.Load(); got != 1 {
		t.Fatalf("HTTP handler calls = %d, want exactly one GET", got)
	}
}

func TestBrokerECHFallbackDoesNotReplayWSSTicketPOST(t *testing.T) {
	certificate, roots := testBrokerCertificate(t)
	listener := newPipeListener()
	tlsListener := tls.NewListener(listener, testBrokerServerConfig(certificate))
	var handlerCalls atomic.Int64
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlerCalls.Add(1)
			if r.Method != http.MethodPost || r.URL.Path != "/api/v1/wss/tickets" {
				t.Errorf("request = %s %s", r.Method, r.URL.Path)
			}
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				t.Errorf("read request body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ticket":"opaque","expires_at":"2026-07-24T12:00:00Z","url":"wss://relay.example/bridge"}`)
		}),
		ErrorLog: log.New(io.Discard, "", 0),
	}
	serveDone := make(chan struct{})
	go func() {
		_ = server.Serve(tlsListener)
		close(serveDone)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
		select {
		case <-serveDone:
		case <-time.After(time.Second):
			t.Error("HTTP server did not stop")
		}
	})

	baseTransport := http.DefaultTransport.(*http.Transport).Clone()
	baseTransport.Proxy = nil
	baseTransport.DialContext = listener.DialContext
	baseTransport.TLSClientConfig = &tls.Config{
		RootCAs:    roots,
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{"http/1.1"},
	}
	baseTransport.TLSHandshakeTimeout = time.Second
	baseTransport.ForceAttemptHTTP2 = false

	httpClient := &http.Client{
		Transport: newBrokerTransport(
			baseTransport,
			newBrokerECHConfigState(embeddedCloudflareECHConfigList),
			time.Second,
		),
		Timeout: 2 * time.Second,
	}
	_, err := (BrokerClient{
		BaseURL:    "https://" + cloudflareBrokerHost,
		HTTPClient: httpClient,
	}).RequestWSSSessionTicket(t.Context(), relay.WSSSessionTicketRequest{
		RelayID: "relay-1",
		FrontID: "front-1",
	}, "client-1", "session-1")
	if err != nil {
		t.Fatalf("request WSS ticket through ECH fallback: %v", err)
	}
	if got := handlerCalls.Load(); got != 1 {
		t.Fatalf("WSS ticket handler calls = %d, want exactly one HTTP POST", got)
	}
	if got := listener.dials.Load(); got != 2 {
		t.Fatalf("TLS connection attempts = %d, want rejected ECH + plain fallback = 2", got)
	}
}

type tlsServerResult struct {
	attempt int64
	state   tls.ConnectionState
	err     error
}

type pipeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
	dials  atomic.Int64
}

func newPipeListener() *pipeListener {
	return &pipeListener{
		conns:  make(chan net.Conn),
		closed: make(chan struct{}),
	}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

func (l *pipeListener) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	l.dials.Add(1)
	clientSide, serverSide := net.Pipe()
	select {
	case l.conns <- serverSide:
		return clientSide, nil
	case <-ctx.Done():
		_ = clientSide.Close()
		_ = serverSide.Close()
		return nil, ctx.Err()
	case <-l.closed:
		_ = clientSide.Close()
		_ = serverSide.Close()
		return nil, net.ErrClosed
	}
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

func testTLSPipeDialer(serverConfig *tls.Config) (
	func(context.Context, string, string) (net.Conn, error),
	<-chan tlsServerResult,
	*atomic.Int64,
) {
	results := make(chan tlsServerResult, 16)
	var calls atomic.Int64
	dial := func(context.Context, string, string) (net.Conn, error) {
		attempt := calls.Add(1)
		clientSide, serverSide := net.Pipe()
		go func() {
			server := tls.Server(serverSide, serverConfig.Clone())
			err := server.Handshake()
			state := server.ConnectionState()
			_ = serverSide.Close()
			results <- tlsServerResult{attempt: attempt, state: state, err: err}
		}()
		return clientSide, nil
	}
	return dial, results, &calls
}

func closeTLSConn(conn net.Conn) {
	_ = conn.SetDeadline(time.Now())
	_ = conn.Close()
}

func readTLSServerResults(t *testing.T, results <-chan tlsServerResult, count int) []tlsServerResult {
	t.Helper()
	got := make([]tlsServerResult, 0, count)
	for len(got) < count {
		select {
		case result := <-results:
			got = append(got, result)
		case <-time.After(2 * time.Second):
			t.Fatalf("received %d of %d TLS server results", len(got), count)
		}
	}
	for index := 1; index < len(got); index++ {
		for current := index; current > 0 && got[current].attempt < got[current-1].attempt; current-- {
			got[current], got[current-1] = got[current-1], got[current]
		}
	}
	return got
}

func testBrokerECHDialer(
	networkDial func(context.Context, string, string) (net.Conn, error),
	roots *x509.CertPool,
	state *brokerECHConfigState,
) *brokerECHDialer {
	return &brokerECHDialer{
		networkDial: networkDial,
		baseTLSConfig: &tls.Config{
			RootCAs:    roots,
			MinVersion: tls.VersionTLS13,
			NextProtos: []string{"http/1.1"},
		},
		state:             state,
		echTimeout:        time.Second,
		tlsHandshakeLimit: time.Second,
	}
}

func testBrokerCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate certificate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "OpenRung ECH test"},
		DNSNames: []string{
			cloudflareBrokerHost,
			testECHPublicName,
			"d2r7mdpyevvs1m.cloudfront.net",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: parsed}, roots
}

func testBrokerServerConfig(certificate tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{"http/1.1"},
	}
}

func testECHConfig(t *testing.T, id uint8, publicName string) ([]byte, []byte, *ecdh.PrivateKey) {
	t.Helper()
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ECH key: %v", err)
	}
	config, configList := marshalTestECHConfig(t, id, key.PublicKey().Bytes(), publicName)
	return config, configList, key
}

func marshalTestECHConfig(t *testing.T, id uint8, publicKey []byte, publicName string) ([]byte, []byte) {
	t.Helper()
	if len(publicKey) != 32 || len(publicName) > 255 {
		t.Fatalf("invalid test ECH config input: public key=%d public name=%d", len(publicKey), len(publicName))
	}

	body := make([]byte, 0, 65)
	body = append(body, id)
	body = appendUint16(body, 0x0020) // DHKEM(X25519, HKDF-SHA256)
	body = appendUint16(body, uint16(len(publicKey)))
	body = append(body, publicKey...)
	body = appendUint16(body, 4)
	body = appendUint16(body, 0x0001) // HKDF-SHA256
	body = appendUint16(body, 0x0001) // AES-128-GCM
	body = append(body, 64, byte(len(publicName)))
	body = append(body, publicName...)
	body = appendUint16(body, 0) // extensions

	config := make([]byte, 0, len(body)+4)
	config = appendUint16(config, 0xfe0d)
	config = appendUint16(config, uint16(len(body)))
	config = append(config, body...)

	configList := make([]byte, 0, len(config)+2)
	configList = appendUint16(configList, uint16(len(config)))
	configList = append(configList, config...)
	return config, configList
}

func appendUint16(dst []byte, value uint16) []byte {
	var encoded [2]byte
	binary.BigEndian.PutUint16(encoded[:], value)
	return append(dst, encoded[:]...)
}
