// Cloudflare Worker: TLS-terminating reverse proxy that fronts the OpenRung broker.
//
// Clients reach https://broker.openrung.org/...  Cloudflare terminates TLS at the edge
// (real cert for the hostname) and this Worker forwards each request to the broker origin,
// which speaks plaintext HTTP on a non-standard port. This gives China clients an HTTPS,
// CDN-fronted discovery endpoint whose blocking incurs Cloudflare-edge collateral damage,
// instead of a single null-routable plaintext IP.
//
// This front is currently the ONLY discovery endpoint the apps ship: clients are HTTPS-only
// (EnforceSecureBrokerURL) and carry no raw-IP fallback, so a blocked or dead front fails
// discovery CLOSED. The real fixes are front diversity (an independent second front, not
// same-zone) and relay-list signing — see README.md. What this Worker adds in the meantime is
// stale-on-error for GET /api/v1/relays: an origin blip (timeout, network error, 5xx) is
// answered with this colo's last healthy relay list (≤ 180 s old, marked X-OpenRung-Stale: 1)
// instead of an error.
//
// The freshness path is unchanged: healthy responses are never served from cache — relay
// candidates are short-lived (≈3 min lease) — and every other endpoint is a plain passthrough.
// See src/handler.js for the logic and test/ for the unit tests.
//
// The origin MUST be a hostname, not a bare IP: Cloudflare Workers cannot fetch() an IP literal
// (e.g. http://54.238.185.205:8080) — that returns Cloudflare error 1003 "Direct IP Access Not
// Allowed". broker-origin.openrung.org is a DNS-only (grey-cloud) A record → 54.238.185.205, so
// the Worker resolves the name and connects straight to the AWS origin (bypassing Cloudflare's
// proxy, which avoids a loop). It must stay DNS-only; proxying it would loop back into the edge.
// The origin is overridable via the ORIGIN var (wrangler dev --var ORIGIN:... / tests).
//
// Caveat: the Worker → origin leg is plaintext HTTP over the public internet (the origin has no
// TLS cert). The censorship-relevant leg (client → Cloudflare) is encrypted; hardening the origin
// leg (Origin CA cert + Full(strict), or Cloudflare Tunnel, or an IP allowlist that only admits
// Cloudflare egress) is a follow-up. See README.md.

import { createHandler } from "./handler.js";

export default {
  async fetch(request, env, ctx) {
    const handler = createHandler({
      fetchImpl: (req, init) => fetch(req, init),
      cache: caches.default,
    });
    return handler(request, env, ctx);
  },
};
