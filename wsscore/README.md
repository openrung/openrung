# wsscore

`github.com/openrung/openrung/wsscore` is OpenRung's reusable opaque
Reality-over-WebSocket transport. It is a nested Go module and the **single
source of truth** used by the desktop client and the relay-local sidecar in
this repository. Android and iOS live in separate repositories and must pin,
integrate, test, and release this module independently; adding it here does not
restore either mobile client by itself.

The CDN terminates the outer TLS/WebSocket connection at the destination
relay's own sidecar. The existing Reality connection remains end to end inside
the binary WebSocket/yamux stream, so neither the CDN nor the sidecar receives
Reality key material or interprets payload bytes.

## What belongs here

- Protocol constants and the signed `Front` wire shape.
- Strict canonical front-ID and production `wss://` CDN URL validation.
- The client-side loopback adapter (`DialClient`, `Client.Serve`, and
  `Client.Close`).
- Binary-only WebSocket-to-`net.Conn` adaptation, the shared yamux profile,
  opaque bidirectional copying, and reusable idle/lifetime controls.
- The gomobile-friendly `SocketProtector` hook. Android implementations must
  delegate `Protect(fd int32) bool` to `VpnService.protect(fd)` so the outer CDN
  socket does not recurse into the VPN tunnel. Protection fails closed and is
  installed before `connect(2)`. Mobile repositories should expose their own
  small gomobile binding wrapper around the client API; `wsscore` also exports
  Go-native relay/test primitives and is not itself a direct gobind surface.

## What never belongs here

- Ticket issuance, claims, signing keys, verification, or replay storage.
- Broker-front selection, HTTPS redirect/Retry-After policy, or direct-first
  fallback and relay-health orchestration.
- CloudFront origin-token authentication, trusted viewer-address handling,
  per-source/global abuse controls, or aggregate telemetry.
- Any target selector, relay routing table, or DNS-based destination dial. The
  root sidecar remains responsible for dialing its one configured loopback
  Reality endpoint.
- Deployment configuration or platform UI.

## Security invariants

`ValidateFrontURL` accepts only an already-canonical production URL with:

- the `wss` scheme and default port;
- a multi-label DNS name (never an IP literal or localhost);
- the exact `/api/v1/wss-bridge` path; and
- no userinfo, query, fragment, escaped path, or surrounding whitespace.

`DialClient` sends the opaque ticket only as `Authorization: Bearer ...`,
disables compression and proxy inheritance, requires the WSS subprotocol, binds
its inner endpoint to a loopback IP literal, and applies bounded handshake,
message, stream-idle, no-stream-idle, stream-concurrency, and session-lifetime
controls. Custom network dial callbacks cannot be combined with
`SocketProtector`, because that could silently bypass Android socket
protection. Custom callbacks that claim to have completed TLS are rejected
entirely, and TLS verification cannot be disabled. When
`ClientOptions.CloudFrontNoSNI` is enabled for a native, one-label
`*.cloudfront.net` distribution URL, `DialClient` omits the ClientHello SNI,
accepts only a normally trusted certificate valid for the exact signed URL
host, and leaves that hostname in the encrypted HTTP `Host` header. Custom
CloudFront CNAMEs and every other CDN URL retain ordinary SNI derived from the
signed front URL. Encrypted ClientHello configuration is rejected on the
CloudFront no-SNI path so it cannot silently add a different public SNI. The
in-repository desktop client enables this option; external module consumers
must opt in deliberately when they upgrade.

## Compatibility tests

`testdata/golden.json` and `golden_test.go` pin the protocol constants, front
normalization decisions, and yamux parameters. `interoperability_test.go`
exercises a live WSS binary stream through both yamux roles and the loopback
client adapter. A golden change is a protocol review event and normally
requires a `ProtocolVersion` decision.

## Pin and release procedure

1. Change `wsscore` in an OpenRung PR. Root and desktop consumers use local
   `replace` directives so the repository stays atomic.
2. Bump `wsscore/VERSION` in the same PR. CI rejects non-README changes without
   a fresh, untagged version.
3. Merge. The repository tags `wsscore/v$(VERSION)` on that merge commit.
4. Mobile repositories pin the new module tag, rebuild their gomobile artifact,
   run platform VPN tests (including Android socket protection), and ship their
   own releases.

For local cross-repository development, use an uncommitted `go.work` or a
temporary consumer-side `replace`; released builds must pin a real
`wsscore/vX.Y.Z` tag.

## License

GPL-3.0-or-later, same as the parent repository (see `LICENSE` in this
directory).
