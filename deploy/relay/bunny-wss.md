# Per-relay bunny.net WSS front

This runbook covers the bunny.net pull-zone form of the WSS fallback. The
architecture, threat model, sidecar contract, ticket binding, and rollout
discipline are identical to
[`cloudfront-wss.md`](cloudfront-wss.md) — read that first. **Everything it
says about one relay per front, never sharing an origin, never forwarding the
viewer `Host`, and never advertising before the full matrix passes applies here
unchanged.** This document records only what differs on bunny.

## Why bunny for these relays

Cloudflare is unusable for relay data: its Self-Serve Subscription Agreement
§2.2.1(j) prohibits proxying VPN traffic, and the Galileo terms do not waive it.
bunny.net's [Acceptable Use Policy](https://bunny.net/acceptable-use/) contains
no equivalent clause — it restricts unlawful material and, separately, requires
prior written consent for live video streaming of short events. Neither reaches
this traffic. That difference, not price, is why relay fronts go here.

## Provider facts this design depends on

Verified 2026-08-06; re-check before relying on any of them again.

| Fact | Observed value |
|---|---|
| Default certificate without SNI | `CN=*.b-cdn.net`, SAN `*.b-cdn.net`, `b-cdn.net`, issued by Sectigo DV |
| Origin URL with a non-standard port | `https://host:8443` accepted and stored verbatim |
| Origin created against unresolvable DNS | Accepted; edge answers `502` with `errorcode: 107` until the origin resolves |
| WebSockets | `EnableWebSockets`; connection tiers `500`, `1000`, `2500`, `5000`, `10000`, `25000` |
| WebSocket price | $0.235 per million connection-minutes, plus bandwidth at standard CDN rates |
| Edge rule with no trigger | Rejected: `edgerule.invalid` / "At least one condition is required" |
| Origin token visible to clients | No — with `ActionType` 6 the token appears in neither response headers nor body |
| `Authorization` forwarded to origin | Yes, unmodified, with no origin-request-policy equivalent to configure |
| `SetRequestHeader` vs a viewer-supplied value | Overwrites it; a client cannot inject its own value for a rule-set header |
| `X-Real-Ip` | The true client address, and also overwritten rather than trusted from the client |
| `X-Forwarded-For` | **Not the client.** A fixed internal-hop address, identical for every viewer |
| Client source port | Not exposed, in any header or edge-rule variable |
| `%{User.IP}` edge-rule variable | The true client address, same as `X-Real-Ip` |

CloudFront needs `Authorization` explicitly added to an origin request policy because
it strips the header from `GET`. bunny forwards it by default, so the WSS ticket
reaches the sidecar with no extra configuration.

The `*.b-cdn.net` no-SNI certificate is what lets the client drop SNI here
exactly as it does on CloudFront. `wsscore`'s `nativeFrontZones` lists both
zones; a front host outside them keeps sending SNI.

bunny does identify itself in responses: `server: BunnyCDN-<pop>`, `cdn-pullzone`,
`cdn-requestid`, `cdn-status`. A censor that can see plaintext responses can
tell bunny is in use and can read the numeric pull zone ID. CloudFront leaks the
equivalent through `x-amz-cf-id`. This is a property of every commodity CDN
front; it is not a reason to prefer one.

## Zone naming is a blocking-resistance decision

CloudFront assigns an opaque `d111111abcdef8.cloudfront.net`. On bunny **we**
choose the label, and bunny zone names are globally unique, so a guessable name
is directly probeable: anyone can resolve `openrung-<relay>.b-cdn.net` and
enumerate the whole fleet's fronts without touching our infrastructure.

Dropping SNI hides the hostname from the ClientHello but not from DNS. Use an
unbranded, non-enumerable label — the front host must not name this project, the
relay, or its location. Never put the relay name in the zone description, and do
not commit the label-to-relay mapping to this repository.

There is deliberately **no checked-in front inventory**. A front's whole value is
that it can be rotated once it is blocked, so a committed mapping would preserve
every hostname this project has ever used — permanently, pre-correlated with its
relay and pull zone, and greppable in one place — which is precisely what
rotation is meant to deny. It would also go stale: a pull zone can exist while no
relay advertises it.

