package wsscore

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"
)

// nativeFrontZones lists the CDN parent zones whose edges answer a ClientHello
// carrying no SNI with a default certificate covering every one-label name
// beneath them. Both entries were confirmed to serve exactly that certificate
// without SNI: CloudFront's *.cloudfront.net and bunny.net's *.b-cdn.net.
//
// A zone belongs here only if its default no-SNI certificate authenticates the
// same host the signed front URL names. Adding one whose edges instead answer
// with an unrelated certificate would silently disable the hostname check that
// makes dropping SNI safe.
var nativeFrontZones = []string{
	"cloudfront.net",
	"b-cdn.net",
}

// nativeFrontHost recognizes only the one-label CDN hostnames covered by their
// zone's default certificate. Custom CNAMEs continue through the ordinary
// SNI-bearing TLS path, because a default certificate cannot authenticate them.
func nativeFrontHost(rawURL string) (string, bool) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", false
	}
	host := parsed.Hostname()
	for _, zone := range nativeFrontZones {
		label, found := strings.CutSuffix(host, "."+zone)
		if !found || label == "" || strings.Contains(label, ".") {
			continue
		}
		return host, true
	}
	return "", false
}

func noSNITLSDialContext(
	networkDial func(context.Context, string, string) (net.Conn, error),
	baseConfig *tls.Config,
	verificationName string,
	phases *dialPhases,
) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		plainConn, err := networkDial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		tlsConn := tls.Client(plainConn, verifiedNoSNIConfig(baseConfig, verificationName))
		// Gorilla's httptrace TLS hooks never fire on this path (it treats the
		// returned connection as already-TLS), so the phase booleans that let
		// the classifier split TCP from TLS failures are marked here. Errors
		// still propagate raw internally; classification stays central.
		phases.tlsStarted.Store(true)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = plainConn.Close()
			return nil, err
		}
		phases.tlsDone.Store(true)
		return tlsConn, nil
	}
}

// verifiedNoSNIConfig separates the certificate verification name from the
// ClientHello server name. crypto/tls otherwise uses Config.ServerName for
// both, so suppressing SNI requires replacing its built-in verification with
// the equivalent x509 check against the signed front URL host.
func verifiedNoSNIConfig(base *tls.Config, verificationName string) *tls.Config {
	config := base.Clone()
	peerVerifier := config.VerifyPeerCertificate
	connectionVerifier := config.VerifyConnection
	roots := config.RootCAs
	currentTime := config.Time

	config.ServerName = ""
	// InsecureSkipVerify only disables crypto/tls's inseparable SNI/hostname
	// path here. The VerifyConnection hook below always performs chain and
	// hostname verification before invoking caller-supplied hooks.
	config.InsecureSkipVerify = true //nolint:gosec
	config.VerifyPeerCertificate = nil
	config.EncryptedClientHelloConfigList = nil
	config.EncryptedClientHelloRejectionVerify = nil
	config.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return errors.New("TLS server did not provide a certificate")
		}
		intermediates := x509.NewCertPool()
		for _, certificate := range state.PeerCertificates[1:] {
			intermediates.AddCert(certificate)
		}
		now := time.Now()
		if currentTime != nil {
			now = currentTime()
		}
		chains, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{
			DNSName:       verificationName,
			Roots:         roots,
			Intermediates: intermediates,
			CurrentTime:   now,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		})
		if err != nil {
			return &tls.CertificateVerificationError{
				UnverifiedCertificates: state.PeerCertificates,
				Err:                    err,
			}
		}
		state.VerifiedChains = chains

		// Match crypto/tls callback ordering and resumption behavior. The older
		// callback is skipped on resumed sessions; VerifyConnection always runs.
		if peerVerifier != nil && !state.DidResume {
			rawCertificates := make([][]byte, len(state.PeerCertificates))
			for index, certificate := range state.PeerCertificates {
				rawCertificates[index] = certificate.Raw
			}
			if err := peerVerifier(rawCertificates, chains); err != nil {
				return err
			}
		}
		if connectionVerifier != nil {
			return connectionVerifier(state)
		}
		return nil
	}
	return config
}
