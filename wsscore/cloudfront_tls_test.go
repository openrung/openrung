package wsscore

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
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
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type observedTLSEdge struct {
	server  *httptest.Server
	dialer  *websocket.Dialer
	sni     <-chan string
	host    <-chan string
	release chan struct{}
}

func newObservedTLSEdge(t *testing.T, certificateNames ...string) *observedTLSEdge {
	t.Helper()
	certificate, roots := newTestServerCertificate(t, certificateNames...)
	sni := make(chan string, 1)
	host := make(chan string, 1)
	release := make(chan struct{})
	upgrader := websocket.Upgrader{
		Subprotocols: []string{Subprotocol},
		CheckOrigin:  func(*http.Request) bool { return true },
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		host <- request.Host
		connection, err := upgrader.Upgrade(w, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		<-release
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			sni <- hello.ServerName
			return nil, nil
		},
	}
	server.StartTLS()

	edge := &observedTLSEdge{
		server: server,
		dialer: &websocket.Dialer{
			TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
			NetDialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
			},
		},
		sni: sni, host: host, release: release,
	}
	// Cleanup is LIFO: release the upgraded handler before Server.Close waits
	// for it, including when an assertion aborts the test early.
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	return edge
}

func newTestServerCertificate(t *testing.T, dnsNames ...string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	now := time.Now()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rootTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "wsscore test root"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	rootCertificate, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: dnsNames[0]},
		DNSNames:              dnsNames,
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, rootCertificate, &serverKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(rootCertificate)
	return tls.Certificate{Certificate: [][]byte{serverDER, rootDER}, PrivateKey: serverKey}, roots
}

func receiveTLSObservation(t *testing.T, name string, values <-chan string) string {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
		return ""
	}
}

func TestCloudFrontDistributionHost(t *testing.T) {
	for _, test := range []struct {
		url  string
		host string
		ok   bool
	}{
		{url: "wss://d111111abcdef8.cloudfront.net" + BridgePath, host: "d111111abcdef8.cloudfront.net", ok: true},
		{url: "wss://cdn.example.com" + BridgePath},
		{url: "wss://cloudfront.net" + BridgePath},
		{url: "wss://nested.d111111abcdef8.cloudfront.net" + BridgePath},
		{url: "wss://d111111abcdef8.cloudfront.net.example" + BridgePath},
		{url: ":// malformed"},
	} {
		host, ok := cloudFrontDistributionHost(test.url)
		if host != test.host || ok != test.ok {
			t.Errorf("cloudFrontDistributionHost(%q) = (%q, %v), want (%q, %v)", test.url, host, ok, test.host, test.ok)
		}
	}
}

func TestNoSNITLSDialUsesSelectedNetworkDialerOnce(t *testing.T) {
	calls := 0
	dial := noSNITLSDialContext(
		func(context.Context, string, string) (net.Conn, error) {
			calls++
			return nil, ErrSocketProtectionFailed
		},
		&tls.Config{MinVersion: tls.VersionTLS12},
		"d111111abcdef8.cloudfront.net",
	)
	connection, err := dial(t.Context(), "tcp", "d111111abcdef8.cloudfront.net:443")
	if connection != nil {
		_ = connection.Close()
		t.Fatal("TLS dial returned a connection after the selected network dialer failed")
	}
	if !errors.Is(err, ErrSocketProtectionFailed) {
		t.Fatalf("TLS dial error = %v, want ErrSocketProtectionFailed", err)
	}
	if calls != 1 {
		t.Fatalf("selected network dialer calls = %d, want 1", calls)
	}
}

func TestDialClientCloudFrontNoSNIOmitsSNIAndPreservesHost(t *testing.T) {
	const distributionHost = "d111111abcdef8.cloudfront.net"
	edge := newObservedTLSEdge(t, "*.cloudfront.net")
	peerCallbackCalled := false
	connectionCallbackCalled := false
	edge.dialer.TLSClientConfig.VerifyPeerCertificate = func(rawCertificates [][]byte, verifiedChains [][]*x509.Certificate) error {
		if len(rawCertificates) != 2 || len(verifiedChains) == 0 {
			return fmt.Errorf("verification callback received raw=%d chains=%d", len(rawCertificates), len(verifiedChains))
		}
		peerCallbackCalled = true
		return nil
	}
	edge.dialer.TLSClientConfig.VerifyConnection = func(state tls.ConnectionState) error {
		if state.ServerName != "" || len(state.VerifiedChains) == 0 {
			return fmt.Errorf("connection callback received SNI %q and %d chains", state.ServerName, len(state.VerifiedChains))
		}
		connectionCallbackCalled = true
		return nil
	}

	client, err := DialClient(t.Context(), ClientOptions{
		URL: "wss://" + distributionHost + BridgePath, Ticket: "cloudfront-ticket",
		WebSocketDialer: edge.dialer, CloudFrontNoSNI: true, PingInterval: -1,
	})
	if err != nil {
		t.Fatalf("DialClient: %v", err)
	}
	defer client.Close()
	if sni := receiveTLSObservation(t, "ClientHello", edge.sni); sni != "" {
		t.Fatalf("ClientHello SNI = %q, want empty", sni)
	}
	if host := receiveTLSObservation(t, "HTTP Host", edge.host); host != distributionHost {
		t.Fatalf("HTTP Host = %q, want %q", host, distributionHost)
	}
	if !peerCallbackCalled || !connectionCallbackCalled {
		t.Fatalf("TLS verification callbacks called: peer=%v connection=%v", peerCallbackCalled, connectionCallbackCalled)
	}
}