Reconstruct it live instead, from the bunny account and the broker's
authenticated, untruncated operational inventory. Put the dedicated
`OPENRUNG_API_TOKEN` in a mode-`0600` file outside the repository and point the
script at the broker's direct TLS origin:

```sh
OPENRUNG_API_TOKEN_FILE=/path/to/broker-api-token \
OPENRUNG_BROKER_URL=https://broker-origin.openrung.org \
  deploy/relay/bunny/bunny-wss-front.sh inventory
```

The script calls `GET /admin/api/relays/inventory`, never the ranked
`/api/v1/relays` client page: that page is capped at 20 candidates and can omit
a healthy relay merely because it ranked below the cut. It rejects any response
whose wire contract is not the unbounded `"inventory"` channel, whose count
does not match its relay array, whose IDs are duplicated or out of stable order,
or which carries a client-page `limit`. The bearer is passed to curl through a
temporary mode-`0600` header file, not argv or the environment, and the script
refuses the public `broker.openrung.org` CDN front so the credential reaches the
origin directly.

The resulting mapping still contains only public front and relay metadata:
clients need the front URL, and origin hostnames appear in Certificate
Transparency the moment Caddy issues a certificate. Deriving therefore leaves
no durable record of retired fronts. `.gitignore` covers the state file
`OPENRUNG_WSS_FRONT_STATE_FILE` writes, so a local copy cannot be committed by
accident.

## Pull zone settings

`bunny/bunny-wss-front.sh create|audit RELAY ORIGIN_HOST FRONT_ID ZONE_NAME`
creates or converges exactly one pull zone for one relay and then audits it. The
origin token is read from a mode-`0600` `OPENRUNG_ORIGIN_TOKEN_FILE` and never
appears in argv, output, or the written state file. Re-running is safe: it
converges every mutable setting below, hard-checks the provider-owned status
fields, and updates existing edge rules in place rather than adding duplicates.
After a create or rotation, the final read-back must contain the exact
file-backed token value, not merely a token of the right length.

| Setting | Required value | Reason |
|---|---|---|
| `Enabled` | `true` | A disabled pull zone serves no traffic even when every other setting is correct |
| `OriginUrl` | `https://<origin-host>:8443` | The relay's own origin TLS endpoint |
| `Type` | `1` (volume) | Lower bandwidth price; set `OPENRUNG_BUNNY_ZONE_TIER=standard` for `0` |
| `EnableWebSockets` | `true` | Off by default; the bridge is a WebSocket |
| `MaxWebSocketConnections` | `500` initially | Raise only to a supported tier (`1000`, `2500`, `5000`, `10000`, or `25000`); it is billed by connection-minute |
| `EnableLogging` | `false` | A bridge log is a record of who reached which relay and when |
| `VerifyOriginSSL` | `true` | Proves the edge reached this relay, not something answering on its address |
| `AddHostHeader` | `false` | Forwarding the viewer host would require the origin certificate to match it |
| `OriginHostHeader` | The origin hostname | Fixed, so the origin certificate is what is verified |
| `CacheControlMaxAgeOverride` | `0` | Every upgrade carries a single-use ticket |
| `MonthlyBandwidthLimit` | `500000000000` initially | Pay-as-you-go has no implicit ceiling |

`Enabled` and `Suspended` are provider-owned status fields, not accepted by
bunny's create/update request models. The script therefore requires
`Enabled=true` and `Suspended=false` before any update instead of claiming it
can repair an account-limit or suspension state through the settings endpoint.

Override the 500 GB cap through `OPENRUNG_WSS_MONTHLY_BANDWIDTH_LIMIT` (bytes).
The script accepts only a positive value: bunny's `0` means unlimited, which is
a billing risk on a front whose whole purpose is to be reachable from networks
that block everything else.

## Origin token via edge rule

bunny has no per-origin custom-header field. The token is delivered by an edge
rule:

