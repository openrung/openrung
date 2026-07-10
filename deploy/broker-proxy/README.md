# Broker CDN front (Cloudflare Worker)

`broker.openrung.org` is a TLS-terminating Cloudflare Worker that reverse-proxies the OpenRung
broker. It gives China clients an **HTTPS, CDN-fronted** relay-discovery endpoint instead of a
single plaintext IP that a one-line ACL can null-route.

```
China client ──HTTPS──► Cloudflare edge ──Worker──HTTP:8080──► broker-origin.openrung.org ──► 54.238.185.205
```

- The broker is a stateless JSON control-plane API; the Worker forwards bytes and never serves
  cached data on the freshness path (relay candidates are short-lived). The one exception is
  stale-on-error for relay discovery — see below.
- This Cloudflare front is currently the **only** discovery endpoint the apps ship: every client's
  `DEFAULT_BROKER_URLS` / `defaultBrokerURLs` contains just this one HTTPS URL. There is **no raw-IP
  fallback** — the relay list is unsigned, so the clients refuse any non-HTTPS broker URL
  (`EnforceSecureBrokerURL`); a bare-IP/plaintext entry would let an on-path censor inject a
  malicious relay set. A blocked edge therefore fails discovery **closed** (offline). That single
  point of failure is what the front-diversity work (multiple HTTPS fronts across independent
  CDNs/domains) and relay-list signing are meant to remove.

## Origin must be a hostname, not an IP (important)

Cloudflare Workers **cannot `fetch()` a bare IP literal** — `http://54.238.185.205:8080` returns
Cloudflare error **1003 "Direct IP Access Not Allowed"**, which the Worker passes straight through
(you'll see 1003 on *both* the custom domain and any workers.dev URL). The Worker therefore targets
**`broker-origin.openrung.org`**, a **DNS-only (grey-cloud) A record → 54.238.185.205** in the
zone. It must stay DNS-only; proxying (orange-cloud) it would loop the subrequest back into the
edge. If you ever change the origin IP, update that DNS record (not the Worker).

## Stale-on-error (`GET /api/v1/relays`)

An origin blip must not take relay discovery down — this front is the only discovery path the
apps ship. For `GET /api/v1/relays` (exact path) only, the Worker:

- proxies to the origin with a **10 s timeout**;
- on a **2xx**, returns the origin response to the client unchanged and stores a copy in the
  colo's Cache API with `Cache-Control: public, s-maxage=180` (the origin's `no-store` still
  reaches clients; the override applies only to this deliberately stored fallback copy);
- on **failure** (timeout, network error, or origin ≥ 500), serves that stored copy with
  **`X-OpenRung-Stale: 1`** and `Cache-Control: no-store`;
- on failure with a **cold cache**, passes an origin 5xx through, and turns a network error into
  a JSON `502` (previously the exception surfaced as an opaque Cloudflare error page);
- passes **4xx through unmasked** — a `429`/`404` is a semantic answer, not an outage.

Notes:

- **The freshness path is unchanged.** Healthy responses always come straight from the origin.
  The cached copy is written on every success but only ever *read* on failure; no healthy
  response is served from cache.
- **Why 180 s.** Relay leases are ~3 min, and all clients validate `expires_at` against the
  `server_time` carried in the *same* response body — so a stale cached list passes client-side
  expiry checks self-consistently and clients cannot detect the staleness themselves. The edge
  must bound it: 180 s keeps a served-stale list within roughly one lease of reality.
- **Per-colo.** The Cache API is colo-local, so the stale copy exists only in Cloudflare colos
  that recently served a fresh response. A colo that never saw a healthy response has nothing to
  fall back on and returns the error as before.
- Degraded responses are detectable (by clients and in telemetry) via `X-OpenRung-Stale: 1`.
- All other paths and methods remain untouched passthrough with **no timeout** (the speed-test
  endpoint is long-lived by design).

Unit tests: `npm test` (Node's built-in runner) from this directory covers the fresh, stale,
cold-cache, 4xx-unmasked, and passthrough paths with injected fetch/cache fakes.

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

- **Origin leg is plaintext HTTP.** The censorship-relevant leg (client → Cloudflare) is
  encrypted, but Cloudflare → origin is HTTP over the public internet. Harden by giving the origin
  a Cloudflare Origin CA cert and switching SSL/TLS mode to Full (strict), by fronting via a
  Cloudflare Tunnel (no public origin port), or by firewalling the origin to Cloudflare egress
  ranges only.
- **Client IP recovery (done).** The Worker forwards the real client IP as `X-Forwarded-For`, and
  the broker now honors `CF-Connecting-IP` / `X-Forwarded-For` **only when the request arrives from a
  trusted proxy** (Cloudflare's published ranges by default; extend via `OPENRUNG_TRUSTED_PROXY_CIDRS`).
  A direct hit on the raw origin port is not trusted, so it cannot spoof the source IP. Residual: a
  request routed through *any* Cloudflare Worker could still forge the header, since the origin port
  is open — low stakes for `client_seen` analytics; close it off with Authenticated Origin Pulls or a
  shared-secret header if that ever matters.
- **SNI blocking.** A determined censor can SNI-block `broker.openrung.org` specifically (classic
  domain fronting is dead). Because this is currently the only front and there is no raw-IP fallback
  (see above), an SNI block takes discovery fully offline. Mitigations, in order of leverage:
  additional HTTPS fronts on other CDNs/domains (so one SNI rule no longer suffices), Encrypted
  Client Hello (ECH) to hide the SNI, and — to unlock non-TLS / out-of-band channels — signing the
  relay list so a fetched directory is trustworthy regardless of the channel that carried it.
