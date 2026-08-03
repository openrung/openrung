// SPDX-License-Identifier: GPL-3.0-or-later

package brokerapi

import (
	"context"
	"crypto/fips140"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"time"
)

// errNoSNIInFIPSMode refuses the no-SNI path rather than run it under a weaker
// certificate policy than the rest of the process. crypto/tls filters verified
// chains through a FIPS 140-3 policy that has no exported equivalent, so the
// hook that replaces its verification cannot enforce it.
//
// This covers the GODEBUG=fips140 setting, which is what turns that filtering
// on. A Go+BoringCrypto build that imports crypto/tls/fipsonly forces the same
// requirement through an internal call that nothing outside crypto/tls can
// observe; this repository does not produce such builds.
var errNoSNIInFIPSMode = errors.New("no-SNI TLS is unavailable in FIPS 140-3 mode")

// cloudFrontDistributionAddress recognizes only the one-label distribution
// names covered by CloudFront's default *.cloudfront.net certificate, on the
// standard HTTPS port. A custom CNAME must keep sending SNI: without it
// CloudFront answers with that same default certificate, which cannot be
// verified against the CNAME the client asked for.
//
// This is the address-shaped counterpart of the URL-shaped check the WSS data
// path applies in wsscore/cloudfront_tls.go. Keep the two in step on what they
// recognize; the port gate and case/trailing-dot normalization are extra here
// because a dial address has been through no canonicalization of its own.
func cloudFrontDistributionAddress(address string) (string, bool) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port != "443" {
		return "", false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	labels := strings.Split(host, ".")
	if len(labels) != 3 || labels[0] == "" || labels[1] != "cloudfront" || labels[2] != "net" {
		return "", false
	}
	return host, true
}

// dialNoSNITLS completes TLS without a ClientHello server name, so the
// distribution name reaches CloudFront only inside the encrypted HTTP Host
// header. There is deliberately no SNI-bearing retry: a fallback would leak
// the exact name this hides, and discovery already races an independent front
// when one stops answering.
func (d *echDialer) dialNoSNITLS(ctx context.Context, network, address, verificationName string) (net.Conn, error) {
	// Before the socket, so a policy this path cannot honor costs no
	// connection. Discovery still reaches the other front.
	if fips140.Enabled() {
		return nil, errNoSNIInFIPSMode
	}
	plainConn, err := d.networkDial(ctx, network, address)
	if err != nil {
		return nil, err
	}
	config := verifiedNoSNIConfig(d.baseTLSConfig, verificationName)
	return handshake(ctx, plainConn, config, d.tlsHandshakeLimit)
}

// verifiedNoSNIConfig separates the certificate verification name from the
// ClientHello server name. crypto/tls otherwise uses Config.ServerName for
// both, so suppressing SNI requires replacing its built-in verification with
// the equivalent x509 check against the host the request was addressed to.
//
// The returned config keeps the transport's own TLS policy — roots, versions,
// ALPN, clock — and differs from an ordinary broker dial only in where the
// hostname is asserted. crypto/tls fills ConnectionState.VerifiedChains only
// on the path this replaces, so it stays empty here; the chains this builds
// reach caller-supplied hooks instead. The one stock step it cannot reproduce
// is FIPS chain filtering, which is why dialNoSNITLS refuses to run at all in
// that mode.
func verifiedNoSNIConfig(base *tls.Config, verificationName string) *tls.Config {
	config := &tls.Config{}
	if base != nil {
		config = base.Clone()
	}
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
	// Both hooks below are skipped whenever an ECH config is set and the server
	// rejects it. crypto/tls then either runs
	// EncryptedClientHelloRejectionVerify alone — no chain building and no
	// hostname check — or, with no such hook, verifies against the ECH public
	// name rather than the host actually being reached. Neither outcome
	// authenticates this connection, so ECH is never combined with a suppressed
	// SNI; the Cloudflare front carries it on the ordinary path.
	config.EncryptedClientHelloConfigList = nil
	config.EncryptedClientHelloRejectionVerify = nil
	// An empty server name degrades crypto/tls's client session cache key to
	// the peer address, so a ticket minted by one distribution would be offered
	// to any other reachable through the same CloudFront edge address — a
	// cross-front correlator on the wire. Broker requests are far too sparse
	// for resumption to be worth that.
	config.ClientSessionCache = nil
	config.SessionTicketsDisabled = true
	config.VerifyConnection = func(state tls.ConnectionState) error {
		// x509 skips hostname verification entirely for an empty DNSName, which
		// would accept any certificate the trusted roots ever signed. Nothing
		// reaches this without a name today; fail the handshake rather than
		// leave that outcome one refactor away.
		if verificationName == "" {
			return errors.New("no-SNI TLS requires a certificate verification name")
		}
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
