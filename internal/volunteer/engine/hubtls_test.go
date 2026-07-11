package engine

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

// selfSignedHub returns a TLS listener presenting a fresh self-signed cert
// (bare-IP style, like a real relay hub) and that cert's SHA-256 fingerprint.
func selfSignedHub(t *testing.T) (net.Listener, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	sum := sha256.Sum256(der)
	fp := hex.EncodeToString(sum[:])

	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Complete the handshake, then close.
			go func(c net.Conn) {
				_ = c.(*tls.Conn).Handshake()
				_ = c.Close()
			}(conn)
		}
	}()
	return ln, fp
}

func dialWith(cfg Config) error {
	tc := hubTLSConfig(cfg)
	conn, err := tls.Dial("tcp", cfg.HubAddr, tc)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

func TestHubCertPinAcceptsMatchingSelfSignedCert(t *testing.T) {
	ln, fp := selfSignedHub(t)
	defer ln.Close()
	addr := ln.Addr().String()

	// Correct pin connects despite the cert being self-signed (no CA).
	if err := dialWith(Config{HubAddr: addr, HubCertFingerprint: fp}); err != nil {
		t.Fatalf("matching pin should connect: %v", err)
	}

	// Colon/upper-case formatting (openssl style) must be accepted too.
	styled := strings.ToUpper(colonize(fp))
	if err := dialWith(Config{HubAddr: addr, HubCertFingerprint: styled}); err != nil {
		t.Fatalf("openssl-styled pin should connect: %v", err)
	}
}

func TestHubCertPinRejectsWrongFingerprint(t *testing.T) {
	ln, _ := selfSignedHub(t)
	defer ln.Close()
	addr := ln.Addr().String()

	wrong := strings.Repeat("00", 32)
	err := dialWith(Config{HubAddr: addr, HubCertFingerprint: wrong})
	if err == nil {
		t.Fatal("a mismatched pin must fail the handshake")
	}
	if !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("expected a fingerprint-mismatch error, got: %v", err)
	}
}

func TestHubCertPinRejectsUnpinnedSelfSignedCert(t *testing.T) {
	ln, _ := selfSignedHub(t)
	defer ln.Close()
	addr := ln.Addr().String()

	// No pin and no insecure flag: standard verification must reject the
	// self-signed cert (this is why the pin is needed at all).
	err := dialWith(Config{HubAddr: addr})
	if err == nil {
		t.Fatal("unpinned standard verification must reject a self-signed hub cert")
	}
}

func colonize(hexstr string) string {
	var b strings.Builder
	for i := 0; i < len(hexstr); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(hexstr[i : i+2])
	}
	return b.String()
}
