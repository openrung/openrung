package relay

import "github.com/openrung/openrung/brokerapi"

// IsIPv6Host delegates to the client-facing schema's helper so the broker's
// family-aware ranking and the clients' family selection judge hosts
// identically.
func IsIPv6Host(host string) bool {
	return brokerapi.IsIPv6Host(host)
}
