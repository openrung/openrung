# Relay-local WSS fallback

## Scope and invariant

The WSS fallback exists for users whose network can reach a CDN hostname but
blocks a relay's raw IP address. It is available only on eligible direct-mode
Foundation relays that explicitly advertise it in their signed descriptor.

There is no standalone WSS intermediary and no cross-relay router:

```text
desktop client
  ├─ first choice ── Reality/TCP ─────────────────────► selected relay :443
  └─ fallback ────── WSS front ─► relay origin TLS :8443
                                      └─ loopback-only relay-local sidecar
                                           └─ fixed 127.0.0.1:443
```

Every advertised WSS front belongs to exactly one relay and its CDN origin is
that relay. The public origin TLS endpoint has only one local WSS handler; the
sidecar itself is loopback-only. The relay-local sidecar has one compiled or
startup-configured Reality loopback target. It accepts no destination, host,
port, URL, or DNS name from a client, ticket, broker response, HTTP header, or
WebSocket frame. It performs no DNS resolution and cannot dial another relay.

The existing volunteer Relay Hub is outside this design and must not be used
for Foundation WSS fallback traffic.

## End-to-end transport

The fallback adds an outer HTTPS/WebSocket transport and a bounded stream
multiplexer around existing Reality connections:

```text
WebSocket binary byte stream
  └─ bounded multiplexing session
       ├─ stream 1: complete, unmodified VLESS + Reality + Vision bytes
       ├─ stream 2: complete, unmodified VLESS + Reality + Vision bytes
       └─ ... up to the ticket and sidecar stream limit
```

CloudFront can observe the outer TLS connection, HTTP upgrade metadata, frame
sizes, timing, and opaque bytes. The relay-local sidecar can observe ticket and
stream-control metadata plus the same opaque Reality bytes. Neither receives
the Reality private session keys or plaintext destinations. Reality continues
to authenticate the selected relay and encrypt client traffic end to end.

For a native `*.cloudfront.net` distribution URL, the client omits TLS SNI and
CloudFront selects the distribution from the encrypted HTTP `Host` header.
The client still verifies CloudFront's default certificate against the exact
signed URL hostname. This removes one cleartext hostname signal, but ordinary
DNS resolution can still expose the distribution hostname; custom CNAME and
non-CloudFront fronts retain normal SNI.

The client and sidecar require WebSocket subprotocol
`openrung-wss-bridge-v1`, disable WebSocket compression, and accept binary
messages only. One ticket authorizes one bounded multiplexing session, not an
unbounded number of inner streams.

The sidecar must copy bytes without interpreting VLESS, Reality, DNS, HTTP, or
the eventual target address. A WSS success is not a substitute for the inner
Reality handshake succeeding.

## Shared transport implementation

The nested Go module `github.com/openrung/openrung/wsscore` is the single
reusable implementation of these data-plane mechanics. The desktop client and
relay-local sidecar both consume it; neither maintains a second WebSocket,
yamux, or opaque-copy implementation. The module owns the public protocol
constants, strict production front-URL validation, binary-only WebSocket stream
adaptation, the shared bounded yamux profile, opaque bidirectional copying,
session/stream lifecycle controls, and an optional socket-control hook for a
future Android caller to connect to `VpnService.protect`.

`wsscore` is intentionally authority-free. Its client is given one exact URL
and an opaque bearer ticket by its caller, and its server-side transport is
given an already authenticated, locally authorized connection. Ticket issuance
and verification policy, durable replay storage, origin-token authentication,
viewer-address trust and source admission, signed relay capability handling,
CloudFront and relay deployment, direct-first and broker-front orchestration,
telemetry, and platform UI all remain outside the module. In particular, the
module cannot choose a relay or client-supplied destination, and extraction
does not change the sidecar's one fixed loopback target.

The module has its own `VERSION`, golden/interoperability suite, and
`wsscore/vX.Y.Z` nested-module tags. In-repository root and desktop builds use
local replacements so the desktop and sidecar move together. Other
repositories must pin a released tag and deliberately upgrade it; a module tag
is not an Android or iOS application release.

## Signed capability and fronts

An eligible relay registers a WSS capability signed by its stable relay
identity. The broker verifies that private registration proof, then publishes
the verified fronts inside its own signed relay directory descriptor. A
capability contains one or more fronts, currently bounded to four. Each front
has a stable front ID, canonical `wss://` URL, and protocol version. A shared
global bridge URL must never be injected into every descriptor.

The broker may issue a ticket only when all of the following are true:

- The relay descriptor is live, direct-mode, Foundation-attested, and currently
  advertises WSS.
- The requested front ID and canonical URL are an exact member of that relay's
  signed capability.
- The ticket subject is that exact relay ID and exact front ID; neither value
  can be supplied later by the sidecar or changed by the client.

Removing a front from the signed capability immediately stops new ticket
issuance. Already issued tickets expire within their short, fixed lifetime.

## Ticket protocol

