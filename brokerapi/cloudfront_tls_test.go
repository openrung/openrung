// SPDX-License-Identifier: GPL-3.0-or-later

package brokerapi

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/fips140"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	// The rule is the distribution-name shape, not this deployment's front, so
	// a fork's own distribution is concealed too and rotating the front cannot
	// silently put the name back on the wire.
	testCloudFrontHost      = cloudFrontBrokerHost
	testOtherCloudFrontHost = "d111111abcdef8.cloudfront.net"
	testCloudFrontWildcard  = "*.cloudfront.net"
)

// testCloudFrontChain mints the certificate shape CloudFront actually serves:
// a wildcard leaf under an intermediate, with only the root trusted. A
// self-signed leaf in the root pool would short-circuit chain building, leaving
// the intermediates this path collects from PeerCertificates unexercised.
func testCloudFrontChain(t *testing.T, dnsNames ...string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	authority := newTestAuthority(t)
	return authority.issue(t, dnsNames...), authority.roots
}

// testAuthority issues several leaves under one trust root, so a test can
// present a certificate that is properly signed yet valid for another name.
type testAuthority struct {
	intermediate    *x509.Certificate
	intermediateDER []byte
	intermediateKey *ecdsa.PrivateKey
	roots           *x509.CertPool
}

func (a *testAuthority) issue(t *testing.T, dnsNames ...string) tls.Certificate {
	t.Helper()
	now := time.Now()
	leafDER, leaf, leafKey := testIssueCertificate(t, &x509.Certificate{
		SerialNumber:          big.NewInt(3),
		Subject:               pkix.Name{CommonName: dnsNames[0]},
		DNSNames:              dnsNames,
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}, a.intermediate, a.intermediateKey)
	return tls.Certificate{
		Certificate: [][]byte{leafDER, a.intermediateDER},
		PrivateKey:  leafKey,
		Leaf:        leaf,
	}
}

func newTestAuthority(t *testing.T) *testAuthority {
	t.Helper()
	now := time.Now()
	authority := func(serial int64, name string) *x509.Certificate {
		return &x509.Certificate{
			SerialNumber:          big.NewInt(serial),
			Subject:               pkix.Name{CommonName: name},
			NotBefore:             now.Add(-time.Hour),
			NotAfter:              now.Add(time.Hour),
			KeyUsage:              x509.KeyUsageCertSign,
			BasicConstraintsValid: true,
			IsCA:                  true,
		}
	}
	_, root, rootKey := testIssueCertificate(t, authority(1, "OpenRung test root"), nil, nil)
	intermediateDER, intermediate, intermediateKey := testIssueCertificate(
		t, authority(2, "OpenRung test intermediate"), root, rootKey,
	)
	roots := x509.NewCertPool()
	roots.AddCert(root)
	return &testAuthority{
		intermediate:    intermediate,
		intermediateDER: intermediateDER,
		intermediateKey: intermediateKey,
		roots:           roots,
	}
}

func testIssueCertificate(
	t *testing.T,
	template, parent *x509.Certificate,
	parentKey *ecdsa.PrivateKey,
) ([]byte, *x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key for %s: %v", template.Subject.CommonName, err)
	}
	signer, signerKey := parent, parentKey
	if signer == nil {
		signer, signerKey = template, key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, signer, key.Public(), signerKey)
	if err != nil {
		t.Fatalf("create certificate for %s: %v", template.Subject.CommonName, err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate for %s: %v", template.Subject.CommonName, err)
	}
	return der, parsed, key
}

