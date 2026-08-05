# Azure Front Door as broker front #3

Status: **live and advertised.** Provisioned 2026-08-05 as
`cdn-edge-cxdnhsg2aadmaubj.z02.azurefd.net`, accepted by `cmd/frontcheck`, and
raced **last** in `brokerapi.DefaultBrokerURLs()`. The endpoint prefix is
generic on purpose — see below; the first attempt used a project-identifying
name and was replaced before anything shipped.

Installed clients only gain this front when they update — the front list is
compiled in, and there is no server-side way to push one. Desktop picks it up on
its next release build; the mobile repositories must bump their pinned
`brokerapi` tag to **v0.4.0** and mirror the addition in their `AppConfig`.

## Why Azure, and why SNI-less

The two existing fronts are a Cloudflare Worker (`broker.openrung.org`) and an
AWS CloudFront distribution. A third independent provider means a single CDN,
DNS zone, or account failure cannot fail discovery closed.

Azure's specific value is collateral damage: the Front Door edge shares address
space with Microsoft services a censor is reluctant to break wholesale. It is
*not* metadata hiding — Azure has no ECH. Suppressing SNI is what keeps the
endpoint name off the wire, exactly as on CloudFront.

Azure is a control-plane front only. Relay data must never move through it: the
nonprofit grant buys roughly 24 TB/year, about 29 days of fleet traffic, and the
subscription converts to pay-as-you-go rather than stopping when credit runs
out.

## Measured behaviour

Measured 2026-08-03 against two independent Front Door edge partitions, each
reached with a third party's endpoint in the `Host` header:

| | no SNI | with SNI |
| --- | --- | --- |
| leaf SANs | `[*.azureedge.net]` | `[*.azurefd.net *.z01…z10 *.a01…a03 *.b01 *.b02.azurefd.net]` |
| issuer | Microsoft TLS G2 ECC CA OCSP 06 | Microsoft TLS G2 ECC CA OCSP 02 |
| TLS | 1.3 | 1.3 |
| response | byte-identical to the SNI path | — |

- Partitions checked: `p-0010` (150.171.84.20, 150.171.84.30) and `t-0009`
  (13.107.226.39). Both behaved identically.
- Routing is genuinely driven by the encrypted `Host` header: a real endpoint
  name returned that endpoint's content (HTTP 200, 244752 bytes for a live
  customer site), while an unknown name returned Front Door's own 404.
- Microsoft blocked classic domain fronting — a ClientHello whose SNI disagrees
  with the Host — years ago. The no-SNI case is distinct because there is no SNI
  to mismatch, the same reason it survives on CloudFront. It is undocumented and
  has no SLA.
- **The default certificate belongs to the edge fleet, not to Front Door.** A
  Microsoft edge outside that fleet served an entirely unrelated default
  certificate. This is why every new endpoint must be measured rather than
  assumed.

## The verification tradeoff

`*.azureedge.net` does not cover a `*.azurefd.net` endpoint, so there is no
hostname to verify against. Three options existed:

1. **Pin the `*.azureedge.net` SAN** — chosen. An impersonator needs a
   publicly-trusted certificate for a Microsoft-owned name.
2. **Pin the leaf public key** — rejected. The observed certificate expires
   within about a quarter; a pin baked into shipped desktop and mobile builds
   would break the front on every rotation.
3. **Verify the chain and no name** — rejected. Strictly weaker for no benefit:
   it accepts any certificate any public root ever signed, which an adversary
   can buy for a domain they own.

What the connection proves is therefore *an Azure edge*, not *our endpoint*.
That is survivable because relay lists are Ed25519-signed and verified against
pinned keys, with a `not_after` bound that also defeats replay — but an
impersonating front would still see client identity headers and telemetry, and
could refuse to serve. Hence: **last in the discovery order.**

A custom domain does not help. Without SNI the edge serves the shared
certificate regardless of the Host, so a custom domain would be no better
authenticated while losing the ordinary verification it gets by keeping SNI.

## Provisioning

```bash
bash deploy/broker/azure-front-door-up.sh
```

The script is idempotent and encodes the settings below, including the two that
are load-bearing rather than stylistic (no custom domain, caching off). It
checks the origin first and asserts caching is actually off afterwards.

### Resolved blocker: sponsorship subscriptions cannot create a profile

Hit on 2026-08-04 and cleared by a quota request on 2026-08-05; recorded because
it will recur on any new sponsorship subscription. Subscription `2ac42581-…`
(offer `Sponsored_2016-01-01`, the nonprofit grant) refused profile creation:

```
ERROR: (BadRequest) The number of profiles created exceeds quota.
       Please contact support to increase quota.
```

