package punch

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/openrung/openrung/punchcore"
)

// quicConfig is shared by both ends. KeepAlivePeriod mirrors the yamux hub tunnel
// keepalive (internal/tunnel/tunnel.go). InitialPacketSize is clamped for the
// nested-TUN path (a punched flow carries a VLESS/Reality TCP stream that itself
// carries the client's TUN traffic).
func quicConfig() *quic.Config {
	return &quic.Config{
		MaxIdleTimeout:     30 * time.Second,
		KeepAlivePeriod:    15 * time.Second,
		InitialPacketSize:  1200,
		MaxIncomingStreams: 1024,
	}
}

// GenerateSessionCert creates a fresh self-signed ECDSA certificate for one punch
// session and returns it with the SHA-256 fingerprint of its leaf DER. The
// fingerprint is relayed to the client over the authenticated hub channel and
// pinned there; QUIC's TLS is a transport/pinning layer only — Reality remains
// the end-to-end security boundary.
func GenerateSessionCert() (tls.Certificate, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("generate punch cert key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("generate punch cert serial: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "openrung-punch"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("create punch cert: %w", err)
	}
	sum := sha256.Sum256(der)
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	return cert, hex.EncodeToString(sum[:]), nil
}

func serverTLSConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{punchcore.ALPN},
	}
}

func clientTLSConfig(fingerprint string) *tls.Config {
	want := fingerprint
	return &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // QUIC cert is pinned by fingerprint below; Reality is the real E2E layer.
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{punchcore.ALPN},
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("punch peer presented no certificate")
			}
			sum := sha256.Sum256(rawCerts[0])
			if hex.EncodeToString(sum[:]) != want {
				return errors.New("punch peer certificate fingerprint mismatch")
			}
			return nil
		},
	}
}

// ListenQUIC starts a QUIC listener on the (unconnected) punched socket. The
// volunteer accepts one connection from the client, then bridges its streams.
func ListenQUIC(sock net.PacketConn, cert tls.Certificate) (*quic.Listener, error) {
	return quic.Listen(sock, serverTLSConfig(cert), quicConfig())
}

// DialQUIC dials the peer over the (unconnected) punched socket, pinning the
// peer's certificate to fingerprint.
func DialQUIC(ctx context.Context, sock net.PacketConn, peer net.Addr, fingerprint string) (*quic.Conn, error) {
	return quic.Dial(ctx, sock, peer, clientTLSConfig(fingerprint), quicConfig())
}

// quicStreamConn adapts a *quic.Stream to net.Conn. quic.Stream already provides
// Read/Write/Close/deadlines; only the addresses are supplied here so the shared
// pipe helper can treat it like any other connection.
type quicStreamConn struct {
	*quic.Stream
	local  net.Addr
	remote net.Addr
}

func (c *quicStreamConn) LocalAddr() net.Addr  { return c.local }
func (c *quicStreamConn) RemoteAddr() net.Addr { return c.remote }