// testTLSSocketDialer is the loopback-socket counterpart of testTLSPipeDialer,
// for tests that assert the client's own handshake error. A synchronous
// net.Pipe deadlocks a client alert against the server's still-unwritten
// handshake flight, so a rejected certificate would surface as a handshake
// deadline instead of the verification failure that caused it.
func testTLSSocketDialer(t *testing.T, serverConfig *tls.Config) (
	func(context.Context, string, string) (net.Conn, error),
	<-chan tlsServerResult,
	*atomic.Int64,
) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for TLS server: %v", err)
	}
	results := make(chan tlsServerResult, 16)
	var calls, accepted atomic.Int64
	var connections sync.WaitGroup
	accepting := make(chan struct{})
	go func() {
		defer close(accepting)
		for {
			serverSide, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			attempt := accepted.Add(1)
			connections.Add(1)
			go func() {
				defer connections.Done()
				server := tls.Server(serverSide, serverConfig.Clone())
				handshakeErr := server.Handshake()
				state := server.ConnectionState()
				_ = serverSide.Close()
				results <- tlsServerResult{attempt: attempt, state: state, err: handshakeErr}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-accepting
		connections.Wait()
	})

	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		calls.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, listener.Addr().String())
	}
	return dial, results, &calls
}

func TestCloudFrontDistributionAddress(t *testing.T) {
	for _, test := range []struct {
		address string
		host    string
		ok      bool
	}{
		{address: testCloudFrontHost + ":443", host: testCloudFrontHost, ok: true},
		{address: testOtherCloudFrontHost + ":443", host: testOtherCloudFrontHost, ok: true},
		{address: "D2R7MDPYEVVS1M.CLOUDFRONT.NET:443", host: testCloudFrontHost, ok: true},
		{address: testCloudFrontHost + ".:443", host: testCloudFrontHost, ok: true},
		{address: testCloudFrontHost + ":8443"},
		{address: "cloudfront.net:443"},
		{address: ".cloudfront.net:443"},
		{address: "nested." + testCloudFrontHost + ":443"},
		{address: testCloudFrontHost + ".example:443"},
		{address: cloudflareBrokerHost + ":443"},
		{address: "127.0.0.1:443"},
		{address: "not-an-address"},
	} {
		host, ok := cloudFrontDistributionAddress(test.address)
		if host != test.host || ok != test.ok {
			t.Errorf(
				"cloudFrontDistributionAddress(%q) = (%q, %v), want (%q, %v)",
				test.address, host, ok, test.host, test.ok,
			)
		}
	}
}

func TestBrokerCloudFrontDialOmitsSNI(t *testing.T) {
	for _, distribution := range []string{testCloudFrontHost, testOtherCloudFrontHost} {
		t.Run(distribution, func(t *testing.T) {
			certificate, roots := testCloudFrontChain(t, testCloudFrontWildcard)
			networkDial, results, calls := testTLSPipeDialer(testBrokerServerConfig(certificate))
			dialer := testBrokerECHDialer(networkDial, roots, newECHConfigState(embeddedCloudflareECHConfigList))

			conn, err := dialer.dialTLSContext(t.Context(), "tcp", distribution+":443")
			if err != nil {
				t.Fatalf("no-SNI CloudFront dial: %v", err)
			}
			defer closeTLSConn(conn)

			if state := conn.(*tls.Conn).ConnectionState(); !state.HandshakeComplete || state.ServerName != "" {
				t.Fatalf("client state = complete %t, ServerName %q", state.HandshakeComplete, state.ServerName)
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("TLS connection attempts = %d, want one no-SNI attempt", got)
			}
			result := readTLSServerResults(t, results, 1)[0]
			if result.err != nil {
				t.Fatalf("server handshake: %v", result.err)
			}
			if result.state.ServerName != "" {
				t.Fatalf(
					"ClientHello server name = %q, want the distribution name to stay off the wire",
					result.state.ServerName,
				)
			}
		})
	}
}

