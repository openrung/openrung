# Broker CDN front (Cloudflare Worker)

`broker.openrung.org` is a TLS-terminating Cloudflare Worker that reverse-proxies the OpenRung
broker. It gives clients on censored networks an **HTTPS, CDN-fronted** control-plane endpoint
instead of a single plaintext IP that a one-line ACL can null-route.

```
censored client ──HTTPS──► Cloudflare edge ──Worker──HTTPS:443──► broker-origin.openrung.org ──► 54.238.185.205
```

- The broker is a stateless JSON control-plane API; the Worker forwards bytes and never serves
  cached data on the freshness path (relay candidates are short-lived). The one exception is
  stale-on-error for relay discovery — see below.
- This Cloudflare front is one of **two independent HTTPS fronts** the apps ship: every client's
  `DEFAULT_BROKER_URLS` / `defaultBrokerURLs` races this URL against an AWS CloudFront
  distribution on a different provider and DNS zone, so a blocked or dead edge here no longer
  fails discovery closed. The relay list is **Ed25519-signed** end-to-end, detaching its
  authenticity from the transport. There is still **no raw-IP fallback**: discovery runs before
  the tunnel and carries the client-identity headers, so the clients refuse any non-HTTPS broker
  URL (`EnforceSecureBrokerURL`).

## Origin must be a hostname, not an IP (important)

