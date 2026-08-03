# Azure Front Door as broker front #3

Status: **client support implemented, not yet advertised.** No Azure
subscription or Front Door endpoint exists for this project, so nothing is
provisioned and no endpoint appears in the built-in discovery order. The client
code path is complete and tested; wiring an endpoint in is the remaining step,
and it is gated on the acceptance check below.

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

### Known blocker: sponsorship subscriptions cannot create a profile

Attempted 2026-08-04 on subscription `2ac42581-…` (offer `Sponsored_2016-01-01`,
the nonprofit grant). Profile creation fails:

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

## Advertising the endpoint

Only after the gate passes:

1. Add the endpoint constant and URL to `brokerapi/types.go`, appending it
   **last** in `DefaultBrokerURLs()`.
2. Delete `TestAzureFrontIsNotYetAdvertised` in `brokerapi/azure_tls_test.go`,
   which exists to catch exactly this being done prematurely.
3. Bump `brokerapi/VERSION`.
4. Mirror the addition in the mobile clients' `AppConfig`, which keep their own
   copy of the discovery order.

Re-run `frontcheck` after any endpoint, CDN, or certificate change.