// The distribution name is verified against the wildcard certificate CloudFront
// serves without SNI, so a broker front cannot be impersonated by any other
// certificate the roots happen to trust.
func TestBrokerCloudFrontDialRejectsInvalidCertificates(t *testing.T) {
	for _, test := range []struct {
		name             string
		certificateNames []string
		discardTrustRoot bool
		currentTime      time.Time
	}{
		{name: "wrong hostname", certificateNames: []string{"*.example.net", "cloudfront.net"}},
		{name: "wrong label depth", certificateNames: []string{"*.d2r7mdpyevvs1m.cloudfront.net"}},
		{name: "untrusted root", certificateNames: []string{testCloudFrontWildcard}, discardTrustRoot: true},
		{
			name:             "expired",
			certificateNames: []string{testCloudFrontWildcard},
			currentTime:      time.Now().Add(2 * time.Hour),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			certificate, roots := testCloudFrontChain(t, test.certificateNames...)
			if test.discardTrustRoot {
				roots = x509.NewCertPool()
			}
			networkDial, results, calls := testTLSSocketDialer(t, testBrokerServerConfig(certificate))
			dialer := testBrokerECHDialer(networkDial, roots, newECHConfigState(embeddedCloudflareECHConfigList))
			if !test.currentTime.IsZero() {
				dialer.baseTLSConfig.Time = func() time.Time { return test.currentTime }
			}
			callerVerifierRan := false
			dialer.baseTLSConfig.VerifyConnection = func(tls.ConnectionState) error {
				callerVerifierRan = true
				return nil
			}

			conn, err := dialer.dialTLSContext(t.Context(), "tcp", testCloudFrontHost+":443")
			if conn != nil {
				closeTLSConn(conn)
				t.Fatal("no-SNI dial returned a connection for an invalid certificate")
			}
			var verificationErr *tls.CertificateVerificationError
			if !errors.As(err, &verificationErr) {
				t.Fatalf("no-SNI dial error = %v, want a certificate verification error", err)
			}
			if callerVerifierRan {
				t.Fatal("caller verification hook ran before mandatory certificate verification")
			}
			// A retry would put the distribution name back on the wire, which is
			// the whole thing this path exists to prevent.
			if got := calls.Load(); got != 1 {
				t.Fatalf("TLS connection attempts = %d, want no SNI-bearing retry", got)
			}
			if serverName := readTLSServerResults(t, results, 1)[0].state.ServerName; serverName != "" {
				t.Fatalf("ClientHello server name = %q, want empty", serverName)
			}
		})
	}
}

func TestBrokerCloudFrontDialRunsCallerHooksAfterVerification(t *testing.T) {
	certificate, roots := testCloudFrontChain(t, testCloudFrontWildcard)
	networkDial, results, _ := testTLSPipeDialer(testBrokerServerConfig(certificate))
	dialer := testBrokerECHDialer(networkDial, roots, newECHConfigState(embeddedCloudflareECHConfigList))

	peerHookRan := false
	connectionHookRan := false
	dialer.baseTLSConfig.VerifyPeerCertificate = func(raw [][]byte, chains [][]*x509.Certificate) error {
		if len(raw) != 2 || len(chains) == 0 {
			return fmt.Errorf("peer hook received raw=%d chains=%d", len(raw), len(chains))
		}
		peerHookRan = true
		return nil
	}
	dialer.baseTLSConfig.VerifyConnection = func(state tls.ConnectionState) error {
		if state.ServerName != "" || len(state.VerifiedChains) == 0 {
			return fmt.Errorf("connection hook received SNI %q and %d chains", state.ServerName, len(state.VerifiedChains))
		}
		connectionHookRan = true
		return nil
	}

	conn, err := dialer.dialTLSContext(t.Context(), "tcp", testCloudFrontHost+":443")
	if err != nil {
		t.Fatalf("no-SNI CloudFront dial: %v", err)
	}
	defer closeTLSConn(conn)
	if result := readTLSServerResults(t, results, 1)[0]; result.err != nil {
		t.Fatalf("server handshake: %v", result.err)
	}
	if !peerHookRan || !connectionHookRan {
		t.Fatalf("caller hooks ran: peer=%v connection=%v", peerHookRan, connectionHookRan)
	}
}

