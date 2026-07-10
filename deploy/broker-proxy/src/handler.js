// Core request-handling logic for the broker proxy Worker, factored out of src/index.js so it
// can be unit-tested under Node with injected fetch/cache fakes (see test/handler.test.mjs).
//
// Behavior:
//   - GET /api/v1/relays (exact path): proxied to the origin with a 10 s timeout. Every 2xx
//     response is returned to the client unchanged AND a copy is stored in this colo's Cache API
//     for 180 s. If the origin times out, errors at the network layer, or returns >= 500, the
//     stored copy is served with `X-OpenRung-Stale: 1` instead of the failure. 3xx/4xx responses
//     (404, 429 back-pressure) are semantic answers and pass through unmasked. A network error
//     with a cold cache returns a JSON 502 (previously the exception bubbled up as an opaque
//     Cloudflare error page).
//   - Everything else: byte-for-byte passthrough with fetch-layer caching disabled and NO
//     timeout (the speed-test endpoint deliberately holds the connection open).

export const DEFAULT_ORIGIN = "http://broker-origin.openrung.org:8080";

const RELAYS_PATH = "/api/v1/relays";
const ORIGIN_TIMEOUT_MS = 10_000;

// How long a stale copy may be served after the last healthy response. Relay leases are ~3 min,
// and every client validates `expires_at` against the `server_time` carried in the SAME response
// body — so a stale cached list passes client-side expiry checks self-consistently and the
// client CANNOT detect the staleness. The edge must bound it instead: 180 s keeps a served-stale
// list within roughly one lease of reality.
const STALE_TTL_SECONDS = 180;

// Never let Cloudflare's fetch layer cache origin responses. The Cache API copy above is the
// only cache, and it is only ever READ on origin failure — the freshness path always hits the
// origin.
const NO_FETCH_CACHE = { cacheTtl: 0, cacheEverything: false };

// Factory so tests can inject a fake fetch and cache. `fetchImpl(request, init)` must behave
// like global fetch; `cache` must expose the Cache API's put/match.
export function createHandler({ fetchImpl, cache }) {
  return async function handle(request, env, ctx) {
    const originBase = env && env.ORIGIN ? env.ORIGIN : DEFAULT_ORIGIN;
    const url = new URL(request.url);
    const proxied = buildProxiedRequest(request, url, originBase);

    if (request.method === "GET" && url.pathname === RELAYS_PATH) {
      return relaysWithStaleFallback(proxied, request.url, ctx, fetchImpl, cache);
    }

    // All other paths/methods: passthrough exactly as before. No timeout — the speed-test
    // endpoint is long-lived by design.
    return fetchImpl(proxied, { cf: NO_FETCH_CACHE });
  };
}

function buildProxiedRequest(request, url, originBase) {
  const origin = new URL(originBase);
  const target = new URL(url);

  // Preserve path + query; swap scheme/host/port to the origin.
  target.protocol = origin.protocol;
  target.hostname = origin.hostname;
  target.port = origin.port;

  const proxied = new Request(target, request);

  // Surface the real client IP to the origin. The broker honors X-Forwarded-For only when the
  // request arrives from a trusted proxy (Cloudflare egress ranges), so this cannot be spoofed
  // by direct origin hits.
  const clientIp = request.headers.get("CF-Connecting-IP");
  if (clientIp) {
    proxied.headers.set("X-Forwarded-For", clientIp);
  }
  proxied.headers.set("X-Forwarded-Proto", "https");

  return proxied;
}

async function relaysWithStaleFallback(proxied, requestUrl, ctx, fetchImpl, cache) {
  // Cache key: a bare GET on the full client URL. Query string included — `limit` etc. vary
  // the response body.
  const cacheKey = new Request(requestUrl, { method: "GET" });

  let originResponse = null;
  try {
    originResponse = await fetchImpl(proxied, {
      cf: NO_FETCH_CACHE,
      signal: AbortSignal.timeout(ORIGIN_TIMEOUT_MS),
    });
  } catch {
    // Network error or 10 s timeout — fall through to the stale path below.
  }

  if (originResponse !== null && originResponse.ok) {
    // Healthy: hand the origin response to the client unchanged and stash a copy for later.
    // The stored copy must override the origin's `no-store` (the Cache API refuses to store a
    // no-store response) with a hard 180 s cap; it is only ever read on origin failure.
    const copy = originResponse.clone();
    const headers = new Headers({
      "Cache-Control": `public, s-maxage=${STALE_TTL_SECONDS}`,
    });
    const contentType = copy.headers.get("Content-Type");
    if (contentType) {
      headers.set("Content-Type", contentType);
    }
    ctx.waitUntil(cache.put(cacheKey, new Response(copy.body, { status: 200, headers })));
    return originResponse;
  }

  if (originResponse !== null && originResponse.status < 500) {
    // 3xx/4xx are semantic answers (404, 429 back-pressure): never mask them with stale data.
    return originResponse;
  }

  // Origin failed (network error, timeout, or 5xx): serve the last healthy list if this colo
  // still holds one.
  const cached = await cache.match(cacheKey);
  if (cached) {
    const headers = new Headers({
      "X-OpenRung-Stale": "1",
      "Cache-Control": "no-store",
    });
    const contentType = cached.headers.get("Content-Type");
    if (contentType) {
      headers.set("Content-Type", contentType);
    }
    return new Response(cached.body, { status: 200, headers });
  }

  if (originResponse !== null) {
    // 5xx with a cold cache: nothing better to offer — pass the origin error through.
    return originResponse;
  }

  // Network error with a cold cache: return a real JSON 502 instead of letting the exception
  // surface as an opaque Cloudflare error page.
  return new Response(JSON.stringify({ error: "broker origin unreachable" }), {
    status: 502,
    headers: {
      "Content-Type": "application/json",
      "Cache-Control": "no-store",
    },
  });
}
