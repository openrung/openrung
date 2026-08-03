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

`brokerapi/cloudfront_tls.go` applies the same technique to the control plane
in a separate copy — `brokerapi` carries no dependencies, so it cannot import
this module. That copy is default-on rather than opt-in, matches dialed
addresses rather than signed URLs, and additionally disables session
resumption, because it runs on a shared pooled transport. The two are expected
to stay aligned on what they verify, so a change to the recognition rule or the
verification hook should be considered for both.

## Dial failure classification

A blocked WSS handshake is the only evidence OpenRung gets about how a network
interferes with it, and at the moment gorilla's dial returns, that evidence is
maximally rich: typed TCP and TLS errors, x509 verification failures, and — for
any non-101 answer — an `*http.Response` carrying the CDN status, all its
headers, and a slurped body. Every one of those also identifies the user or the
front: error strings embed the distribution hostname and resolved edge
addresses, `X-Amz-Cf-Id` is a globally unique per-request correlation ID, and
certificate errors quote subjects.

`DialClient` therefore **classifies, then destroys**. One central classifier
runs at that choke point, before the response body is closed, and reduces the
whole picture to a single token from the closed set below. Only that token
leaves the module, inside a `DialError`:

- its fields are unexported and `Error()` returns a **compile-time fixed
  string** — never text derived from the underlying error;
- it deliberately has **no `Unwrap`**, so no caller can walk back to the
  destroyed chain;
- `errors.Is(err, context.Canceled)` still works for a cancelled dial, and
  `FailureReason(err)` is the accessor consumers use;
- the `ErrSocketProtectionFailed` sentinel is still returned bare, exactly as
  before, keeping the address-stripping precedent it established.

Phase attribution uses **booleans only** — `tcpConnected`, `tlsStarted`,
`tlsDone`, `gotFirstResponseByte`, `bogonAddress`. There are no timestamps and
no durations anywhere in this path, and adding one would be a privacy
regression, not a feature: a censor chooses when it drops or resets, so any
attacker-modulated number attached to an event becomes a tag that can single a
user out of a shared address pool. The no-SNI path marks the TLS booleans
directly (gorilla's httptrace TLS hooks never fire when `NetDialTLSContext` is
set); the ordinary-SNI path marks the same booleans through `httptrace`, so both
front types mint identical tokens. `GotConn` is deliberately not registered —
its `GotConnInfo.Conn` exposes the resolved edge address.

### Token registry

| Token | Meaning |
| --- | --- |
| `ws_upgrade` | A 101 whose Upgrade/`Sec-WebSocket-Accept` headers failed validation. A malformed `permessage-deflate` offer also lands here. |
| `http_401` | The relay sidecar rejected the ticket — which proves the whole censored path worked end to end. |
| `http_403` | The CDN edge refused. AWS-side and actionable with AWS, not with the local network. |
| `http_421` | CloudFront misdirected request: the no-SNI Host-routing drift signature, and a deployment self-check. |
| `rate_limited` | 429, reusing the ecosystem-wide token. |
| `http_502` / `http_503` | Our own sidecar or relay is down — observed *through* a working censored path. |
| `http_other` | Any other status. The raw code is never interpolated into a token beyond this list. |
| `ws_subprotocol` | The post-101 checks: unsolicited extensions, or the required subprotocol missing. |
| `dns_bogon` | The dial was directed into reserved address space. A public CDN name cannot legitimately resolve there, so this is DNS injection (Iran classically answers `10.10.34.x`). Takes precedence over the network tokens it would otherwise masquerade as, because the countermeasure is a DNS one, not IP diversity. |
| `dns_failure` | Resolver-level failure or blocking. |
| `cancelled` | Local intent. Excluded from every censorship numerator. |
| `connection_refused` | A RST answered the SYN: IP-level blocking, or an edge outage if it is global. |
| `network_unreachable` | Dead uplink or airplane mode. Keeps benign noise out of the interference buckets. |
| `connection_reset` / `tls_reset` / `response_reset` | A reset or EOF, split by the phase it interrupted. |
| `tls_not_tls` | The peer answered non-TLS bytes on :443 — an injected blockpage or captive portal. Essentially never benign toward a CDN. |
| `cert_expired` | The chain failed only because it had expired, which overwhelmingly means a wrong device clock. Split out to keep the next token high-precision. |
| `cert_verify` | Any other certificate rejection: active interception, today otherwise indistinguishable from a timeout. |
| `tls_alert` | The peer sent a fatal TLS alert — a TLS-terminating middlebox actively refusing. |
| `tls_handshake` | TLS residual, reusing the ecosystem-wide token. |
| `tcp_timeout` | The SYN was silently dropped. The censor never saw a ClientHello, so this cannot be SNI-triggered. |
| `tls_timeout` | ClientHello sent, then silence: the primary silent-drop DPI signature. |
| `response_timeout` | The handshake was admitted and the first data killed — a DPI mode that adding SNI would not fix. |
| `handshake_timeout` | Defensive residual for a timeout in no identifiable phase. |
| `unclassified` | Nothing matched. Deliberately distinct from consumers' legacy `unknown`, which after this change uniquely identifies pre-taxonomy builds; the `unclassified` share is this taxonomy's own coverage meter. |