// A rejected ECH handshake skips both verification hooks and verifies against
// the configured server name, which is empty on this path. The no-SNI config
// must therefore never carry an ECH config, whatever the base transport holds.
func TestBrokerCloudFrontDialIgnoresECHConfiguration(t *testing.T) {
	t.Run("handshake stays plain", func(t *testing.T) {
		certificate, roots := testCloudFrontChain(t, testCloudFrontWildcard)
		networkDial, results, calls := testTLSPipeDialer(testBrokerServerConfig(certificate))
		dialer := testBrokerECHDialer(networkDial, roots, newECHConfigState(embeddedCloudflareECHConfigList))
		dialer.baseTLSConfig.EncryptedClientHelloConfigList = embeddedCloudflareECHConfigList

		conn, err := dialer.dialTLSContext(t.Context(), "tcp", testCloudFrontHost+":443")
		if err != nil {
			t.Fatalf("no-SNI CloudFront dial: %v", err)
		}
		defer closeTLSConn(conn)
		if got := calls.Load(); got != 1 {
			t.Fatalf("TLS connection attempts = %d, want one no-SNI attempt", got)
		}
		result := readTLSServerResults(t, results, 1)[0]
		if result.err != nil || result.state.ECHAccepted || result.state.ServerName != "" {
			t.Fatalf(
				"server handshake = err %v, ECHAccepted %t, ServerName %q",
				result.err, result.state.ECHAccepted, result.state.ServerName,
			)
		}
	})

	t.Run("verification still applies", func(t *testing.T) {
		authority := newTestAuthority(t)
		certificate := authority.issue(t, "*.example.net")
		networkDial, results, _ := testTLSSocketDialer(t, testBrokerServerConfig(certificate))
		dialer := testBrokerECHDialer(networkDial, authority.roots, newECHConfigState(embeddedCloudflareECHConfigList))
		dialer.baseTLSConfig.EncryptedClientHelloConfigList = embeddedCloudflareECHConfigList

		conn, err := dialer.dialTLSContext(t.Context(), "tcp", testCloudFrontHost+":443")
		if conn != nil {
			closeTLSConn(conn)
			t.Fatal("a certificate for another name was accepted on the no-SNI path")
		}
		var hostnameErr x509.HostnameError
		if !errors.As(err, &hostnameErr) {
			t.Fatalf("no-SNI dial error = %v, want an x509 hostname error", err)
		}
		// A retained ECH config would put the public name on the wire and
		// verify against it, so the observed server name is the tell.
		if serverName := readTLSServerResults(t, results, 1)[0].state.ServerName; serverName != "" {
			t.Fatalf("ClientHello server name = %q, want empty", serverName)
		}
	})
}

func TestBrokerCloudFrontDialKeepsTransportALPN(t *testing.T) {
	certificate, roots := testCloudFrontChain(t, testCloudFrontWildcard)
	serverConfig := testBrokerServerConfig(certificate)
	serverConfig.NextProtos = []string{"h2", "http/1.1"}
	networkDial, results, _ := testTLSPipeDialer(serverConfig)
	dialer := testBrokerECHDialer(networkDial, roots, newECHConfigState(embeddedCloudflareECHConfigList))
	dialer.baseTLSConfig.NextProtos = []string{"h2", "http/1.1"}

	conn, err := dialer.dialTLSContext(t.Context(), "tcp", testCloudFrontHost+":443")
	if err != nil {
		t.Fatalf("no-SNI CloudFront dial: %v", err)
	}
	defer closeTLSConn(conn)
	result := readTLSServerResults(t, results, 1)[0]
	if result.err != nil {
		t.Fatalf("server handshake: %v", result.err)
	}
	// Dropping ALPN here would silently downgrade every CloudFront broker
	// request to HTTP/1.1.
	if got := result.state.NegotiatedProtocol; got != "h2" {
		t.Fatalf("negotiated protocol = %q, want h2", got)
	}
}