A ticket is a signed, short-lived, single-use authorization for one WSS
handshake. Its authenticated claims include at least:

- protocol version and signing-key ID;
- unpredictable unique ticket ID;
- exact relay ID;
- exact signed front ID;
- issued-at, not-before, and expiry times;
- a maximum inner-stream count no greater than the protocol ceiling.

Version 1 tickets use the fixed `openrung-wss-bridge` audience and Ed25519
signatures selected by an authenticated key ID. The ticket contains no browsing
destination or dial target. Unknown versions, audiences, or key IDs, duplicate
or unknown claims, malformed/non-canonical encodings, excessive lengths,
future-issued tickets outside bounded clock skew, and expired tickets fail
closed.

The sidecar verifies, in order:

1. The immediate connection satisfies the origin-facing firewall policy.
2. The CloudFront-injected origin token matches an active token in constant
   time and maps to exactly one configured front ID. Tokens are not shared
   across fronts.
3. The ticket signature, time window, key ID, and structure are valid.
4. The ticket relay ID equals the sidecar's configured local relay ID.
5. The ticket front ID equals the front ID authenticated by the origin token.
6. The unique ticket ID is atomically consumed in the replay store.

Only then may the sidecar complete the upgrade and start the bounded
multiplexing session. For each permitted inner stream, it connects only to its
fixed `127.0.0.1:443` Reality listener. A failure after consumption requires a
new ticket; CloudFront origin connection attempts are therefore set to one.

Production replay state is a relay-local durable journal, not process memory.
The sidecar hashes each ticket ID and synchronously persists the hash and
bounded expiry before treating consumption as successful. It obtains an
exclusive lock, repairs only a bounded incomplete crash tail, and fails closed
if the journal cannot be opened, locked, written, synchronized, or compacted.
The compose deployment mounts a writable named volume at `/var/lib/openrung`
while keeping the rest of the sidecar filesystem read-only. Routine restart,
upgrade, and rollback procedures must preserve that volume; it is neither
shared between relays nor replaced by a centralized replay service.

The client sends the ticket only as:

```http
Authorization: Bearer <ticket>
```

Tickets must never appear in the WSS URL, query string, path, cookies,
subprotocol, logs, metrics, command line, or error text.

The current protocol ceilings are a 4,096-byte compact ticket, a five-minute
maximum ticket lifetime, two minutes of maximum configurable clock skew, and
1,024 inner streams per ticket. Deployments may tighten these values but must
not widen them without a new reviewed protocol version.

## Obtaining a ticket

Ticket acquisition is control-plane HTTPS, not part of relay health probing.
The desktop client:

1. Selects a relay using the normal signed directory and health ranking.
2. Attempts direct Reality first.
3. Requests a ticket only after the direct attempt reports an eligible genuine
   remote network/data-path failure.
4. Requests the ticket for one exact advertised relay/front pair.
5. Tries the corresponding WSS URL with the ticket in `Authorization`.

Ticket requests must use HTTPS, use the normal trusted certificate store, and
reject every redirect rather than forwarding credentials to the redirect
target. When several broker fronts are configured, a network failure or
eligible transient server response advances to the next front within one total
deadline. A `Retry-After` value is honored only when it parses successfully and
falls within the configured maximum wait and remaining total deadline;
unbounded dates or delays are rejected or clamped according to client policy.

Broker-front failure, ticket denial, CDN failure, and WSS failure are fallback
outcomes. They must not be reported as failures of the destination relay's
direct path and must not reduce that relay's normal health score.

## Direct-first failure classification

The fallback is intentionally narrower than “direct did not work.”

Eligible failures are evidence that a correctly prepared direct connection
could not traverse the remote network path, such as a bounded TCP connect
timeout, unreachable/refused/reset path, or equivalent remote I/O failure
before a usable direct session is established.

The following are local failures and must not request a ticket:

- missing, incompatible, or non-executable sing-box;
- invalid generated configuration or local configuration validation failure;
- inability to create local files, bind the loopback proxy, or start the
  process;
- local permission, TUN/proxy setup, or OS integration failure;
- cancellation, explicit disconnect, or application shutdown;
- an invalid relay descriptor or unsupported local feature.

A successful direct connection never requests a ticket, opens a CDN
connection, or changes WSS state. This is a required test invariant, not an
optimization.

WSS state is kept separate from relay health. A failed front can enter a short,
bounded front-local cooldown, but that cooldown must not poison direct ranking
or another front. A fatal WSS transport result is scoped to the current local
network epoch. Network-interface, route, DNS, resume-from-sleep, or equivalent
connectivity changes clear the stale transport latch so a temporary outage
cannot permanently disable fallback. Recovery still begins with a fresh direct
attempt; it never reuses a consumed ticket.

## Resource and lifecycle controls

All limits are finite, configurable, and enforced before allocating
unbounded resources:

