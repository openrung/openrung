// SPDX-License-Identifier: GPL-3.0-or-later

package brokerapi

import (
	"net"
	"strings"
)

// azureEdgeNoSNICertificateSAN is the only DNS SAN on the default certificate
// the shared Azure edge serves when the ClientHello carries no server name.
//
// Measured 2026-08-03 against two independent Front Door edge partitions
// (p-0010 via 150.171.84.20/.30, t-0009 via 13.107.226.39), each reached with a
// third party's endpoint in the Host header:
//
//	no SNI  -> leaf CN/SAN [*.azureedge.net], issuer "Microsoft TLS G2 ECC CA
//	           OCSP 06", chains to a public root, TLS 1.3
//	with SNI-> leaf CN *.azurefd.net, SANs [*.azurefd.net *.z01..z10, a01..a03,
//	           b01, b02.azurefd.net], a different issuer
//
// Both paths returned byte-identical responses for the same Host, and an
// unknown Host returned Front Door's 404 rather than the endpoint's content, so
// routing is genuinely driven by the encrypted Host header and is unaffected by
// omitting SNI. Microsoft blocked classic domain fronting (mismatched SNI vs
// Host) years ago; the no-SNI case is distinct because there is no SNI to
// mismatch, which is also why it survives on CloudFront. It is undocumented and
// carries no SLA.
//
// The certificate is a property of the edge fleet, not of Front Door as a
// product: a Microsoft edge outside that fleet served an entirely different
// default certificate. So this SAN is only known to hold for endpoints that
// land on the Front Door fleet, which is why a new endpoint has to be checked
// before it is advertised — see cmd/frontcheck.
const azureEdgeNoSNICertificateSAN = "*.azureedge.net"

// azureFrontDoorAddress recognizes Front Door Standard/Premium endpoint names
// on the standard HTTPS port: the bare "<endpoint>.azurefd.net" and the zoned
// "<endpoint>.<zone>.azurefd.net" that Azure actually issues today, where the
// zone is a letter followed by two digits (z01-z10, a01-a03, b01-b02 at the
// time of writing, matched by shape so a new zone needs no code change).
//
// Like the CloudFront recognizer this matches the name shape rather than this
// deployment's own endpoint, so rotating the endpoint — or a fork running its
// own — cannot silently put the name back on the wire as cleartext SNI.
//
// A custom domain pointed at Front Door is deliberately excluded. It gains
// nothing: without SNI the edge serves the same shared default certificate
// regardless of the Host, so a custom domain would be no better authenticated
// while losing the ordinary-TLS verification it does get by keeping SNI.
func azureFrontDoorAddress(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port != "443" {
		return false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	labels := strings.Split(host, ".")
	switch len(labels) {
	case 3:
		return labels[0] != "" && labels[1] == "azurefd" && labels[2] == "net"
	case 4:
		return labels[0] != "" && isAzureFrontDoorZone(labels[1]) &&
			labels[2] == "azurefd" && labels[3] == "net"
	default:
		return false
	}
}

func isAzureFrontDoorZone(label string) bool {
	if len(label) != 3 {
		return false
	}
	if label[0] < 'a' || label[0] > 'z' {
		return false
	}
	return label[1] >= '0' && label[1] <= '9' && label[2] >= '0' && label[2] <= '9'
}

// azureFrontDoorVerification pins the shared Azure edge certificate instead of
// the endpoint hostname, because Azure gives no way to do the latter.
//
// THE TRADEOFF. Without SNI the edge serves *.azureedge.net, which does not
// cover the *.azurefd.net endpoint being dialed, so there is no hostname to
// verify against and no configuration that changes this: a custom domain still
// gets the shared certificate on the no-SNI path, and the *.azureedge.net
// endpoint name that would match belongs to a retiring product. The three real
// options were to pin this SAN, to pin the leaf public key, or to check the
// chain and no name at all.
//
// Leaf pinning is rejected: the observed certificate expires within roughly a
// quarter, and a pin baked into shipped desktop and mobile builds would break
// this front on every rotation. Chain-only is rejected as strictly worse than
// pinning the SAN for no benefit — it would accept any certificate any public
// root ever signed, which an adversary can buy for a domain they own.
//
// Pinning the SAN means an impersonator must present a publicly-trusted
// certificate for a name under azureedge.net, i.e. be Microsoft or hold a
// mis-issuance for a Microsoft domain. What the connection then proves is "this
// is an Azure edge", not "this is our endpoint". That is a genuine downgrade
// from the other two fronts, and it is survivable only because TLS is not where
// this client's trust actually rests:
//
//   - The relay list is Ed25519-signed by the broker origin and verified
//     against keys pinned in this module (signing.go), so a front that is not
//     ours cannot forge one. The signed envelope also binds channel, echoed
//     limit, and a not_after freshness bound, so it cannot replay an expired
//     list either.
//   - What an impersonator would still get is the request side and denial of
//     service: the client identity headers, telemetry payloads, and the ability
//     to serve nothing. That is the cost being accepted here, and it is the
//     reason this front belongs last in the discovery order rather than first.
//
// If Microsoft changes the default certificate, this fails closed and front
// racing covers reachability.
func azureFrontDoorVerification() noSNIVerification {
	return noSNIVerification{requiredSAN: azureEdgeNoSNICertificateSAN}
}