func TestNoSNIConfigPreservesTransportPolicy(t *testing.T) {
	roots := x509.NewCertPool()
	clock := func() time.Time { return time.Unix(1, 0) }
	base := &tls.Config{
		ServerName:                          "leaked.example",
		RootCAs:                             roots,
		MinVersion:                          tls.VersionTLS13,
		NextProtos:                          []string{"h2", "http/1.1"},
		Time:                                clock,
		ClientSessionCache:                  tls.NewLRUClientSessionCache(8),
		EncryptedClientHelloConfigList:      embeddedCloudflareECHConfigList,
		EncryptedClientHelloRejectionVerify: func(tls.ConnectionState) error { return nil },
	}
	config := verifiedNoSNIConfig(base, testCloudFrontHost)

	if config.ServerName != "" || !config.InsecureSkipVerify || config.VerifyConnection == nil {
		t.Fatalf(
			"config = ServerName %q, InsecureSkipVerify %t, VerifyConnection set %t",
			config.ServerName, config.InsecureSkipVerify, config.VerifyConnection != nil,
		)
	}
	if config.RootCAs != roots || config.MinVersion != tls.VersionTLS13 || len(config.NextProtos) != 2 {
		t.Fatalf(
			"transport TLS policy was not preserved: roots kept %t, MinVersion %x, NextProtos %v",
			config.RootCAs == roots, config.MinVersion, config.NextProtos,
		)
	}
	if config.Time == nil || !config.Time().Equal(clock()) {
		t.Fatal("transport clock was not preserved")
	}
	if config.EncryptedClientHelloConfigList != nil || config.EncryptedClientHelloRejectionVerify != nil {
		t.Fatal("ECH configuration survived into the no-SNI config")
	}
	// An empty server name degrades crypto/tls's session cache key to the peer
	// address, which would offer one distribution's ticket to another.
	if config.ClientSessionCache != nil || !config.SessionTicketsDisabled {
		t.Fatalf(
			"session resumption is still available: cache set %t, tickets disabled %t",
			config.ClientSessionCache != nil, config.SessionTicketsDisabled,
		)
	}
	if base.ServerName != "leaked.example" || base.EncryptedClientHelloConfigList == nil {
		t.Fatal("the caller's base TLS config was mutated")
	}
}

// x509 skips hostname verification for an empty DNSName, so without the guard a
// chain that is merely trusted would authenticate any host at all. The state
// below is otherwise fully verifiable, leaving the missing name as the only
// possible reason to reject it.
func TestNoSNIConfigRefusesEmptyVerificationName(t *testing.T) {
	authority := newTestAuthority(t)
	certificate := authority.issue(t, testCloudFrontWildcard)
	state := tls.ConnectionState{PeerCertificates: testPeerCertificates(t, certificate)}

	if err := verifiedNoSNIConfig(&tls.Config{RootCAs: authority.roots}, testCloudFrontHost).
		VerifyConnection(state); err != nil {
		t.Fatalf("the same state does not verify under a real name: %v", err)
	}
	err := verifiedNoSNIConfig(&tls.Config{RootCAs: authority.roots}, "").VerifyConnection(state)
	if err == nil {
		t.Fatal("an empty verification name verified a certificate against no hostname at all")
	}
	var verificationErr *tls.CertificateVerificationError
	if errors.As(err, &verificationErr) {
		t.Fatalf("empty name was rejected by certificate verification, not by the guard: %v", err)
	}
}

func testPeerCertificates(t *testing.T, certificate tls.Certificate) []*x509.Certificate {
	t.Helper()
	peers := make([]*x509.Certificate, 0, len(certificate.Certificate))
	for _, der := range certificate.Certificate {
		parsed, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatalf("parse presented certificate: %v", err)
		}
		peers = append(peers, parsed)
	}
	return peers
}

