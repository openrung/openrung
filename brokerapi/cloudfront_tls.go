// SPDX-License-Identifier: GPL-3.0-or-later

package brokerapi

import (
	"net"
	"strings"
)

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

// cloudFrontVerification binds the connection to the exact distribution being
// dialed. CloudFront's no-SNI default certificate covers every one-label
// distribution name, so suppressing SNI costs this front nothing: the peer
// still has to hold a certificate for the host the request is addressed to.
//
// Azure Front Door cannot do this; see azureFrontDoorVerification.
func cloudFrontVerification(distribution string) noSNIVerification {
	return noSNIVerification{hostname: distribution}
}
