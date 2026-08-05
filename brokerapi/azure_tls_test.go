// SPDX-License-Identifier: GPL-3.0-or-later

package brokerapi

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// testNow is the clock the verification helpers are called with directly, where
// no tls.Config supplies one. The test authority issues certificates valid for
// an hour either side of the current time.
func testNow() time.Time { return time.Now() }

const (
	// A Front Door Standard/Premium endpoint of the shape Azure issues today,
	// and a bare-zone variant. Neither is this deployment's endpoint: the rule
	// is the name shape, so a fork's endpoint is concealed too.
	testAzureEndpoint     = "openrung-broker-a1b2c3d4e5f6g7h8.z01.azurefd.net"
	testAzureBareEndpoint = "openrung-broker.azurefd.net"
)

func TestAzureFrontDoorAddress(t *testing.T) {
	for _, test := range []struct {
		address string
		ok      bool
	}{
		{address: testAzureEndpoint + ":443", ok: true},
		{address: testAzureBareEndpoint + ":443", ok: true},
		{address: "endpoint.b02.azurefd.net:443", ok: true},
		{address: "endpoint.a03.azurefd.net:443", ok: true},
		// Case and a trailing dot survive no canonicalization of their own on a
		// dial address, so the recognizer has to normalize both itself.
		{address: "ENDPOINT.Z01.AZUREFD.NET:443", ok: true},
		{address: testAzureEndpoint + ".:443", ok: true},

		// A zone label is a letter and two digits; anything else is a
		// third-party subdomain, not an Azure zone.
		{address: "endpoint.zone.azurefd.net:443"},
		{address: "endpoint.z1.azurefd.net:443"},
		{address: "endpoint.z001.azurefd.net:443"},
		{address: "endpoint.0z1.azurefd.net:443"},
		{address: "deep.endpoint.z01.azurefd.net:443"},
		{address: ".z01.azurefd.net:443"},
		{address: "azurefd.net:443"},
		{address: ".azurefd.net:443"},
		{address: testAzureEndpoint + ".example:443"},

		// Only the standard HTTPS port, and never another front's name.
		{address: testAzureEndpoint + ":8443"},
		{address: cloudFrontBrokerHost + ":443"},
		{address: cloudflareBrokerHost + ":443"},
		{address: "endpoint.z01.azureedge.net:443"},
		{address: "127.0.0.1:443"},
		{address: "not-an-address"},
	} {
		if got := azureFrontDoorAddress(test.address); got != test.ok {
			t.Errorf("azureFrontDoorAddress(%q) = %v, want %v", test.address, got, test.ok)
		}
	}
}

func TestEndpointUnboundBrokerFront(t *testing.T) {
	tests := []struct {
		brokerURL string
		want      bool
	}{
		{brokerURL: AzureBrokerURL, want: true},
		{brokerURL: "https://" + testAzureBareEndpoint + "/prefix", want: true},
		{brokerURL: "HTTPS://" + strings.ToUpper(testAzureEndpoint) + ".:443/", want: true},
		{brokerURL: "https://" + testAzureEndpoint + ":8443/"},
		{brokerURL: "http://" + testAzureEndpoint + "/"},
		{brokerURL: "https://broker.example/"},
		{brokerURL: CloudFrontBrokerURL},
		{brokerURL: "https://user:password@" + testAzureEndpoint + "/"},
		{brokerURL: "not a broker URL"},
	}
	for _, test := range tests {
		if got := EndpointUnboundBrokerFront(test.brokerURL); got != test.want {
			t.Errorf("EndpointUnboundBrokerFront(%q) = %v, want %v", test.brokerURL, got, test.want)
		}
	}
}

func TestBrokerAzureDialOmitsSNI(t *testing.T) {
	for _, endpoint := range []string{testAzureEndpoint, testAzureBareEndpoint} {
		t.Run(endpoint, func(t *testing.T) {
			certificate, roots := testCloudFrontChain(t, azureEdgeNoSNICertificateSAN)
			networkDial, results, calls := testTLSPipeDialer(testBrokerServerConfig(certificate))
			dialer := testBrokerECHDialer(networkDial, roots, newECHConfigState(embeddedCloudflareECHConfigList))

			conn, err := dialer.dialTLSContext(t.Context(), "tcp", endpoint+":443")
			if err != nil {
				t.Fatalf("no-SNI Azure dial: %v", err)
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
					"ClientHello server name = %q, want the endpoint name to stay off the wire",
					result.state.ServerName,
				)
			}
		})
	}
}