// The Host header CloudFront routes on stays inside the encrypted stream, so
// suppressing SNI costs the request nothing.
func TestBrokerCloudFrontRequestKeepsHostHeaderOffTheWire(t *testing.T) {
	certificate, roots := testCloudFrontChain(t, testCloudFrontWildcard)
	listener := newPipeListener()
	tlsListener := tls.NewListener(listener, testBrokerServerConfig(certificate))
	var handlerCalls atomic.Int64
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlerCalls.Add(1)
			if r.TLS == nil || r.TLS.ServerName != "" {
				t.Errorf("request arrived with TLS server name %q", r.TLS.ServerName)
			}
			if r.Host != testCloudFrontHost {
				t.Errorf("HTTP Host = %q, want %q", r.Host, testCloudFrontHost)
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
		Transport: newTransport(baseTransport, newECHConfigState(embeddedCloudflareECHConfigList), time.Second),
		Timeout:   2 * time.Second,
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, CloudFrontBrokerURL+"healthz", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("GET through the no-SNI CloudFront front: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	if got := handlerCalls.Load(); got != 1 {
		t.Fatalf("HTTP handler calls = %d, want exactly one GET", got)
	}
	if got := listener.dials.Load(); got != 1 {
		t.Fatalf("TLS connection attempts = %d, want one no-SNI attempt", got)
	}
}

// Everything CloudFront's default certificate does not cover keeps ordinary
// SNI: without it those hosts cannot be verified at all.
func TestBrokerNonDistributionAddressesKeepSNI(t *testing.T) {
	for _, test := range []struct {
		name    string
		address string
		want    string
	}{
		{name: "custom broker", address: "broker.example.org:443", want: "broker.example.org"},
		{name: "alternate port", address: testCloudFrontHost + ":8443", want: testCloudFrontHost},
		{
			name:    "nested distribution label",
			address: "nested." + testCloudFrontHost + ":443",
			want:    "nested." + testCloudFrontHost,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			certificate, roots := testCertificate(t, test.want)
			networkDial, results, _ := testTLSPipeDialer(testBrokerServerConfig(certificate))
			dialer := testBrokerECHDialer(networkDial, roots, newECHConfigState(embeddedCloudflareECHConfigList))

			conn, err := dialer.dialTLSContext(t.Context(), "tcp", test.address)
			if err != nil {
				t.Fatalf("ordinary TLS dial: %v", err)
			}
			defer closeTLSConn(conn)
			result := readTLSServerResults(t, results, 1)[0]
			if result.err != nil {
				t.Fatalf("server handshake: %v", result.err)
			}
			if result.state.ServerName != test.want {
				t.Fatalf("ClientHello server name = %q, want %q", result.state.ServerName, test.want)
			}
		})
	}
}

func TestNoSNIDialUsesTransportNetworkDialerOnce(t *testing.T) {
	dialErr := errors.New("network dial refused")
	var calls atomic.Int64
	dialer := testBrokerECHDialer(
		func(context.Context, string, string) (net.Conn, error) {
			calls.Add(1)
			return nil, dialErr
		},
		x509.NewCertPool(),
		newECHConfigState(embeddedCloudflareECHConfigList),
	)

	conn, err := dialer.dialTLSContext(t.Context(), "tcp", testCloudFrontHost+":443")
	if conn != nil {
		closeTLSConn(conn)
		t.Fatal("no-SNI dial returned a connection after the network dial failed")
	}
	if !errors.Is(err, dialErr) {
		t.Fatalf("no-SNI dial error = %v, want %v", err, dialErr)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("network dials = %d, want 1", got)
	}
}

