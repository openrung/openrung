# Per-relay CloudFront WSS front

This runbook describes the CloudFront configuration for the WSS fallback on an
eligible, direct-mode Foundation relay. It is deliberately **per relay**. A
CloudFront distribution used for relay A must have relay A itself as its only
origin; it must never select, proxy to, or fail over to relay B.

The public listeners have separate jobs:

```text
direct path:  client ── Reality/TCP ─────────────────────────► relay public :443

fallback:     client ── WSS ─► CloudFront ── TLS :8443 ─► relay origin TLS
                                                            │
                                                            └─ loopback-only
                                                               relay-local sidecar
                                                                 │
                                                                 └─ fixed TCP
                                                                    127.0.0.1:443
```

CloudFront terminates the viewer-side outer TLS connection. The relay origin
TLS endpoint terminates the separate CloudFront-to-origin TLS connection and
passes only the WSS handler to the loopback-only sidecar. The sidecar removes
WebSocket framing, terminates the bounded stream multiplexer, and copies each
opaque inner Reality stream to its fixed loopback endpoint. Reality still
authenticates and encrypts end to end between the desktop client and the
destination relay.

The origin TLS endpoint and sidecar may be packaged together, but there must be
no public cleartext sidecar listener. If they are separate local processes, the
TLS endpoint has one fixed loopback upstream and exposes only
`/api/v1/wss-bridge`; it must not accept a caller-selected upstream.
Disable the origin TLS proxy's access and request logs entirely; in particular,
it must never record `Authorization`, origin tokens,
`CloudFront-Viewer-Address`, source addresses, request URLs, or byte samples.

The repository's relay compose file packages the sidecar in the same relay
image under the optional `wss` profile. Its root filesystem remains read-only,
with one relay-local named volume mounted at `/var/lib/openrung` for the durable
single-use ticket journal. Preserve that volume across normal image updates and
container recreation. Never mount the same replay volume on another relay or
move replay decisions into a fleet service.

This is not a shared service. Do not point a distribution at the broker, Relay
Hub, another relay, a load-balanced relay pool, an origin group, or an origin
selection function. The existing Relay Hub is unrelated volunteer/CGNAT
infrastructure and is not part of this fallback.

## One relay, one or more fronts

The safe baseline is one standard CloudFront distribution with one custom
origin for one relay. For stronger censorship failure-domain diversity, a relay
may have multiple WSS fronts, but every front must still terminate at that same
relay.

Use a separate distribution for each independently advertised front. Give each
distribution one fixed custom origin header whose token is assigned only to
that front:

```text
X-OpenRung-Origin-Token: <secret token assigned to this front>
```

The broker binds a ticket to the relay ID and signed front ID. The sidecar
maps each accepted origin token to exactly one configured front ID, then
compares the ticket claims with its local relay ID and the front ID derived
from that token. A token must never be assigned to two fronts. CloudFront
overwrites a viewer-supplied value for an origin custom header, so the token
cannot be selected by the client. AWS documents that behavior in [Add custom
headers to origin
requests](https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/add-origin-custom-headers.html).

Several CNAMEs can point at one distribution, but CloudFront does not expose
which alias was used to a custom origin unless the viewer `Host` is forwarded
or edge code is added. Consequently, multiple aliases on one distribution are
one logical front and only one canonical URL may be advertised for that front.
Use separate distributions, with distinct fixed front IDs, when multiple URLs
must be separately advertised and ticket-bound.

Do not forward the viewer `Host`. CloudFront otherwise requires the origin TLS
certificate to match that viewer host rather than the origin hostname. See
[Origin settings](https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/DownloadDistValuesOrigin.html).

“Front” here means a real CloudFront distribution hostname or a valid CNAME,
with matching URL host, HTTP `Host`, and TLS SNI. It does not mean TLS domain
fronting. CloudFront checks domain-fronting conditions and can reject mismatched
requests with HTTP 421; see [Use custom URLs by adding alternate domain
names](https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/CNAMEs.html).

## Origin DNS and TLS

Create an unadvertised origin hostname for the relay, for example
`origin-relay-a.example.net`, resolving to that relay's public address. The
CloudFront origin is:

