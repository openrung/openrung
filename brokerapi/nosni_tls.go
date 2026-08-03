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

// noSNIVerification states what a certificate presented on a suppressed-SNI
// handshake has to prove. Suppressing SNI costs crypto/tls's built-in check
// (Config.ServerName drives both the ClientHello name and verification), so the
// replacement has to say explicitly what identity is being asserted — the two
// fronts assert different ones, and the difference is a security property, not
// an implementation detail.
//
// Exactly one field is set. An empty value is rejected at verification time
// rather than silently accepting anything the roots ever signed.
type noSNIVerification struct {
	// hostname binds the connection to one name, checked by x509 exactly as an
	// ordinary TLS dial would. This is the strong form: the peer must hold a
	// certificate for the host the request is addressed to. CloudFront allows it
	// because its no-SNI default certificate (*.cloudfront.net) covers the
	// distribution hostname being dialed.
	hostname string

	// requiredSAN binds the connection only to the operator of a shared edge:
	// the leaf must carry this exact DNS SAN, and the chain is then verified
	// without a hostname. This is the weaker form, for a front whose no-SNI
	// certificate cannot cover its own endpoint name — see azure_tls.go for why
	// Azure Front Door needs it and what the connection no longer proves.
	requiredSAN string
}

// verify performs the chain and identity checks crypto/tls would have done,
// and returns the verified chains so caller-supplied hooks still receive them.
func (v noSNIVerification) verify(
	peers []*x509.Certificate,
	roots *x509.CertPool,
	now time.Time,
) ([][]*x509.Certificate, error) {
	// x509 skips hostname verification entirely for an empty DNSName, which
	// would accept any certificate the trusted roots ever signed. A rule that
	// asserts nothing is a bug, not a permissive default: fail the handshake
	// rather than leave that outcome one refactor away.
	switch {
	case v.hostname != "" && v.requiredSAN != "":
		return nil, errors.New("no-SNI TLS accepts one verification rule, got both a hostname and a required SAN")
	case v.hostname == "" && v.requiredSAN == "":
		return nil, errors.New("no-SNI TLS requires a certificate verification rule")
	}
	if len(peers) == 0 {
		return nil, errors.New("TLS server did not provide a certificate")
	}

	// The SAN assertion is what supplies name binding when the chain below is
	// built without a DNSName, so it has to happen first and unconditionally.
	if v.requiredSAN != "" && !hasExactDNSName(peers[0], v.requiredSAN) {
		return nil, &tls.CertificateVerificationError{
			UnverifiedCertificates: peers,
			Err: errors.New(
				"leaf certificate does not carry the required SAN " + v.requiredSAN +
					" (got " + strings.Join(peers[0].DNSNames, ", ") + ")",
			),
		}
	}

	intermediates := x509.NewCertPool()
	for _, certificate := range peers[1:] {
		intermediates.AddCert(certificate)
	}
	chains, err := peers[0].Verify(x509.VerifyOptions{
		DNSName:       v.hostname,
		Roots:         roots,
		Intermediates: intermediates,
		CurrentTime:   now,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		return nil, &tls.CertificateVerificationError{
			UnverifiedCertificates: peers,
			Err:                    err,
		}
	}
	return chains, nil
}

// hasExactDNSName compares SAN entries literally. It deliberately performs no
// wildcard expansion: the caller pins the wildcard string the edge actually
// serves, so "does the leaf carry this SAN" is a string question, and running
// it through hostname matching would only add a way to get it wrong.
func hasExactDNSName(leaf *x509.Certificate, name string) bool {
	for _, candidate := range leaf.DNSNames {
		if strings.EqualFold(candidate, name) {
			return true
		}
	}
	return false
}

// NoSNIFront reports whether direct connections to brokerURL suppress the
// ClientHello server name, and describes what the certificate is then required
// to prove. It is the single source of truth for that question: a front
// acceptance check (cmd/frontcheck) must not restate the name shapes that
// dialTLSContext matches, or the two could disagree about what ships.
//
// A proxied connection is not covered. net/http performs the tunnelled
// handshake itself, so DialTLSContext never runs and the name is sent.
func NoSNIFront(brokerURL string) (rule string, suppressed bool) {
	parsed, err := EnforceSecureBrokerURL(brokerURL)
	if err != nil {
		return "", false
	}
	port := parsed.Port()
	if port == "" {
		port = "443"
	}
	address := net.JoinHostPort(parsed.Hostname(), port)
	if distribution, native := cloudFrontDistributionAddress(address); native {
		return "CloudFront: certificate must be valid for " + distribution, true
	}
	if azureFrontDoorAddress(address) {
		return "Azure Front Door: certificate must carry the SAN " + azureEdgeNoSNICertificateSAN +
			" (the endpoint name cannot be verified; see azure_tls.go)", true
	}
	return "", false
}

// dialNoSNITLS completes TLS without a ClientHello server name, so the front's
// hostname reaches the CDN only inside the encrypted HTTP Host header. There is
// deliberately no SNI-bearing retry: a fallback would leak the exact name this
// hides, and discovery already races independent fronts when one stops
// answering.
func (d *echDialer) dialNoSNITLS(
	ctx context.Context,
	network, address string,
	verification noSNIVerification,
) (net.Conn, error) {
	// Before the socket, so a policy this path cannot honor costs no
	// connection. Discovery still reaches the other fronts.
	if fips140.Enabled() {
		return nil, errNoSNIInFIPSMode
	}
	plainConn, err := d.networkDial(ctx, network, address)
	if err != nil {
		return nil, err
	}
	config := verifiedNoSNIConfig(d.baseTLSConfig, verification)
	return handshake(ctx, plainConn, config, d.tlsHandshakeLimit)
}

// verifiedNoSNIConfig separates the certificate verification rule from the
// ClientHello server name. crypto/tls otherwise uses Config.ServerName for
// both, so suppressing SNI requires replacing its built-in verification with an
// equivalent x509 check.
//
// The returned config keeps the transport's own TLS policy — roots, versions,
// ALPN, clock — and differs from an ordinary broker dial only in where identity
// is asserted. crypto/tls fills ConnectionState.VerifiedChains only on the path
// this replaces, so it stays empty here; the chains this builds reach
// caller-supplied hooks instead. The one stock step it cannot reproduce is FIPS
// chain filtering, which is why dialNoSNITLS refuses to run at all in that mode.
func verifiedNoSNIConfig(base *tls.Config, verification noSNIVerification) *tls.Config {
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
	// identity verification before invoking caller-supplied hooks.
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
	// the peer address, so a ticket minted by one front would be offered to any
	// other reachable through the same edge address — a cross-front correlator
	// on the wire. Broker requests are far too sparse for resumption to be worth
	// that.
	config.ClientSessionCache = nil
	config.SessionTicketsDisabled = true
	config.VerifyConnection = func(state tls.ConnectionState) error {
		now := time.Now()
		if currentTime != nil {
			now = currentTime()
		}
		chains, err := verification.verify(state.PeerCertificates, roots, now)
		if err != nil {
			return err
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