This is **not** a real count. The subscription has zero profiles, and
`POST …/providers/Microsoft.Cdn/checkResourceUsage` reports `afdprofile`
`currentValue: 0, limit: 500`. The same request fails identically through raw
REST, so it is a resource-provider gate rather than a CLI artifact — the
sponsorship offer appears to enforce a limit that the usage API does not report.

Lifting it needs a **free** quota request, which must go through the Azure
portal: the Support REST API refuses on anything below a paid support plan
(`InvalidSupportPlan … your support plan type is Developer`). Portal path is
Help + support → Create a support request → issue type *Service and subscription
limits (quotas)* → quota type *CDN*.

### Cost, before committing to this front

Retail pricing (queried 2026-08-04, `prices.azure.com`):

| | |
| --- | --- |
| Standard base fee | **$35.00 / month** |
| Standard data transfer out | **$0.17 / GB** |
| Standard requests | $0.01125 / 10K |

The base fee alone is $420/year, roughly a fifth of the $2,000 grant, and egress
is billed well above the $0.087/GB VM rate the earlier estimate assumed. Worth
weighing against Fastly Fast Forward, which is free, has a Tor precedent, and
carries no VPN clause in its AUP.

### Settings

1. Create a Front Door **Standard** profile (Premium's WAF is not needed; a
   profile allows 3000 concurrent connections and caps connections at 2 hours).
2. Add an endpoint. Azure assigns `<name>-<hash>.z01.azurefd.net`. Do **not**
   add a custom domain — see above.
3. Origin group: the broker origin, HTTPS only, with both the host name and the
   origin host header set to `broker-origin.openrung.org`. That name is what
   Caddy on the origin holds a certificate for, and the edge uses the host
   header as SNI to the origin — a bare IP fails certificate validation. Front
   the origin directly, never another CDN front.
4. Route: match `/*`, forward to the origin group, **caching disabled**. The
   relay list must not be cached — a stale signed list is a live outage, and
   `/api/v1/relays` already sets `no-store` at the origin. In the CLI there is
   no `--enable-caching false`; caching is off precisely when the route carries
   no `--cache-configuration`, which is why the script omits it and then asserts
   `cacheConfiguration` is empty afterwards.
5. Health probe: `GET /healthz` over HTTPS. The default probe path is `/`, where
   a failure would mean only that the root has no handler rather than that the
   broker is unhealthy.

Budget alerts only email; they do not stop spending. Set one, and keep the
runbook for deallocating if credit is exhausted.

### The endpoint name must not identify this project

Suppressing SNI keeps the endpoint name out of the ClientHello, but the client
resolves it over **ordinary cleartext DNS** — `brokerapi` installs no custom
resolver and there is no DoH. So the hostname is the one part of this front a
passive on-path observer still sees, and the name we choose is the whole of what
they learn.

An endpoint called `openrung-broker-…` makes that DNS query a keyword match. Two
consequences, the second worse than the first:

- The front can be blocklisted by pattern, without the censor knowing anything
  about this project in advance.
- The query **marks the user** as running this software. In Iran that is a
  personal-safety property, not just a reachability one.

So the prefix is deliberately generic (`cdn-edge`), matching how CloudFront's
`d2r7mdpyevvs1m.cloudfront.net` reveals nothing. Azure appends an unguessable
suffix, so a boring prefix costs nothing. The resource group and profile names
are never on the wire and stay descriptive.

Endpoint names are **immutable** — changing one means creating a new endpoint
with its own route, verifying it, and deleting the old one.

Note the same reasoning indicts `broker.openrung.org`, the *primary* front,
far more directly: it is a subdomain of the project's own domain, and ECH hides
the SNI but not the A-record lookup. That is a larger, separate decision — the
Cloudflare front is the deliberately well-known one — but it is where a
DNS-observing censor gets the most, not here.

### Client IP behind this front

Requests arriving through Front Door are attributed to the **Front Door edge
IP**, not the real client, exactly as they already are through CloudFront. That
is deliberate and lives in [`Caddyfile`](./Caddyfile): the origin strips
`CF-Connecting-IP` and overwrites `X-Forwarded-For` with its own immediate peer,
because a CDN that forwards viewer headers would otherwise let a client inject a
forged client IP. Fidelity is traded for unspoofability.

So the Azure front introduces no new exposure here, but it does inherit the
consequences: per-IP rate limits and the 64-new-identities-per-IP-per-day
registration cap bucket by edge IP for fronted traffic, and telemetry records
the edge IP as `client_ip`. Worth watching after this front carries real load —
if Azure egresses to the origin from a narrower set of addresses than CloudFront
does, those caps bite sooner. `OPENRUNG_REGISTRATION_CAP_EXEMPT_CIDRS` is the
lever if they do.

## Acceptance gate — run before advertising

```bash
go run ./cmd/frontcheck -url https://<endpoint>.z01.azurefd.net/
```

