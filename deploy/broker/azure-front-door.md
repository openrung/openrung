# Azure Front Door as broker front #3

Status: **live and advertised.** Provisioned 2026-08-05 as
`cdn-edge-cxdnhsg2aadmaubj.z02.azurefd.net`, accepted by `cmd/frontcheck`, and
advertised **only as a last-resort discovery phase** after both
exact-endpoint-authenticated fronts fail. The endpoint prefix is generic on
purpose — see below; the first attempt used a project-identifying name and was
replaced before anything shipped.

Installed clients only gain this front when they update — the front list is
compiled in, and there is no server-side way to push one. Desktop picks it up on
its next release build; the mobile repositories must bump their pinned
`brokerapi` tag to **v0.4.0** and rebuild their AAR/XCFramework. Their native
bindings already call `BrokerCandidates`, `FirstReachable`, and
`RequestWSSTicket`, so the URL, two-phase discovery policy, and hard pre-HTTP
ticket refusal arrive with that rebuilt module. Mobile still owns ticket
ordering and retries; it should also filter Azure there as defense in depth and
to avoid a guaranteed failed attempt.

## Why Azure, and why SNI-less

The two existing fronts are a Cloudflare Worker (`broker.openrung.org`) and an
AWS CloudFront distribution. A third independent provider means a single CDN,
DNS zone, or account failure cannot fail discovery closed.

Azure is a control-plane front only. Relay data must never move through it: the
nonprofit grant buys roughly 24 TB/year, about 29 days of fleet traffic, and the
subscription converts to pay-as-you-go rather than stopping when credit runs
out.

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
That is survivable for relay discovery because lists are Ed25519-signed and
verified against pinned keys, with a `not_after` bound that also defeats replay.
It is not sufficient for every broker response: an impersonating front would
still see client identity headers and telemetry, could refuse to serve, and
could steal a WSS bearer ticket by forwarding the request to the real broker.
`RequestWSSTicket` therefore rejects this front before sending HTTP, and desktop
ticket failover filters it as defense in depth.

Discovery enforces the weaker trust boundary structurally rather than relying
only on list position. Cloudflare and CloudFront race in the first phase; Azure
does not start when their stagger elapses and is attempted only after both have
failed. A merely slow response cannot silently downgrade discovery.

A custom domain does not help. Without SNI the edge serves the shared
certificate regardless of the Host, so a custom domain would be no better
authenticated while losing the ordinary verification it gets by keeping SNI.

## Provisioning

```bash
OPENRUNG_AZURE_ORIGIN_AUTH_FILE=/path/to/origin-auth bash deploy/broker/azure-front-door-up.sh
```