| Field | Value |
|---|---|
| `ActionType` | `6` — `SetRequestHeader` |
| `ActionParameter1` | `X-OpenRung-Origin-Token` |
| `ActionParameter2` | This front's token |
| `Triggers` | One `Type` `0` (URL) trigger matching exactly `https://<front-host>/api/v1/wss-bridge` |
| `TriggerMatchingType` | `0` |

`ActionType` `5` is `SetResponseHeader`, one below the value used here. Setting
`5` by mistake would return this front's origin token to every client that asked
for the bridge path, defeating origin authentication entirely. The script pins
`6`, and its audit fails if any rule on the zone uses `5`, overrides the origin,
or contains any `ExtraActions`. The last check matters because an unsafe action
can otherwise hide behind a harmless top-level action while sharing its exact
trigger.

bunny requires at least one trigger, which is a better outcome than an
unconditional rule: the exact-URL trigger means no other path, and no
query-bearing variant, is ever given the token.

## Viewer address via a second edge rule

The sidecar needs the viewer address for its per-source session and stream
limits, and reads it from the header named by
`OPENRUNG_WSS_VIEWER_ADDRESS_HEADER`, defaulting to CloudFront's
`CloudFront-Viewer-Address`. It parses `ip:port` and keeps only the address;
an IPv6 viewer counts as its `/64` prefix.
IPv6 may be bracketed (`[2001:db8::1]:443`) or bare (`2001:db8::1:443`); the
bare form is what `%{User.IP}:443` produces for IPv6 viewers.

bunny sends nothing in that shape, so each pull zone carries a second
`SetRequestHeader` rule, on the same exact-URL trigger:

| Field | Value |
|---|---|
| `ActionParameter1` | `X-OpenRung-Viewer-Address` |
| `ActionParameter2` | `%{User.IP}:443` |

and each relay's sidecar is configured with
`OPENRUNG_WSS_VIEWER_ADDRESS_HEADER=X-OpenRung-Viewer-Address`.

The port is a fixed placeholder. bunny exposes no client source port anywhere,
and the sidecar discards the port after parsing, so the value is syntax only —
but it must be non-zero or the address is rejected and every upgrade fails with
`trusted viewer address required`.

**Do not use `X-Forwarded-For` here.** bunny fills it with an internal-hop
address that is identical for every viewer, which would collapse the entire
world onto one source key and silently disable per-source limiting — the limits
would appear configured and enforce nothing. `X-Real-Ip` does carry the true
address and would work if the sidecar accepted a bare IP; the edge rule is used
instead because it produces the shape the shipped sidecar already parses, and
because a rule-set header is demonstrably immune to client injection.

If the sidecar ever learns to accept a bare address, this rule can be dropped in
favour of pointing the header at `X-Real-Ip`, which would remove the fabricated
port.

Rotation is the same overlap discipline as CloudFront, driven by the sidecar's
two-token ring:

1. Add the new token to this front's sidecar ring, keeping the previous token
   mapped to the same front ID.
2. Re-run `bunny-wss-front.sh create` with the new token file. It reuses the
   existing rule's `Guid`, so the value is replaced rather than duplicated.
3. Wait for edge propagation, keep the overlap for an operational interval, then
   drop the old token from the ring.

## Origin-facing firewall

bunny publishes its edge addresses at
`https://api.bunny.net/system/edgeserverlist` as a flat JSON array of individual
IPv4 addresses, several hundred CIDRs after collapsing. That outgrows provider
firewalls — a Lightsail instance firewall accepts 60 source ranges per port and
a Hetzner Cloud firewall 500 effective rules in total — so the allowlist is
enforced on the host instead, on every provider.

`bunny/host-wss-origin-firewall.sh check|apply RELAY HOST` loads an
`inet openrung_wss_origin` nftables table — bunny edge ranges accepted on
`8443`, everything else on that port dropped, IPv6 included — with the host's
own `nft` in one transaction, saves it to
`/etc/openrung/wss-origin-firewall.nft`, and enables a boot unit that reloads
it. The unit has no stop action, so stopping it never reopens the port. It fails
closed, leaving the host untouched, on any fetch, parse, or plausibility
failure. Run `check` first; it prints the exact ranges being added and removed
without touching anything. `iptables -S` and `ufw status` do not show this
table; read it with `nft list table inet openrung_wss_origin`.

