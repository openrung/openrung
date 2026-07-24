// Package brokerapi is the shared OpenRung end-user broker client.
//
// It owns broker URL policy, request construction, identity and cache headers,
// relay-list signature verification, and the opportunistic ECH transport used
// for the Cloudflare broker front. Applications retain their own UI, retry,
// persistence, and tunnel policy. Relay registration and heartbeat clients are
// operational relay concerns outside this module.
package brokerapi