// The recognizer works on the dialed address, so a front URL repointed at a
// custom CNAME would silently lose SNI concealment rather than fail.
func TestBrokerCloudFrontConstantsStayLinked(t *testing.T) {
	parsed, err := EnforceSecureBrokerURL(CloudFrontBrokerURL)
	if err != nil {
		t.Fatalf("parse %s: %v", CloudFrontBrokerURL, err)
	}
	if parsed.Hostname() != cloudFrontBrokerHost {
		t.Fatalf("CloudFrontBrokerURL host = %q, want %q", parsed.Hostname(), cloudFrontBrokerHost)
	}
	address := net.JoinHostPort(parsed.Hostname(), "443")
	if host, ok := cloudFrontDistributionAddress(address); !ok || host != cloudFrontBrokerHost {
		t.Fatalf("the CloudFront front is not recognized as a distribution: (%q, %v)", host, ok)
	}
	// Each built-in front conceals its hostname by a different mechanism, and
	// no front may end up on both paths.
	if isCloudflareBrokerAddress(address) {
		t.Fatal("the CloudFront front also matches the ECH front")
	}
	cloudflareAddress := net.JoinHostPort(cloudflareBrokerHost, "443")
	if _, ok := cloudFrontDistributionAddress(cloudflareAddress); ok {
		t.Fatal("the Cloudflare front also matches the no-SNI path")
	}
	if !isCloudflareBrokerAddress(cloudflareAddress) {
		t.Fatal("the Cloudflare front no longer matches the ECH path")
	}
}

// The shared process-global transport is what every production caller gets, so
// the config it hands the no-SNI path must be safe as built.
func TestDefaultTransportBaseYieldsSafeNoSNIConfig(t *testing.T) {
	base := http.DefaultTransport.(*http.Transport).Clone().TLSClientConfig
	config := verifiedNoSNIConfig(base, cloudFrontBrokerHost)

	if config.InsecureSkipVerify != true || config.VerifyConnection == nil {
		t.Fatal("the mandatory verification hook is not installed")
	}
	if config.ServerName != "" || config.EncryptedClientHelloConfigList != nil {
		t.Fatalf("config = ServerName %q, ECH list %x", config.ServerName, config.EncryptedClientHelloConfigList)
	}
	if config.ClientSessionCache != nil || !config.SessionTicketsDisabled {
		t.Fatal("session resumption is available on the no-SNI path")
	}
	// Nil roots select the system pool or the platform verifier; an empty pool
	// would trust nothing at all.
	if config.RootCAs != nil {
		t.Fatal("the system root pool was replaced")
	}
	if len(config.NextProtos) != len(base.NextProtos) {
		t.Fatalf("ALPN offer = %v, want the transport's %v", config.NextProtos, base.NextProtos)
	}
}

func TestNoSNIConfigIsNilSafe(t *testing.T) {
	config := verifiedNoSNIConfig(nil, cloudFrontBrokerHost)
	if config == nil || config.VerifyConnection == nil || !config.InsecureSkipVerify {
		t.Fatal("a nil base config did not produce a usable no-SNI config")
	}
}

// crypto/tls skips VerifyPeerCertificate on a resumed session but always runs
// VerifyConnection, so the chain is re-verified where stock TLS would not.
func TestNoSNIConfigVerifiesResumedConnections(t *testing.T) {
	authority := newTestAuthority(t)
	peerHookRan := false
	config := verifiedNoSNIConfig(&tls.Config{
		RootCAs: authority.roots,
		VerifyPeerCertificate: func([][]byte, [][]*x509.Certificate) error {
			peerHookRan = true
			return nil
		},
	}, cloudFrontBrokerHost)

	chain := testPeerCertificates(t, authority.issue(t, testCloudFrontWildcard))
	if err := config.VerifyConnection(tls.ConnectionState{DidResume: true, PeerCertificates: chain}); err != nil {
		t.Fatalf("resumed connection verification: %v", err)
	}
	if peerHookRan {
		t.Fatal("VerifyPeerCertificate ran on a resumed connection")
	}
	// Same trust root, different name: the rejection must come from the
	// hostname, not from an issuer the test happened not to trust.
	elsewhere := testPeerCertificates(t, authority.issue(t, "*.example.net"))
	err := config.VerifyConnection(tls.ConnectionState{DidResume: true, PeerCertificates: elsewhere})
	var hostnameErr x509.HostnameError
	if !errors.As(err, &hostnameErr) {
		t.Fatalf("resumed session for another name = %v, want an x509 hostname error", err)
	}
}