| Setting | Required value |
|---|---|
| Origin type | Custom origin |
| Origin domain | This relay's origin hostname |
| Origin protocol policy | HTTPS only |
| HTTPS port | `8443` |
| Minimum origin TLS protocol | TLS 1.2 |
| Origin IP address type | IPv4 only initially |
| Origin Shield | Disabled |
| Origin group / failover | None |

AWS permits custom-origin HTTPS ports `80`, `443`, and `1024` through `65535`,
so `8443` is supported. The hostname must resolve publicly, and the certificate
served by the relay origin TLS endpoint on `8443` must be publicly trusted and
cover the configured origin hostname. An expired, incomplete, invalid, or
self-signed chain produces a CloudFront 502. See [Origin
settings](https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/DownloadDistValuesOrigin.html)
and [Require HTTPS between CloudFront and a custom
origin](https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/using-https-cloudfront-to-custom-origin.html).

Viewer IPv6 and origin IPv6 are separate settings. Keeping the custom origin at
its default IPv4-only mode avoids needing the IPv6 origin-facing firewall list;
CloudFront documents the available modes in [Enable IPv6 for CloudFront
distributions](https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/cloudfront-enable-ipv6.html).

Public Reality remains on `443`. Do not put HTTP, TLS termination, or the
sidecar in front of that listener.

## Exact cache behavior

Create one behavior for the WSS path, or use it as the default behavior if the
distribution serves nothing else:

| Setting | Required value |
|---|---|
| Canonical viewer URL | `wss://<front-host>/api/v1/wss-bridge` |
| Path pattern | `/api/v1/wss-bridge` (or the default behavior on a dedicated distribution) |
| Viewer protocol policy | HTTPS only |
| Allowed methods | `GET`, `HEAD` |
| Cache policy | Managed `CachingDisabled` |
| Cache policy ID | `4135ea2d-6df8-44a3-9df3-4b5a84be39ad` |
| Origin request policy | Custom, minimal allowlist below |
| Query strings | None |
| Cookies | None |
| Compression | Disabled by `CachingDisabled` |
| Target origin | This relay's single custom origin |

CloudFront WebSocket support is automatic, but WebSockets use HTTP/1.1 only.
AWS lists the required and recommended handshake headers in [Use WebSockets
with CloudFront
distributions](https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/distribution-working-with.websockets.html).
The canonical URL has no user information, query, or fragment. The client and
sidecar require subprotocol `openrung-wss-bridge-v1`, accept binary messages
only, and disable WebSocket compression.

Allowlist only these request headers:

| Header | Purpose |
|---|---|
| `Authorization` | Carries `Bearer <ticket>` on the upgrade request. |
| `CloudFront-Viewer-Address` | Authenticated source-limit input after origin authentication. |
| `Sec-WebSocket-Key` | Required WebSocket handshake input. |
| `Sec-WebSocket-Version` | Required WebSocket handshake input. |
| `Sec-WebSocket-Protocol` | Forwarded when the client supplies a subprotocol. |
| `Sec-WebSocket-Extensions` | Forwarded so extension negotiation is explicit. |
| `Sec-WebSocket-Accept` | Included because AWS lists it in its recommended WebSocket policy set. |

Do not include `Host`, cookies, or query strings. CloudFront handles
`Connection: Upgrade` and `Upgrade: websocket` as part of its WebSocket support;
they are not origin custom headers.

The WSS upgrade is a `GET`, and CloudFront removes `Authorization` from `GET`
and `HEAD` requests unless configured to forward it. Current AWS guidance
allows `Authorization` to be added individually to an origin request policy.
Because it is intentionally not part of a cache key, caching must remain
disabled. See [Configure CloudFront to forward the Authorization
header](https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/add-origin-custom-headers.html)
and [Managed cache
policies](https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/using-managed-cache-policies.html).

`CloudFront-Viewer-Address` contains the viewer IP and source port and can be
added only through an origin request policy, not a cache policy. The sidecar
must not trust it until the immediate source has passed the origin-facing
firewall and the request has passed origin-token authentication. See [Add
CloudFront request
headers](https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/adding-cloudfront-headers.html).

## Exact origin timeout settings

Use these bounded origin values:

| Setting | Value | Reason |
|---|---:|---|
| Connection attempts | `1` | Prevent CloudFront from retrying a consumed single-use ticket. |
| Connection timeout | `5` seconds | Bound failure before the origin handshake. |
| Origin response/read timeout | `10` seconds | Bound the HTTP upgrade response. |
| Response completion timeout | Unset | Do not impose an HTTP response-completion lifetime on an upgraded connection. |
| Origin keep-alive timeout | `5` seconds | Only affects reusable HTTP connections after a response, not an active WSS session. |

For a custom-origin `GET`, AWS uses the connection-attempt count for response
retries as well as connection attempts. A retry can repeat the same
`Authorization` ticket after the sidecar has atomically consumed it, so this
value must be one. AWS documents the defaults and retry behavior in [Origin
settings](https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/DownloadDistValuesOrigin.html).

CloudFront separately documents a ten-minute WebSocket idle timeout when it
has observed no bytes from origin to viewer. Client-to-origin traffic alone is
not sufficient under that definition. The sidecar should send a WebSocket ping
comfortably inside that window, while still enforcing its own shorter idle
policy and maximum session lifetime. See [CloudFront WebSocket
quotas](https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/cloudfront-limits.html).

## Origin authentication and rotation

The origin-facing firewall proves only that the connection came from some
CloudFront origin-facing server. It does not identify this distribution. The
sidecar must additionally compare `X-OpenRung-Origin-Token` in constant time
against configured per-front token rings. A successful lookup authenticates
the exact front ID; only then may the sidecar trust the viewer address or
compare the ticket's front claim. Tokens are never shared between front rings.

CloudFront configuration updates are not atomic across edge locations. During
an update, some edges use the previous header and others use the new one. Rotate
an origin token in this order:

1. Add the new token to this front's sidecar token ring; keep the previous token
   mapped to the same front ID.
2. Update this front's CloudFront custom origin header to the new token.
3. Wait until the distribution status is `Deployed`.
4. Keep the overlap for an operational safety interval, then remove the old
   token from that front's ring.

AWS describes the mixed old/new propagation window in [Update a
distribution](https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/HowToUpdateDistribution.html).

Ticket signing keys use the same overlap principle, but are controlled by the
broker and sidecar rather than CloudFront: publish and accept the new key before
issuing with it; continue accepting the old key for at least the maximum ticket
TTL plus clock skew and replay-retention window; then remove it. New tickets
must be issued only with the current key. Replay entries must remain effective
across the overlap.

## Origin-facing firewall

Only `8443` is restricted to CloudFront. Public direct Reality on `443` remains
reachable by clients.

### EC2/VPC security group

Allow inbound TCP `8443` only from the AWS-managed IPv4 prefix list:

```text
com.amazonaws.global.cloudfront.origin-facing
```

If origin IPv6 is deliberately enabled, also allow:

```text
com.amazonaws.global.ipv6.cloudfront.origin-facing
```

These lists contain all CloudFront origin-facing servers and AWS maintains
them automatically. See [Locations and IP address ranges of CloudFront edge
servers](https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/LocationsOfEdgeServers.html).