func TestDialClientCloudFrontNoSNIIsOptIn(t *testing.T) {
	const distributionHost = "d111111abcdef8.cloudfront.net"
	edge := newObservedTLSEdge(t, "*.cloudfront.net")
	client, err := DialClient(t.Context(), ClientOptions{
		URL: "wss://" + distributionHost + BridgePath, Ticket: "ordinary-sni-ticket",
		WebSocketDialer: edge.dialer, PingInterval: -1,
	})
	if err != nil {
		t.Fatalf("DialClient: %v", err)
	}
	defer client.Close()
	if sni := receiveTLSObservation(t, "ClientHello", edge.sni); sni != distributionHost {
		t.Fatalf("ClientHello SNI = %q, want %q", sni, distributionHost)
	}
	if host := receiveTLSObservation(t, "HTTP Host", edge.host); host != distributionHost {
		t.Fatalf("HTTP Host = %q, want %q", host, distributionHost)
	}
}

func TestDialClientCloudFrontNoSNIRejectsInvalidCertificates(t *testing.T) {
	const distributionHost = "d111111abcdef8.cloudfront.net"
	for _, test := range []struct {
		name             string
		certificateName  string
		discardTrustRoot bool
		currentTime      time.Time
	}{
		{name: "wrong hostname", certificateName: "other.example"},
		{name: "untrusted root", certificateName: "*.cloudfront.net", discardTrustRoot: true},
		{name: "expired", certificateName: "*.cloudfront.net", currentTime: time.Now().Add(2 * time.Hour)},
	} {
		t.Run(test.name, func(t *testing.T) {
			edge := newObservedTLSEdge(t, test.certificateName)
			if test.discardTrustRoot {
				edge.dialer.TLSClientConfig.RootCAs = x509.NewCertPool()
			}
			if !test.currentTime.IsZero() {
				edge.dialer.TLSClientConfig.Time = func() time.Time { return test.currentTime }
			}
			callerVerifierRan := false
			edge.dialer.TLSClientConfig.VerifyConnection = func(tls.ConnectionState) error {
				callerVerifierRan = true
				return nil
			}
			client, err := DialClient(t.Context(), ClientOptions{
				URL: "wss://" + distributionHost + BridgePath, Ticket: "invalid-certificate-ticket",
				WebSocketDialer: edge.dialer, CloudFrontNoSNI: true, PingInterval: -1,
			})
			if client != nil {
				_ = client.Close()
				t.Fatal("DialClient returned a client for an invalid certificate")
			}
			if err == nil {
				t.Fatal("DialClient accepted an invalid certificate")
			}
			if callerVerifierRan {
				t.Fatal("caller verification hook ran before mandatory certificate verification")
			}
			if sni := receiveTLSObservation(t, "ClientHello", edge.sni); sni != "" {
				t.Fatalf("ClientHello SNI = %q, want empty", sni)
			}
			select {
			case host := <-edge.host:
				t.Fatalf("HTTP request with Host %q was sent before certificate verification", host)
			default:
			}
		})
	}
}

func TestDialClientNonCloudFrontRetainsSNI(t *testing.T) {
	const frontHost = "cdn.example.com"
	edge := newObservedTLSEdge(t, frontHost)
	client, err := DialClient(t.Context(), ClientOptions{
		URL: "wss://" + frontHost + BridgePath, Ticket: "cdn-ticket",
		WebSocketDialer: edge.dialer, CloudFrontNoSNI: true, PingInterval: -1,
	})
	if err != nil {
		t.Fatalf("DialClient: %v", err)
	}
	defer client.Close()
	if sni := receiveTLSObservation(t, "ClientHello", edge.sni); sni != frontHost {
		t.Fatalf("ClientHello SNI = %q, want %q", sni, frontHost)
	}
	if host := receiveTLSObservation(t, "HTTP Host", edge.host); host != frontHost {
		t.Fatalf("HTTP Host = %q, want %q", host, frontHost)
	}
}