| Control | Required behavior |
|---|---|
| HTTP/TLS/WebSocket handshake deadline and concurrency | Close incomplete handshakes within one fixed deadline and cap in-flight ticket/replay/upgrade work globally. |
| Header and ticket size | Reject oversized requests before ticket parsing or logging. |
| Origin dial deadline | Bound the single fixed loopback dial. |
| WebSocket message/frame and stream buffer | Bound memory; do not buffer a full session. |
| Concurrent sessions | Enforce both global and per-source ceilings. |
| Handshake/request rate | Enforce configurable global and per-source token buckets. |
| Idle timeout | Close sessions with no useful traffic; WebSocket ping/pong does not make an otherwise abandoned inner stream immortal. |
| Maximum lifetime | Close every session even if continuously active. |
| Replay retention | Durably retain consumed-ID hashes through ticket expiry plus permitted clock skew, including across process/container restarts. |
| Shutdown | Stop upgrades, cancel the loopback dial, close both copy directions, and wait only a bounded drain period. |

`CloudFront-Viewer-Address` may be used for the source key only after the
origin token is authenticated. Parse the address strictly and use the IP,
not the ephemeral source port, for source limits. If it is absent or malformed,
fail closed or use a deliberately more restrictive authenticated-edge bucket;
never trust viewer-supplied `X-Forwarded-For` as an equivalent.

Per-source limits must be deployment-configurable. Iranian mobile carriers may
place many legitimate users behind one carrier-grade NAT address, so a small
hard-coded “users per IP” value is not acceptable. Global caps, handshake-rate
limits, short ticket lifetimes, and single-use replay protection remain in
force even when a carrier-NAT source ceiling is raised.

## Rotation

Origin-token and ticket-key rotation both require overlap:

- The sidecar keeps a separate active-token ring for each front. Add the new
  token to the same front's ring first, update that WSS front, wait for the CDN
  configuration to finish propagating, then retire the previous token. Never
  reuse an origin token for a different front.
- Ticket issuers sign only with the current key. Brokers and sidecars accept
  current and previous verification keys until the maximum ticket TTL, clock
  skew, and replay-retention interval have elapsed. An old key is never used
  for new issuance after the switch.
- Key IDs are mandatory and unknown IDs fail without trying every key.
- Replay state is not cleared by a key reload and is keyed so identical ticket
  IDs cannot bypass protection during overlap.

The CloudFront-specific sequence is in
[`deploy/relay/cloudfront-wss.md`](../deploy/relay/cloudfront-wss.md).

## Privacy and operational counters

Do not log or export:

- bearer tickets, ticket IDs, signatures, or `Authorization`;
- origin tokens or ticket signing/verification key material;
- `CloudFront-Viewer-Address`, viewer IPs, or source-limit keys;
- WebSocket or Reality payload bytes, frame samples, or hashes;
- inner or outer target addresses;
- per-session identifiers, front URLs, or relay IDs as metric labels.

CloudFront standard access logs, real-time access logs, and connection logs are
disabled for WSS-front distributions because they are per-request records and
can contain viewer addresses. AWS documents the available fields, including
`c-ip`, in [Configure standard
logging](https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/standard-logging.html).

The sidecar emits only fixed-name, label-free aggregate counters and gauges,
for example total accepted handshakes, total authentication rejections, total
replays, current sessions, total source-limit rejections, total loopback dial
failures, and total lifecycle closes. Outcome names are separate fixed
counters, not free-form or attacker-controlled labels. Error responses and
operator messages are generic and never echo request material.

The origin TLS reverse proxy has access/request logging disabled. A relay that
advertises WSS also disables the ordinary relay per-connection observer and
lets Xray bind `0.0.0.0:443` directly; otherwise each loopback inner stream
would create an address and byte-count record outside the sidecar's aggregate
counter policy.

## Client repository scope

This repository's desktop client is the only client restored by this change.
It consumes `wsscore` for the shared transport while retaining direct-first
selection, local-failure classification, ticket and broker-front failover,
bounded retry handling, telemetry, and independent fallback health in the
desktop application layer as described above.

Android and iOS are developed in separate repositories. They are not updated,
restored, or made WSS-capable by this repository change. Mobile release notes
must continue to describe WSS fallback as unavailable until each mobile
repository independently implements and tests the same protocol and security
contract. The reusable module and its Android socket-control hook are adoption
building blocks only: Android still has to wire the hook to
`VpnService.protect`, and both platforms must add their own ticket,
direct-first, lifecycle, and UI integration before publishing a separate
mobile release.

## Rollout and rollback contract

Advertisement is the feature switch. Deploy and authenticate the relay-local
sidecar and WSS front first; advertise the signed capability only after
wrong-relay, wrong-front, replay, source-limit, rotation, cleanup, and opaque
Reality tests pass. Desktop rollout may then canary a small set of relay/front
pairs without changing direct selection.

Rollback removes the front from the signed relay capability and stops ticket
issuance first. Issued tickets expire, active sessions drain only to their
maximum lifetime, and then the CDN and sidecar listener can be disabled. Direct
Reality on public `443`, relay registration, and direct health history remain
untouched throughout rollback.