Apply it before opening the port, then open `8443` in the provider firewall to
IPv4 only, as a single rule. Only `8443` is restricted. Public Reality on `443`
stays open to everyone.

Run `check` on a schedule and `apply` whenever bunny's published edge list
changes; also run both immediately before each relay rollout. A stale allowlist
can strand new bunny edges or retain access for retired ones, so this is an
ongoing convergence task rather than one-time provisioning.

## Rollout

Identical to the CloudFront rollout, with the provider steps substituted. Per
relay, in order, one relay at a time:

1. Create the unadvertised origin hostname and confirm it resolves to this relay.
2. `foundation-wss-host.sh origin-tls` — Caddy on `8443` with a publicly trusted
   certificate, access logs disabled, only the bridge path behind it.
3. `host-wss-origin-firewall.sh apply`, then open `8443` (IPv4) in the provider
   firewall — restrict `8443` to bunny edges.
4. `foundation-wss-host.sh sidecar` — loopback-only sidecar, ticket verifier
   ring, per-front origin token ring.
5. `bunny-wss-front.sh create` — the pull zone and its edge rule.
6. Run the full `cmd/wssmatrix` sequence: `direct`, `edge`, `origin`, `revoked`,
   then `issued` after advertising. Nothing is advertised until `edge`, `origin`,
   and `revoked` have all passed against this exact zone.
7. `foundation-wss-host.sh advertise` — add the front to this relay's signed
   descriptor. It accepts only native `*.cloudfront.net` and `*.b-cdn.net` hosts,
   because a custom hostname could not be verified under the client's no-SNI path.
8. `foundation-wss-host.sh audit` and a fresh signed directory fetch.

### An IPv6-advertised relay needs `OPENRUNG_LISTEN_HOST=0.0.0.0`

A relay with fronts lets xray own `443` directly and accepts only `0.0.0.0` or
`127.0.0.1` as its listen host, so the sidecar can reach Reality over IPv4
loopback. `entrypoint.sh` defaults an unset listen host to `::`, which that
check rejects: `advertise` then rolls back and exits **1 with no message**. Set
`OPENRUNG_LISTEN_HOST=0.0.0.0` first. xray's `0.0.0.0` listener is dual-stack,
so the relay keeps serving its advertised IPv6 address. Without fronts xray
also binds `0.0.0.0` dual-stack, so the listen host can stay; only a relay
that turns `OPENRUNG_CONNECTION_LOG` on must restore it to unset or `::`,
because the connection observer honours `0.0.0.0` literally and stops
answering on IPv6.

### The relay must have connection logging off first

`OPENRUNG_CONNECTION_LOG` defaults to **false**; older relay images defaulted it
to **true**. A relay refuses to advertise any front while it is on:

```text
WSS fronts require connection logging and connection observing disabled so
relay-local WSS streams produce no per-connection address or byte-count records
```

This is a privacy gate, not a nuisance: WSS streams arrive over loopback, so the
per-connection observer would turn every bridged session into an address and
byte-count record on the relay. Make sure `relay.env` leaves
`OPENRUNG_CONNECTION_LOG` unset or `false` before `advertise`, otherwise the
relay fails to register, and `advertise` rolls the environment back and exits
**1 with no message** — its remote preconditions are bare `test`s. It also
leaves `relay.env.pre-wss-advertise` behind, which blocks the next attempt on
its `! test -e` precondition until it is removed.

## Rollback

As in `cloudfront-wss.md`: stop advertising, wait out the ticket TTL, drain up to
the sidecar's maximum session lifetime, then disable the pull zone. Close `8443`
in the provider firewall only after all sessions are gone. Public Reality on
`443` is untouched at every step.

A pull zone can be disabled without deletion, which keeps the name reserved.
Deleting it frees the label for any other bunny customer to claim, so prefer
disabling unless the intent is to abandon the label permanently.