The file holds the origin-auth secret (see
[Client IP behind this front](#client-ip-behind-this-front)); install the same
value on the origin first.

The script is convergent and fail-closed, not merely create-if-missing. On every
run it checks the origin, requires the existing profile to have the Standard
SKU, reconciles the endpoint state, health probe, load-balancing, origin TLS,
and route protocol settings, and then reads the effective configuration back.
It verifies the origin host and host header, HTTPS port, certificate-name check,
enabled states, probe fields, sole origin, route origin group, HTTPS-only input
and forwarding, `/*` match, default-domain link, and sole endpoint, origin
group, origin, and route.

Some route state is deliberately not removed automatically. If an existing
route has a cache configuration, custom-domain attachment, origin path, or any
rule set other than the origin-auth one, the script exits with the offending
value before updating that route. The profile must contain only that rule set,
and the rule set only its single unconditional header rule.
Those additions can change request routing or re-enable caching, and removing
them may detach operator-created resources; inspect and remove them deliberately
in Azure, then rerun. It also requires the dedicated profile to contain no
custom-domain resources at all. A successful run therefore means all asserted
properties matched after reconciliation—not just that resources with the right
names existed.

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
6. Rule set `originauth` with one rule, `setoriginauth`: no conditions, one
   action that overwrites the `X-OpenRung-Azure-Auth` request header with the
   origin-auth secret. It is the route's only rule set. The script writes and
   reads it through ARM (`az rest`) because the route's rule-set flag differs
   between the core Azure CLI and the `cdn` extension.

Rerun `bash deploy/broker/azure-front-door-up.sh` after any portal or CLI
change. The script repairs drift in the ordinary mutable fields above and fails
on incompatible attachments or any value Azure did not apply as requested.

Budget alerts only email; they do not stop spending. Set one, and keep the
runbook for deallocating if credit is exhausted.

### The endpoint name must not identify this project

Suppressing SNI keeps the endpoint name out of the ClientHello, but the client
resolves it over **ordinary cleartext DNS** — `brokerapi` installs no custom
resolver and there is no DoH. So the hostname is the one part of this front a
passive on-path observer still sees, and the name we choose is the whole of what
they learn.

The prefix is therefore deliberately generic (`cdn-edge`), matching how CloudFront's
`d2r7mdpyevvs1m.cloudfront.net` reveals nothing. Azure appends an unguessable
suffix, so a boring prefix costs nothing. The resource group and profile names
are never on the wire and stay descriptive.

Endpoint names are **immutable** — changing one means creating a new endpoint
with its own route and verifying it before clients move. The provisioning script
intentionally requires one endpoint per dedicated profile and rejects a changed
`OPENRUNG_AZURE_ENDPOINT` before creating anything. For a staged replacement,
provision a fresh profile (and, if convenient, resource group), run the gate,
ship the new URL, and retain the old profile until clients using its compiled-in
URL can be retired. Then delete the old profile deliberately.

### Client IP behind this front

Front Door reports the address of the TCP connection the request arrived on in
`X-Azure-SocketIP`. That is the value used. `X-Azure-ClientIP` is not, because
a caller can influence it, and neither is `X-Forwarded-For`, to which Front Door
appends. The origin is also reachable directly, so
[`Caddyfile`](./Caddyfile) trusts `X-Azure-SocketIP` only on requests carrying
the origin-auth secret that the route's rule set writes into
`X-OpenRung-Azure-Auth`. Those requests reach the broker with that address as
`X-Forwarded-For`. Everything else, including Front Door's health probes, keeps
the immediate-peer address. The CloudFront front follows the same pattern; see
[origin TLS](origin-tls.md).

Rollout order, each step safe on its own:

1. Add `OPENRUNG_AZURE_ORIGIN_AUTH` to `/etc/caddy/origin-auth.env`, install the
   Caddyfile, validate and reload Caddy (see [origin TLS](origin-tls.md)).
   Nothing changes yet: Front Door does not send the header.
2. Run `azure-front-door-up.sh` with `OPENRUNG_AZURE_ORIGIN_AUTH_FILE` pointing
   at a private file holding the same value.
3. Verify that the broker records the viewer's address for a request through
   the front, while a direct request to the origin that copies the header
   names keeps its own peer address.

To rotate, update the env file and reload Caddy, then rerun the script with the
new file. To roll back, detach and delete the `originauth` rule set; requests
then fall back to the immediate-peer address.

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
route with caching left on would otherwise pass every other check.

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
   last in `DefaultBrokerURLs()`. More importantly, `FirstReachable` classifies
   native Azure endpoints as endpoint-unbound and does not start their phase
   until every exact-endpoint candidate has failed. The strict-phase tests in
   `brokerapi/client_test.go` hold that boundary independently of list order;
   `TestAzureFrontRemainsLastInDefaultOrder` also preserves the stable default
   preference.
2. `TestAzureFrontIsNotYetAdvertised` replaced by
   `TestBrokerAzureConstantsStayLinked`, which asserts the shipped endpoint is
   recognized by the no-SNI recognizer — a name the recognizer missed would be
   dialed *with* SNI and silently leak.
3. `brokerapi/VERSION` → 0.4.0.
4. `RequestWSSTicket` rejects endpoint-unbound fronts before HTTP, and desktop
   ticket orchestration filters them as defense in depth. This is required
   because the signed directory authenticates relay lists but cannot make a
   bearer response confidential from an impersonating Azure edge.
5. Still outstanding: the mobile repositories must pin `brokerapi/v0.4.0` and
   rebuild their native bindings. Their discovery binding already delegates to
   `BrokerCandidates` / `FirstReachable`, and their per-attempt ticket binding
   already delegates to `RequestWSSTicket`; no discovery URL or phase should be
   duplicated in platform `AppConfig`. Their Swift/Kotlin ticket-front builders
   should nevertheless filter an Azure winning front as defense in depth and
   keep the platform-owned default ticket lists endpoint-authenticated only.

Re-run `frontcheck` after any endpoint, CDN, or certificate change.

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