It checks, read-only, that the shipping transport suppresses SNI for the host;
that the no-SNI handshake satisfies the pinned rule (reporting the certificate
in full); that a signed relay list arrives and verifies under the pinned keys,
**over a connection whose ClientHello is confirmed to have carried no server
name**; that the served list was signed for this request rather than replayed
from a cache; that an ordinary SNI dial returns the same relay configuration,
proving both paths reach the same origin; and that an unroutable `Host` is not
served our origin.

Two of those deserve emphasis, because both catch failures nothing else here
would. The SNI observation is a measurement of the connection that actually
carried the signed list, not an inference from configuration. And the freshness
bound catches a front that caches the relay list: a cached body still verifies,
since the signature covers a 30-minute window plus five minutes of skew, so a
route with caching left on would otherwise pass every other check. A Cloudflare
edge once served this deployment a stale `/api/v1/relays` for about four hours.

If the run fails only on the relay-configuration comparison and reports
*different relay sets*, the fleet most likely changed between the two fetches —
re-run. A mismatch reported as *the same relays with a different configuration*
is the serious one.

**Run it from an unproxied shell.** It refuses to start when `HTTPS_PROXY` or
`ALL_PROXY` selects a proxy for the candidate, because Go tunnels proxied HTTPS
with `CONNECT` and then performs its own SNI-bearing handshake — the no-SNI
dialer never runs, so a run behind a proxy would report on a path clients do not
take. `NO_PROXY` exemptions are honoured. This is also worth remembering about
the shipping client: a user behind a proxy sends the front's name in the
ClientHello, which no front-side change can prevent.

Every check must pass. Record the printed certificate details in the pull
request that advertises the endpoint.

## Advertising — done for this endpoint

Recorded so a replacement endpoint follows the same path. Done on 2026-08-05
after the gate passed:

1. `azureBrokerHost` / `AzureBrokerURL` added in `brokerapi/types.go`, appended
   **last** in `DefaultBrokerURLs()`. Last is not cosmetic — this front proves
   only that the peer is an Azure edge, so it must not displace a front that
   proves it is ours. `TestAzureFrontIsRacedLast` holds that.
2. `TestAzureFrontIsNotYetAdvertised` replaced by
   `TestBrokerAzureConstantsStayLinked`, which asserts the shipped endpoint is
   recognized by the no-SNI recognizer — a name the recognizer missed would be
   dialed *with* SNI and silently leak.
3. `brokerapi/VERSION` → 0.4.0.
4. Still outstanding: the mobile repositories must pin `brokerapi/v0.4.0` and
   mirror the addition in their own `AppConfig`.

Re-run `frontcheck` after any endpoint, CDN, or certificate change.

### Gate result, 2026-08-05

```
PASS  transport TLS policy for this host
        SNI suppressed — Azure Front Door: certificate must carry the SAN *.azureedge.net
PASS  handshake completes and satisfies that policy
        TLS 1.3, ALPN ""
        leaf subject "*.azureedge.net", issuer "Microsoft TLS G2 ECC CA OCSP 06"
        leaf SANs [*.azureedge.net]
        chain depth 4, leaf expires 2026-11-14T10:49:57Z
PASS  signed relay list fetches and verifies over the shipping path
        4180 bytes, signature verified under pinned key 627405615601c589
        server_time is 0s old, so it was signed for this request
        carried over TLS with ClientHello server name ""
PASS  SNI-bearing control serves the same signed list
        matches the no-SNI response
PASS  edge routes on the Host header, not the address
        Host "frontcheck-unroutable.invalid" got HTTP 404 and no relay signature
```

Azure assigned a **z02** endpoint, not the z01 seen in every earlier
measurement. The recognizer matches the zone by shape rather than by literal, so
this needed no code change; a hardcoded `z01` would have fallen through to
SNI-bearing TLS and leaked the endpoint name.

First propagation took about 12 minutes, during which the endpoint returned
Front Door's generic 404 on both the no-SNI and SNI paths equally. That is
expected for a new profile and is not an SNI problem — if only the no-SNI path
404s, that is a different fault.

**Wait for consistent 200s before running the gate.** Propagation does not flip
cleanly: a new endpoint spends several minutes returning an intermittent mix of
404 and 200 *from the same edge address*, because servers behind that anycast IP
pick the new config up at different times. Running `frontcheck` in that window
produces a confusing partial failure — the handshake and Host-routing checks
pass while the two relay fetches 404 — which looks like a routing
misconfiguration and is not. Poll until the relay path returns 200 steadily
(a dozen consecutive successes is a reasonable bar), then run the gate:

```bash
until curl -sf -o /dev/null "https://<endpoint>/api/v1/relays?limit=5"; do sleep 10; done
```
