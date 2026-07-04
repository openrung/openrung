// Cloudflare Worker: TLS-terminating reverse proxy that fronts the OpenRung broker.
//
// Clients reach https://broker.openrung.org/...  Cloudflare terminates TLS at the edge
// (real cert for the hostname) and this Worker forwards each request to the broker origin,
// which speaks plaintext HTTP on a non-standard port. This gives China clients an HTTPS,
// CDN-fronted discovery endpoint whose blocking incurs Cloudflare-edge collateral damage,
// instead of a single null-routable plaintext IP. The raw origin IP stays in the app's
// broker fallback list, so a blocked edge degrades to the direct IP rather than failing.
//
// The broker is a stateless JSON control-plane API (relay discovery, telemetry, speed-test).
// Responses MUST NOT be cached — relay candidates are short-lived (≈3 min lease).
//
// The origin MUST be a hostname, not a bare IP: Cloudflare Workers cannot fetch() an IP literal
// (e.g. http://54.238.185.205:8080) — that returns Cloudflare error 1003 "Direct IP Access Not
// Allowed". broker-origin.openrung.org is a DNS-only (grey-cloud) A record → 54.238.185.205, so
// the Worker resolves the name and connects straight to the AWS origin (bypassing Cloudflare's
// proxy, which avoids a loop). It must stay DNS-only; proxying it would loop back into the edge.
//
// Caveat: the Worker → origin leg is plaintext HTTP over the public internet (the origin has no
// TLS cert). The censorship-relevant leg (client → Cloudflare) is encrypted; hardening the origin
// leg (Origin CA cert + Full(strict), or Cloudflare Tunnel, or an IP allowlist that only admits
// Cloudflare egress) is a follow-up. See README.md.

const ORIGIN = "http://broker-origin.openrung.org:8080";

export default {
  async fetch(request) {
    const target = new URL(request.url);
    const origin = new URL(ORIGIN);

    // Preserve path + query; swap scheme/host/port to the origin.
    target.protocol = origin.protocol;
    target.hostname = origin.hostname;
    target.port = origin.port;

    const proxied = new Request(target, request);

    // Surface the real client IP to the origin. The broker only reads RemoteAddr today (so it
    // currently sees Cloudflare's IP), but this future-proofs it for X-Forwarded-For parsing and
    // keeps the client_seen telemetry meaningful once the broker honors the header.
    const clientIp = request.headers.get("CF-Connecting-IP");
    if (clientIp) {
      proxied.headers.set("X-Forwarded-For", clientIp);
    }
    proxied.headers.set("X-Forwarded-Proto", "https");

    // Never cache control-plane responses; relay candidates are short-lived.
    return fetch(proxied, { cf: { cacheTtl: 0, cacheEverything: false } });
  },
};
