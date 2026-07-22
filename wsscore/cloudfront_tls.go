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

// cloudFrontDistributionHost recognizes only the one-label distribution names
// covered by CloudFront's default *.cloudfront.net certificate. Custom CNAMEs
// continue through the ordinary SNI-bearing TLS path.
func cloudFrontDistributionHost(rawURL string) (string, bool) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", false
	}
	host := parsed.Hostname()
	labels := strings.Split(host, ".")
	if len(labels) != 3 || labels[0] == "" || labels[1] != "cloudfront" || labels[2] != "net" {
		return "", false
	}
	return host, true
}

func noSNITLSDialContext(
	networkDial func(context.Context, string, string) (net.Conn, error),
	baseConfig *tls.Config,
	verificationName string,
) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		plainConn, err := networkDial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		tlsConn := tls.Client(plainConn, verifiedNoSNIConfig(baseConfig, verificationName))
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = plainConn.Close()
			return nil, err
		}
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
