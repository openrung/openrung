// Package brokerapi is the shared OpenRung end-user broker client.
//
// It owns broker URL policy, request construction, identity and cache headers,
// relay-list signature verification, and the transport that keeps a built-in
// front's hostname out of the ClientHello: opportunistic ECH for the Cloudflare
// front, which falls back to ordinary TLS when ECH is blocked, and
// unconditional SNI-less TLS for the CloudFront front. Applications retain their own
// UI, retry, persistence, and tunnel policy. Relay registration and heartbeat
// clients are operational relay concerns outside this module.
package brokerapi