Two entries are quasi-identifier-adjacent and are listed as such rather than
waved through: `cert_expired` is a device-clock tell worth roughly one bit, and
`dns_bogon` is semi-stable for anyone behind a split-horizon resolver or a
captive portal.

TCP-phase tokens describe the **final address attempt** of a dial that may have
tried several resolved addresses.

The socket errnos behind `connection_refused`, `network_unreachable`, and the
reset tokens are matched through a build-tagged table (`failure_posix.go`,
`failure_windows.go`) because Go defines `syscall.ECONNREFUSED` and friends on
Windows as its own invented values while the net package surfaces the raw
Winsock numbers — so matching the portable names there would compile cleanly,
never match, and silently degrade every socket failure to `unclassified`. The
standard library carries the same split in `net/error_unix.go` and
`net/error_windows.go`. CI runs on Linux only, so a test asserts the table is
non-zero and collision-free rather than relying on a platform run. That check
alone cannot catch a reversion to the portable names — Go's invented values are
also non-zero and distinct — so a windows-tagged companion test pins the raw
Winsock numbers themselves; it needs a Windows test run to execute, which CI
does not provide, leaving the hardcoded literals as the everyday defense and
any one-off developer run as the tripwire.

### The set is frozen

The taxonomy is itself a censor-writable channel. An adversary sitting on the
path chooses whether to refuse, drop, reset, or answer with a status, and so
writes roughly four to five bits into each event a targeted user emits. That is
accepted as inherent to diagnosing interference at all — you cannot measure
interference without letting the interferer influence the measurement — but it
is the second reason, beyond leak risk, that the token set is closed. Adding or
changing a token is a privacy-review event: it must be justified against both
what it could leak and what an adversary could deliberately write into it.

Consumers must never serialize anything but the token, and must project it
through an explicit allowlist so an unrecognized value degrades to their generic
transport-failure reason instead of reaching telemetry verbatim.

### Ticket safety

On a dial that TLS verification rejects — including a DNS-poisoned dial into
reserved space — verification fails before any HTTP request is written, so no
`Authorization` bearer bytes ever reach the endpoint. `failure_test.go` asserts
this for every certificate-failure mode, and `cloudfront_tls_test.go` asserts it
for the no-SNI path.

## Compatibility tests

`testdata/golden.json` and `golden_test.go` pin the protocol constants, front
normalization decisions, and yamux parameters. `interoperability_test.go`
exercises a live WSS binary stream through both yamux roles and the loopback
client adapter. A golden change is a protocol review event and normally
requires a `ProtocolVersion` decision. Error classification is additive API
surface, not protocol surface, and does not touch the golden vectors.

`failure_test.go` provokes each token on a live wire — a refused port, a hung
dial, a mid-TLS teardown, a plaintext listener on the TLS port, invalid and
expired certificates, a version-mismatch alert, each CDN status, a bad upgrade,
a silent edge — in both TLS modes wherever the mode changes the code path. Every
one of those cases also asserts that the resulting text carries no hostname, no
address literal, and no ticket bytes; that assertion is the mechanical guard a
reviewer can rely on.

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
