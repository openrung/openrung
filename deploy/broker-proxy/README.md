# Broker CDN front (Cloudflare Worker)

`broker.openrung.org` is a TLS-terminating Cloudflare Worker that reverse-proxies the OpenRung
broker. It gives China clients an **HTTPS, CDN-fronted** relay-discovery endpoint instead of a
single plaintext IP that a one-line ACL can null-route.

```
China client ──HTTPS──► Cloudflare edge ──Worker──HTTP:8080──► broker-origin.openrung.org ──► 54.238.185.205
```

- The broker is a stateless JSON control-plane API; the Worker just forwards bytes and forces
  no-caching (relay candidates are short-lived).
- The raw origin IP remains in the apps' broker fallback list
  (`AppConfig.DEFAULT_BROKER_URLS` / `AppConfig.defaultBrokerURLs`), so a blocked edge degrades to
  the direct IP rather than failing.

## Origin must be a hostname, not an IP (important)

Cloudflare Workers **cannot `fetch()` a bare IP literal** — `http://54.238.185.205:8080` returns
Cloudflare error **1003 "Direct IP Access Not Allowed"**, which the Worker passes straight through
(you'll see 1003 on *both* the custom domain and any workers.dev URL). The Worker therefore targets
**`broker-origin.openrung.org`**, a **DNS-only (grey-cloud) A record → 54.238.185.205** in the
zone. It must stay DNS-only; proxying (orange-cloud) it would loop the subrequest back into the
edge. If you ever change the origin IP, update that DNS record (not the Worker).

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
  domain fronting is dead). This raises cost (the hostname must be discovered first, and the
  fallback IP still works) but is not absolute. Encrypted Client Hello (ECH) is a future lever.