// The Azure front pins the shared edge SAN, so it must accept the certificate
// Azure actually serves without SNI and reject one that merely chains to a
// trusted root. This is the whole security value of the weaker rule: an
// impersonator needs a publicly-trusted certificate for a Microsoft name, not
// just for a domain they happen to own.
func TestBrokerAzureDialRequiresTheSharedEdgeSAN(t *testing.T) {
	for _, test := range []struct {
		name             string
		certificateNames []string
		accepted         bool
	}{
		{
			name:             "the certificate Azure serves without SNI",
			certificateNames: []string{azureEdgeNoSNICertificateSAN},
			accepted:         true,
		},
		{
			// The observed leaf carries this SAN alone, but extra names must not
			// disqualify it — Azure is free to add some.
			name:             "shared edge SAN among others",
			certificateNames: []string{"*.azurefd.net", azureEdgeNoSNICertificateSAN},
			accepted:         true,
		},
		{
			// What the endpoint is actually named. Azure never serves this
			// without SNI, and accepting it would mean the pin does nothing.
			name:             "the endpoint's own name",
			certificateNames: []string{"*.azurefd.net", "*.z01.azurefd.net"},
		},
		{
			name:             "another domain entirely, same trusted root",
			certificateNames: []string{"*.example.net"},
		},
		{
			// A wildcard one level up does not cover *.azureedge.net, and the
			// comparison is a literal SAN match, so it must not be expanded.
			name:             "a wildcard that would match under hostname rules",
			certificateNames: []string{"*.net"},
		},
		{
			name:             "the parent name without the wildcard",
			certificateNames: []string{"azureedge.net"},
		},
		{
			name:             "a lookalike registered elsewhere",
			certificateNames: []string{"*.azureedge.net.example.com"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			certificate, roots := testCloudFrontChain(t, test.certificateNames...)
			networkDial, _, _ := testTLSSocketDialer(t, testBrokerServerConfig(certificate))
			dialer := testBrokerECHDialer(networkDial, roots, newECHConfigState(nil))

			conn, err := dialer.dialTLSContext(t.Context(), "tcp", testAzureEndpoint+":443")
			if err == nil {
				closeTLSConn(conn)
			}
			if test.accepted {
				if err != nil {
					t.Fatalf("certificate %v was rejected: %v", test.certificateNames, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("certificate %v was accepted for the Azure front", test.certificateNames)
			}
			var verificationErr *tls.CertificateVerificationError
			if !errors.As(err, &verificationErr) {
				t.Fatalf("rejection = %v, want a certificate verification error", err)
			}
		})
	}
}

// Case folding matters because the pin is compared against SAN strings rather
// than run through hostname matching.
func TestAzureVerificationMatchesSANCaseInsensitively(t *testing.T) {
	authority := newTestAuthority(t)
	chain := testPeerCertificates(t, authority.issue(t, strings.ToUpper(azureEdgeNoSNICertificateSAN)))
	if _, err := azureFrontDoorVerification().verify(chain, authority.roots, testNow()); err != nil {
		t.Fatalf("upper-case SAN was rejected: %v", err)
	}
}

// An expired or untrusted chain must still fail even though the pinned SAN is
// present: the SAN assertion supplies the name binding, not the chain check it
// stands alongside.
func TestAzureVerificationStillRequiresATrustedChain(t *testing.T) {
	authority := newTestAuthority(t)
	chain := testPeerCertificates(t, authority.issue(t, azureEdgeNoSNICertificateSAN))

	if _, err := azureFrontDoorVerification().verify(chain, x509.NewCertPool(), testNow()); err == nil {
		t.Fatal("a certificate with the pinned SAN verified against an empty root pool")
	}
	expired := testNow().AddDate(1, 0, 0)
	if _, err := azureFrontDoorVerification().verify(chain, authority.roots, expired); err == nil {
		t.Fatal("an expired certificate with the pinned SAN verified")
	}
}

// The two fronts assert different identities, and neither rule may be usable
// for the other's certificate. A mix-up in either direction would silently
// weaken one front.
func TestFrontVerificationRulesDoNotCrossApply(t *testing.T) {
	authority := newTestAuthority(t)

	azureLeaf := testPeerCertificates(t, authority.issue(t, azureEdgeNoSNICertificateSAN))
	if _, err := cloudFrontVerification(testCloudFrontHost).verify(azureLeaf, authority.roots, testNow()); err == nil {
		t.Fatal("the Azure edge certificate satisfied the CloudFront hostname rule")
	}

	cloudFrontLeaf := testPeerCertificates(t, authority.issue(t, testCloudFrontWildcard))
	if _, err := azureFrontDoorVerification().verify(cloudFrontLeaf, authority.roots, testNow()); err == nil {
		t.Fatal("the CloudFront certificate satisfied the Azure shared-edge pin")
	}
}

// The advertised Azure front must actually take the no-SNI path, and must not
// collide with either other front's mechanism. The endpoint that ships was
// accepted by cmd/frontcheck on 2026-08-05.
func TestBrokerAzureConstantsStayLinked(t *testing.T) {
	parsed, err := EnforceSecureBrokerURL(AzureBrokerURL)
	if err != nil {
		t.Fatalf("parse %s: %v", AzureBrokerURL, err)
	}
	if parsed.Hostname() != azureBrokerHost {
		t.Fatalf("AzureBrokerURL host = %q, want %q", parsed.Hostname(), azureBrokerHost)
	}
	address := net.JoinHostPort(parsed.Hostname(), "443")
	if !azureFrontDoorAddress(address) {
		t.Fatalf("the advertised Azure front %q is not recognized, so it would be dialed WITH SNI", address)
	}
	// Each built-in front conceals its hostname by a different mechanism, and
	// no front may end up on two paths at once.
	if isCloudflareBrokerAddress(address) {
		t.Fatal("the Azure front also matches the ECH front")
	}
	if _, ok := cloudFrontDistributionAddress(address); ok {
		t.Fatal("the Azure front also matches the CloudFront path")
	}
	if azureFrontDoorAddress(net.JoinHostPort(cloudFrontBrokerHost, "443")) {
		t.Fatal("the CloudFront front also matches the Azure path")
	}
	if azureFrontDoorAddress(net.JoinHostPort(cloudflareBrokerHost, "443")) {
		t.Fatal("the Cloudflare front also matches the Azure path")
	}
}

// Keep the stable built-in preference order even though FirstReachable also
// enforces the stronger boundary by putting endpoint-unbound fronts in a
// separate phase.
func TestAzureFrontRemainsLastInDefaultOrder(t *testing.T) {
	defaults := DefaultBrokerURLs()
	if len(defaults) == 0 || defaults[len(defaults)-1] != AzureBrokerURL {
		t.Fatalf("built-in front order = %v, want the Azure front last", defaults)
	}
	for _, candidate := range defaults[:len(defaults)-1] {
		if EndpointUnboundBrokerFront(candidate) {
			t.Fatalf("an endpoint-unbound front appears before the end of the order: %q", candidate)
		}
	}
}
