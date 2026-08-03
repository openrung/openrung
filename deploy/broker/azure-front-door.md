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

Requires an Azure subscription. The Microsoft for Nonprofits grant has not been
applied for yet; account creation and payment details are the maintainer's to
enter.

1. Create a Front Door **Standard** profile (Premium's WAF is not needed; a
   profile allows 3000 concurrent connections and caps connections at 2 hours).
2. Add an endpoint. Azure assigns `<name>-<hash>.z01.azurefd.net`. Do **not**
   add a custom domain — see above.
3. Origin group: the broker origin `54.238.185.205:443`, HTTPS only, origin host
   header set to the origin's own TLS name. Front the origin directly, never
   another CDN front.
4. Route: match `/*`, forward to the origin group, **caching disabled**. The
   relay list must not be cached — a stale signed list is a live outage, and
   `/api/v1/relays` already sets `no-store` at the origin.
5. Leave health probes at their defaults.

Budget alerts only email; they do not stop spending. Set one, and keep the
runbook for deallocating if credit is exhausted.

## Acceptance gate — run before advertising

```bash
go run ./cmd/frontcheck -url https://<endpoint>.z01.azurefd.net/
```

It checks, read-only, that the shipping transport suppresses SNI for the host;
that the no-SNI handshake satisfies the pinned rule (reporting the certificate
in full); that a signed relay list arrives and verifies under the pinned keys;
that an ordinary SNI dial returns the same list, proving both paths reach the
same origin; and that an unroutable `Host` is not served our origin.

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