Each list has weight 55. One IPv4 rule consumes 55 of the default 60
security-group rules; adding both address families normally requires a quota
increase. See [AWS-managed prefix
lists](https://docs.aws.amazon.com/vpc/latest/userguide/working-with-aws-managed-prefix-lists.html).

Remove any broader `8443` ingress rule. A permissive rule alongside the prefix
list defeats the restriction.

### Lightsail

Lightsail instance firewalls accept source IP/CIDR ranges, not VPC managed
prefix-list IDs. Build the `8443` allowlist from AWS's official
[`ip-ranges.json`](https://ip-ranges.amazonaws.com/ip-ranges.json), selecting
only `CLOUDFRONT_ORIGIN_FACING` entries for the enabled origin address family.
Do not use the broader `CLOUDFRONT` viewer-edge list.

AWS documents TLS verification and update notifications for the feed in [AWS
IP address
ranges](https://docs.aws.amazon.com/vpc/latest/userguide/aws-ip-ranges.html).
The synchronizer must:

1. Download over HTTPS and validate the certificate and JSON before changing
   rules.
2. Add new ranges before removing retired ranges.
3. Preserve the last known-good rules on fetch, parse, or provider failure.
4. Alert and fail closed if the provider quota cannot hold the new set.
5. Update IPv4 and IPv6 independently.

As verified on 2026-07-21, the feed contained 45 IPv4 and 3 IPv6
`CLOUDFRONT_ORIGIN_FACING` ranges. That is an observation, not a guaranteed
limit. Lightsail currently documents up to 60 source addresses per address
family; see [Control instance traffic with firewalls in
Lightsail](https://docs.aws.amazon.com/lightsail/latest/userguide/understanding-firewall-and-port-mappings-in-amazon-lightsail.html).
Keep origin IPv4-only unless IPv6 is operationally required.

## Quotas and capacity planning

The current [CloudFront quota
table](https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/cloudfront-limits.html)
lists:

| Resource | Default quota |
|---|---:|
| Standard distributions per account | 500, increaseable |
| Alternate domain names per distribution | 100, increaseable |
| Origins per distribution | 100, increaseable |
| Cache behaviors per distribution | 75, increaseable |
| Distributions associated with one cache policy | 100 |
| Distributions associated with one origin request policy | 100 |
| Custom cache policies per account | 20, increaseable |
| Custom origin request policies per account | 20, increaseable |

One independent WSS front consumes one distribution. Multiple fronts for one
relay therefore multiply distribution usage. Do not consolidate different
relays behind one distribution to work around a quota. Request a distribution
increase, add fronts gradually, and shard identical custom origin-request
policies before the 100-distribution association limit. The managed
`CachingDisabled` policy has the same documented association consideration.

A distribution permits one attached viewer certificate. Every CNAME must be
covered by that certificate's SAN; wildcard SANs can cover multiple aliases.
CloudFront viewer certificates managed through ACM are selected from
`us-east-1`. See [Add an alternate domain
name](https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/CreatingCNAME.html).

## Rollout

Roll out one relay and one front at a time:

1. Confirm the relay has a stable relay ID and direct Reality succeeds on
   public `443`.
2. Deploy the loopback-only relay-local sidecar with its fixed
   `127.0.0.1:443` target, local relay ID, accepted ticket-key set, and distinct
   per-front origin-token rings. Configure the relay with
   `OPENRUNG_LISTEN_HOST=0.0.0.0` and `OPENRUNG_CONNECTION_LOG=false`; do not
   advertise WSS yet.
3. Install the origin certificate, bind origin TLS on `8443` with access logs
   disabled and only the
   fixed loopback sidecar handler behind it, and apply the origin-facing
   firewall. Confirm a request without the origin token is rejected without
   dialing Reality loopback.
4. Create the distribution with the exact settings above. Wait for `Deployed`.
5. Exercise authorization failure, wrong-relay ticket, wrong-front ticket,
   replay, source limiting, idle cleanup, and a successful opaque Reality
   session through the front. Keep CloudFront access logs disabled.
6. Add this front to only this relay's signed broker descriptor. Verify the
   broker issues tickets only while that exact capability is advertised.
7. Canary the desktop fallback while continuing to measure direct Reality
   independently. Add further relays/fronts only after the canary is stable.

## Rollback

Rollback is additive and does not touch public Reality on `443`:

1. Stop advertising the affected front in the relay's signed capability and
   stop issuing new tickets for it.
2. Wait at least the ticket TTL plus clock skew. Allow established WSS sessions
   to drain only up to the sidecar's configured maximum lifetime; force-close
   after that bound.
3. Disable the CloudFront distribution or remove its DNS only after ticket
   issuance has stopped. Retain both origin tokens during CloudFront propagation.
4. Stop the sidecar listener and remove the `8443` firewall rules after all
   sessions are gone.
5. Leave direct relay registration, public `443`, Reality keys, and direct
   health history unchanged.

For an urgent origin-token compromise, first add a replacement token to the
sidecar, update CloudFront, and retain the compromised token only for the
shortest propagation window that availability policy permits. A ticket-key
compromise requires disabling issuance for the affected key ID and front,
removing the capability if necessary, and draining within the configured
session lifetime; never silently widen ticket validation.

Protocol, failure-classification, logging, and client scope are documented in
[`docs/wss-fallback.md`](../../docs/wss-fallback.md).