Cloudflare Workers **cannot `fetch()` a bare IP literal** — `http://54.238.185.205:8080` returns
Cloudflare error **1003 "Direct IP Access Not Allowed"**, which the Worker passes straight through
(you'll see 1003 on *both* the custom domain and any workers.dev URL). The Worker therefore targets
**`broker-origin.openrung.org`**, a **DNS-only (grey-cloud) A record → 54.238.185.205** in the
zone. It must stay DNS-only; proxying (orange-cloud) it would loop the subrequest back into the
edge. If you ever change the origin IP, update that DNS record (not the Worker).

## Stale-on-error (`GET /api/v1/relays`)

An origin blip must not take relay discovery down — both deployed fronts proxy the same single
origin, so origin trouble hits them together. For `GET /api/v1/relays` (exact path) only, the
Worker:

- proxies to the origin with a **10 s timeout**;
- on a **200**, returns the origin response to the client unchanged and stores a complete copy —
  body **and all headers** — in the colo's Cache API with `Cache-Control: public, max-age=900`
  (the origin's `no-store` still reaches clients; the override applies only to this deliberately
  stored fallback copy). Only a full `200` is stored: a `206` partial body would poison the
  fallback, so any other 2xx passes through to the client unchanged but is never cached;
- on **failure** (timeout, network error, or origin ≥ 500), serves that stored copy — original
  headers intact — with **`X-OpenRung-Stale: 1`** and `Cache-Control: no-store, no-transform`;
- on failure with a **cold cache**, passes an origin 5xx through, and turns a network error into
  a JSON `502` (previously the exception surfaced as an opaque Cloudflare error page);
- passes **4xx through unmasked** — a `429`/`404` is a semantic answer, not an outage.

Notes:

- **The freshness path is unchanged.** Healthy responses always come straight from the origin.
  The cached copy is written on every success but only ever *read* on failure; no healthy
  response is served from cache.
- **Why 900 s (15 min).** All clients validate relay `expires_at` against the `server_time`
  carried in the *same* response body — a stale cached list passes client-side expiry checks
  self-consistently, so client-side expiry checks cannot bound edge staleness either way. The
  bound has to live at the edge, and the relay-list signing spec fixes it: a **15-minute stale
  window** inside a 30-minute signed `not_after`. The edge cap is 900 s to match.
- **Signing-ready.** The stored copy preserves *all* origin headers (only `Cache-Control` is
  overridden and `Set-Cookie` dropped), so the upcoming `X-OpenRung-Relays-Signature` — computed
  over the exact body bytes — survives a stale serve intact; signature-requiring clients would
  reject a stale body served without it. The cache key is version-namespaced
  (`__or_cache_v=1`); bump it to `2` when the signing broker deploys so pre-signing headerless
  bodies can never replay to signature-requiring clients.
- **Per-colo.** The Cache API is colo-local, so the stale copy exists only in Cloudflare colos
  that recently served a fresh response. A colo that never saw a healthy response has nothing to
  fall back on and returns the error as before.
- Degraded responses are detectable (by clients and in telemetry) via `X-OpenRung-Stale: 1`.
- Paths other than the exact relay-list GET and WSS-ticket POST remain untouched passthrough with
  **no timeout** (the speed-test endpoint is long-lived by design).

## WSS ticket passthrough (`POST /api/v1/wss/tickets`)

WSS ticket acquisition is control-plane traffic. For this exact method and path, the Worker:

- forwards the JSON POST to the broker with a **10 s timeout** and `redirect: "manual"`, so an
  origin redirect is returned to the client rather than followed with the POST or identity
  headers;
- never uses stale relay-list data and never caches a success or error;
- forces `Cache-Control: no-store` and `Pragma: no-cache` while preserving the origin status,
  body, and semantic headers such as `Retry-After` and `Location`; and
- returns an uncached JSON `502` when the ticket origin times out or is unreachable.

Desktop clients require HTTPS, reject every redirect response, and try the next configured broker
front within one bounded ticket-acquisition deadline. They honor `Retry-After` only when it fits
both their configured maximum wait and the remaining deadline. The Worker neither issues the
ticket itself nor receives the subsequent WebSocket data: after validating the response against
the signed directory, the client connects to the selected relay's advertised WSS front and sends
the ticket only as `Authorization: Bearer` on `/api/v1/wss-bridge`. That CDN front's origin is the
same relay's local sidecar.

Unit tests: `npm test` (Node's built-in runner) from this directory covers the fresh, stale,
cold-cache, 4xx-unmasked, WSS-ticket timeout/redirect/cache behavior, and ordinary passthrough
paths with injected fetch/cache fakes.

## Deploy

From this directory, with wrangler authenticated against the `openrung.org` Cloudflare account:

```bash
wrangler deploy
```

`custom_domain: true` makes Cloudflare provision the proxied DNS record and edge certificate for
`broker.openrung.org` automatically. First deploy may take up to ~1 min for the cert to issue.

Verify:

```bash
curl -s "https://broker.openrung.org/api/v1/relays?limit=1" | head
curl -s -o /dev/null -w '%{http_code}\n' "https://broker.openrung.org/healthz"
```

Logs: `wrangler tail openrung-broker-proxy`.

## Known limitations / follow-ups

- **Origin leg uses HTTPS.** This Worker defaults to the same Caddy Let's
  Encrypt terminator on `:443` used by the independent AWS CloudFront broker
  front (see `deploy/broker/origin-tls.md`). Keep certificate validation
  enabled and firewall the origin to approved CDN egress ranges. The old
  `http://broker-origin.openrung.org:8080` endpoint must not be configured in
  production because successful ticket responses contain short-lived bearer
  credentials.
- **Client IP recovery (done).** The Worker forwards the real client IP as `X-Forwarded-For`, and
  the broker now honors `CF-Connecting-IP` / `X-Forwarded-For` **only when the request arrives from a
  trusted proxy** (Cloudflare's published ranges by default; extend via `OPENRUNG_TRUSTED_PROXY_CIDRS`).
  A direct hit on the raw origin port is not trusted, so it cannot spoof the source IP. Residual: a
  request routed through *any* Cloudflare Worker could still forge the header, since the origin port
  is open — low stakes for `client_seen` analytics; close it off with Authenticated Origin Pulls or a
  shared-secret header if that ever matters.
- **SNI blocking.** A determined censor can SNI-block `broker.openrung.org` specifically (classic
  domain fronting is dead). Two of the planned mitigations have shipped: the independent
  CloudFront second front means a single SNI rule no longer takes discovery offline, and the
  relay list is now Ed25519-signed, so a fetched directory is trustworthy regardless of the
  channel that carried it — which unlocks future non-TLS / out-of-band channels (signed static
  mirrors, a pinned direct-IP fallback). Encrypted Client Hello (ECH) to hide the SNI remains
  open.