// A censor that completes the TCP handshake and then swallows the ClientHello
// must not hold a broker request past the front-racing budget.
func TestBrokerCloudFrontDialBoundsBlackholedHandshake(t *testing.T) {
	var blackhole net.Conn
	t.Cleanup(func() {
		if blackhole != nil {
			_ = blackhole.Close()
		}
	})
	dialer := testBrokerECHDialer(
		func(context.Context, string, string) (net.Conn, error) {
			clientSide, serverSide := net.Pipe()
			blackhole = serverSide
			return clientSide, nil
		},
		x509.NewCertPool(),
		newECHConfigState(embeddedCloudflareECHConfigList),
	)
	dialer.tlsHandshakeLimit = 40 * time.Millisecond

	started := time.Now()
	conn, err := dialer.dialTLSContext(t.Context(), "tcp", testCloudFrontHost+":443")
	elapsed := time.Since(started)
	if conn != nil {
		closeTLSConn(conn)
		t.Fatal("a blackholed handshake returned a connection")
	}
	if err == nil {
		t.Fatal("a blackholed handshake did not fail")
	}
	if elapsed > time.Second {
		t.Fatalf("blackholed no-SNI handshake took %v, want the transport's handshake budget", elapsed)
	}
}

// The FIPS setting is fixed at process start, so the assertion runs in a
// re-executed copy of this test binary rather than in the parent.
const fipsModeChildEnv = "OPENRUNG_TEST_FIPS_CHILD"

func TestBrokerCloudFrontDialRefusesFIPSMode(t *testing.T) {
	if os.Getenv(fipsModeChildEnv) != "1" {
		command := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
		command.Env = append(os.Environ(), fipsModeChildEnv+"=1", "GODEBUG=fips140=on")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("FIPS-mode child process: %v\n%s", err, output)
		}
		// A child that matched no test also exits zero.
		if !bytes.Contains(output, []byte("PASS: "+t.Name())) {
			t.Fatalf("FIPS-mode child ran no assertions:\n%s", output)
		}
		return
	}

	// Without this the whole test would pass vacuously on a toolchain that
	// ignores the setting.
	if !fips140.Enabled() {
		t.Fatal("GODEBUG=fips140=on did not enable FIPS mode in the child process")
	}

	var dials atomic.Int64
	dialer := testBrokerECHDialer(
		func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			return nil, errors.New("the no-SNI path opened a connection in FIPS mode")
		},
		x509.NewCertPool(),
		newECHConfigState(embeddedCloudflareECHConfigList),
	)

	conn, err := dialer.dialTLSContext(t.Context(), "tcp", testCloudFrontHost+":443")
	if conn != nil {
		closeTLSConn(conn)
		t.Fatal("the no-SNI path returned a connection under a chain policy it cannot enforce")
	}
	if !errors.Is(err, errNoSNIInFIPSMode) {
		t.Fatalf("no-SNI dial in FIPS mode = %v, want %v", err, errNoSNIInFIPSMode)
	}
	if got := dials.Load(); got != 0 {
		t.Fatalf("network dials = %d, want none before the refusal", got)
	}

	// Refusing must be confined to this path: crypto/tls enforces the same
	// policy itself wherever an ordinary SNI-bearing dial is used.
	certificate, roots := testCertificate(t, testCustomBrokerHost)
	networkDial, results, _ := testTLSPipeDialer(testBrokerServerConfig(certificate))
	plainDialer := testBrokerECHDialer(networkDial, roots, newECHConfigState(embeddedCloudflareECHConfigList))
	plainConn, err := plainDialer.dialTLSContext(t.Context(), "tcp", testCustomBrokerHost+":443")
	if err != nil {
		t.Fatalf("ordinary TLS dial in FIPS mode: %v", err)
	}
	defer closeTLSConn(plainConn)
	if result := readTLSServerResults(t, results, 1)[0]; result.err != nil {
		t.Fatalf("server handshake in FIPS mode: %v", result.err)
	}
}
