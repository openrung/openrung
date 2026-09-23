// Core request-handling logic for the broker proxy Worker, factored out of src/index.js so it
// can be unit-tested under Node with injected fetch/cache fakes (see test/handler.test.mjs).
//
// Behavior:
//   - GET /api/v1/relays (exact path): proxied to the origin with a 10 s timeout. A 200 response
//     is returned to the client unchanged AND a complete copy (body + ALL headers) is stored in
//     this colo's Cache API for 900 s. If the origin times out, errors at the network layer, or
//     returns >= 500, the stored copy is served with `X-OpenRung-Stale: 1` instead of the
//     failure. Non-200 2xx (a 206 partial body would poison the fallback), 3xx, and 4xx
//     responses (404, 429 back-pressure) are semantic answers and pass through unmasked, never
//     stored. A network error with a cold cache returns a JSON 502 (previously the exception
//     bubbled up as an opaque Cloudflare error page).
//   - Everything else: byte-for-byte passthrough with fetch-layer caching disabled and NO
//     timeout (the speed-test endpoint deliberately holds the connection open).

// Keep the ticket-bearing control-plane leg encrypted all the way to the
// broker origin. Port 8080 remains available only as an explicit development
// override; it must never be the production default.
export const DEFAULT_ORIGIN = "https://broker-origin.openrung.org";

const RELAYS_PATH = "/api/v1/relays";
const WSS_TICKETS_PATH = "/api/v1/wss/tickets";
const ORIGIN_TIMEOUT_MS = 10_000;
const WSS_TICKET_TIMEOUT_MS = 10_000;

// How long a stale copy may be served after the last healthy 200. Every client validates relay
// `expires_at` against the `server_time` carried in the SAME response body, so client-side
// expiry checks are self-consistent and CANNOT bound edge staleness either way — the bound has
// to live at the edge. The relay-list signing spec fixes the stale window at 15 minutes inside
// a 30-minute signed `not_after`, so the edge cap is 900 s to match. Exported so the tests can
// couple the header value and the TTL bound to this one constant.
export const STALE_TTL_SECONDS = 900;

// Version namespace for stored fallback copies, appended to the cache-key URL as
// `__or_cache_v`. Bump to 2 when the signing broker deploys, so pre-signing headerless bodies
// can never replay to signature-requiring clients.
const CACHE_KEY_VERSION = "1";

// Never let Cloudflare's fetch layer cache origin responses. The Cache API copy above is the
// only cache, and it is only ever READ on origin failure — the freshness path always hits the
// origin.
const NO_FETCH_CACHE = { cacheTtl: 0, cacheEverything: false };

// Source deny list ---------------------------------------------------------------------------
// Cloudflare sets CF-Connecting-IP at the edge and overwrites any client-supplied copy, so it is
// the one client address this Worker can trust. Prefixes come from the DENY_CIDRS binding
// (comma-separated CIDRs, IPv4 or IPv6; blank and unparseable entries are skipped). A denied
// request is answered here, so the origin never sees it and it consumes no origin budget.
//
// The same constraint as the broker's own list applies: this matches ONE address per request, so
// only prefixes known to be a single caller belong in it — a prefix covering a shared address
// would refuse every client behind it.

// Parsed prefixes are memoized against the raw binding string: the binding is fixed for the
// lifetime of an isolate, so this parses once rather than per request.
let denyCache = { raw: null, prefixes: [] };

export function denyPrefixes(env) {
  const raw = env && typeof env.DENY_CIDRS === "string" ? env.DENY_CIDRS : "";
  if (raw !== denyCache.raw) {
    denyCache = { raw, prefixes: parseDenyCIDRs(raw) };
  }
  return denyCache.prefixes;
}

// parseDenyCIDRs turns the binding string into {bytes, bits} prefixes, canonicalized by masking
// off the host bits so a sloppy entry ("198.51.100.7/24") still matches its whole network.
export function parseDenyCIDRs(raw) {
  const prefixes = [];
  for (const entry of String(raw || "").split(",")) {
    const text = entry.trim();
    if (text === "") continue;
    const slash = text.lastIndexOf("/");
    const addrText = slash === -1 ? text : text.slice(0, slash);
    const bytes = ipToBytes(addrText);
    if (!bytes) continue;
    const maxBits = bytes.length * 8;
    let bits = maxBits;
    if (slash !== -1) {
      const bitsText = text.slice(slash + 1);
      if (!/^\d{1,3}$/.test(bitsText)) continue;
      bits = Number(bitsText);
      if (bits > maxBits) continue;
    }
    prefixes.push({ bytes: maskBytes(bytes, bits), bits });
  }
  return prefixes;
}

// isDeniedSource reports whether ip falls inside any prefix. An absent or unparseable address is
// never denied: the conservative direction, since a wrong match cuts off a real client.
export function isDeniedSource(ip, prefixes) {
  if (!prefixes || prefixes.length === 0) return false;
  const addr = ipToBytes(ip);
  if (!addr) return false;
  return prefixes.some((prefix) => containsAddress(prefix, addr));
}

function containsAddress(prefix, addr) {
  // Families never mix: an IPv4 prefix cannot contain an IPv6 address or vice versa.
  if (prefix.bytes.length !== addr.length) return false;
  const whole = prefix.bits >> 3;
  for (let i = 0; i < whole; i++) {
    if (prefix.bytes[i] !== addr[i]) return false;
  }
  const remainder = prefix.bits & 7;
  if (remainder === 0) return true;
  const mask = 0xff << (8 - remainder);
  return (prefix.bytes[whole] & mask) === (addr[whole] & mask);
}

function maskBytes(bytes, bits) {
  const masked = new Uint8Array(bytes);
  for (let i = 0; i < masked.length; i++) {
    const bitsBefore = i * 8;
    if (bitsBefore >= bits) {
      masked[i] = 0;
    } else if (bits - bitsBefore < 8) {
      masked[i] &= 0xff << (8 - (bits - bitsBefore));
    }
  }
  return masked;
}

// ipToBytes returns 4 bytes for IPv4 and 16 for IPv6, or null. An IPv4-mapped IPv6 address
// (::ffff:a.b.c.d) collapses to its 4 IPv4 bytes so an IPv4 prefix matches it either way.
function ipToBytes(ip) {
  if (typeof ip !== "string") return null;
  const text = ip.trim();
  if (text === "") return null;
  return text.includes(":") ? ipv6ToBytes(text) : ipv4ToBytes(text);
}

function ipv4ToBytes(text) {
  const parts = text.split(".");
  if (parts.length !== 4) return null;
  const bytes = new Uint8Array(4);
  for (let i = 0; i < 4; i++) {
    const part = parts[i];
    // No leading zeros: "010" is ambiguous (octal in some parsers) and never canonical.
    if (!/^\d{1,3}$/.test(part) || (part.length > 1 && part[0] === "0")) return null;
    const value = Number(part);
    if (value > 255) return null;
    bytes[i] = value;
  }
  return bytes;
}

function ipv6ToBytes(text) {
  const halves = text.split("::");
  if (halves.length > 2) return null;
  const head = parseGroups(halves[0], halves.length === 1);
  const tail = halves.length === 2 ? parseGroups(halves[1], true) : [];
  if (head === null || tail === null) return null;

  let groups;
  if (halves.length === 2) {
    const fill = 8 - head.length - tail.length;
    if (fill < 1) return null; // "::" must stand for at least one zero group
    groups = [...head, ...new Array(fill).fill(0), ...tail];
  } else {
    groups = head;
  }
  if (groups.length !== 8) return null;

  const bytes = new Uint8Array(16);
  for (let i = 0; i < 8; i++) {
    bytes[i * 2] = (groups[i] >> 8) & 0xff;
    bytes[i * 2 + 1] = groups[i] & 0xff;
  }
  return isV4Mapped(bytes) ? bytes.slice(12) : bytes;
}

// parseGroups reads one side of a "::" into 16-bit groups. A trailing dotted-quad (the
// ::ffff:a.b.c.d form) is allowed only in the last position, where it contributes two groups.
function parseGroups(chunk, allowEmbeddedV4) {
  if (chunk === "") return [];
  const pieces = chunk.split(":");
  const groups = [];
  for (let i = 0; i < pieces.length; i++) {
    const piece = pieces[i];
    if (piece.includes(".")) {
      if (!allowEmbeddedV4 || i !== pieces.length - 1) return null;
      const v4 = ipv4ToBytes(piece);
      if (!v4) return null;
      groups.push((v4[0] << 8) | v4[1], (v4[2] << 8) | v4[3]);
      continue;
    }
    if (!/^[0-9a-fA-F]{1,4}$/.test(piece)) return null;
    groups.push(parseInt(piece, 16));
  }
  return groups;
}

function isV4Mapped(bytes) {
  for (let i = 0; i < 10; i++) {
    if (bytes[i] !== 0) return false;
  }
  return bytes[10] === 0xff && bytes[11] === 0xff;
}

function forbidden() {
  return new Response(JSON.stringify({ error: "forbidden" }), {
    status: 403,
    headers: { "Content-Type": "application/json", "Cache-Control": "no-store" },
  });
}

// Factory so tests can inject a fake fetch and cache. `fetchImpl(request, init)` must behave
// like global fetch; `cache` must expose the Cache API's put/match.
export function createHandler({ fetchImpl, cache }) {
  return async function handle(request, env, ctx) {
    // Screened first: a denied source must not reach the origin, the cache, or the timeout paths.
    if (isDeniedSource(request.headers.get("CF-Connecting-IP"), denyPrefixes(env))) {
      return forbidden();
    }

    const originBase = env && env.ORIGIN ? env.ORIGIN : DEFAULT_ORIGIN;
    const url = new URL(request.url);
    const proxied = buildProxiedRequest(request, url, originBase);

    if (request.method === "GET" && url.pathname === RELAYS_PATH) {
      return relaysWithStaleFallback(proxied, request.url, ctx, fetchImpl, cache);
    }

    if (request.method === "POST" && url.pathname === WSS_TICKETS_PATH) {
      return wssTicketPassthrough(proxied, fetchImpl);
    }

    // All other paths/methods: passthrough exactly as before. No timeout — the speed-test
    // endpoint is long-lived by design.
    return fetchImpl(proxied, { cf: NO_FETCH_CACHE });
  };
}

async function wssTicketPassthrough(proxied, fetchImpl) {
  try {
    const response = await fetchImpl(proxied, {
      cf: NO_FETCH_CACHE,
      redirect: "manual",
      signal: AbortSignal.timeout(WSS_TICKET_TIMEOUT_MS),
    });
    // Tickets and ticket errors are per-client state. Preserve the streaming
    // body and all semantic headers (notably Retry-After), but prohibit every
    // downstream/shared cache from storing them.
    const uncached = new Response(response.body, response);
    uncached.headers.set("Cache-Control", "no-store");
    uncached.headers.set("Pragma", "no-cache");
    return uncached;
  } catch {
    return new Response(JSON.stringify({ error: "broker ticket origin unreachable" }), {
      status: 502,
      headers: { "Content-Type": "application/json", "Cache-Control": "no-store" },
    });
  }
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
  // Cache key: a bare GET on the full client URL (query string included — `limit` etc. vary the
  // response body) plus the version-namespace param (see CACHE_KEY_VERSION: bump to 2 when the
  // signing broker deploys, so pre-signing headerless bodies can never replay to
  // signature-requiring clients).
  const keyUrl = new URL(requestUrl);
  keyUrl.searchParams.set("__or_cache_v", CACHE_KEY_VERSION);
  const cacheKey = new Request(keyUrl, { method: "GET" });

  let originResponse = null;
  try {
    originResponse = await fetchImpl(proxied, {
      cf: NO_FETCH_CACHE,
      signal: AbortSignal.timeout(ORIGIN_TIMEOUT_MS),
    });
  } catch {
    // Network error or 10 s timeout — fall through to the stale path below.
  }

  if (originResponse !== null && originResponse.status === 200) {
    // Healthy: hand the origin response to the client unchanged and stash a complete copy for
    // later. Only a full 200 may be stored — a 206 partial body would poison the fallback with
    // a truncated relay list.
    ctx.waitUntil(storeFallbackCopy(cache, cacheKey, originResponse.clone()));
    return originResponse;
  }

  if (originResponse !== null && originResponse.status < 500) {
    // Non-200 2xx (204, 206) and 3xx/4xx are semantic answers (404, 429 back-pressure): pass
    // them through unchanged, never store them, never mask them with stale data.
    return originResponse;
  }

  // Origin failed (network error, timeout, or 5xx): serve the last healthy list if this colo
  // still holds one.
  const cached = await cache.match(cacheKey);
  if (cached) {
    if (originResponse !== null) {
      // Discard the unconsumed 5xx body we are about to drop on the floor; leaving the stream
      // unread leaks it in workerd ("response body not consumed").
      ctx.waitUntil(Promise.resolve(originResponse.body?.cancel()).catch(() => {}));
    }
    // Serve the stored copy with ALL of its headers intact — body + X-OpenRung-Relays-Signature
    // must survive byte-for-byte (see storeFallbackCopy) — re-marked so nothing downstream
    // re-caches or rewrites it, and so clients/telemetry can detect the degraded serve.
    const stale = new Response(cached.body, cached);
    stale.headers.set("Cache-Control", "no-store, no-transform");
    stale.headers.set("X-OpenRung-Stale", "1");
    return stale;
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

// Store a fallback copy of a healthy 200, preserving ALL origin headers — not just
// Content-Type: the broker will soon send X-OpenRung-Relays-Signature computed over the exact
// body bytes, and a stale-served response must keep body + signature intact or
// signature-requiring clients will reject it. Two edits only: the origin's `no-store` is
// replaced with the hard stale cap (the Cache API refuses to store a no-store response; the
// stored copy is only ever read on origin failure), and Set-Cookie is dropped so no per-client
// cookie can be replayed to other clients.
async function storeFallbackCopy(cache, cacheKey, copy) {
  const buf = await copy.arrayBuffer();
  const cached = new Response(buf, copy);
  cached.headers.set("Cache-Control", `public, max-age=${STALE_TTL_SECONDS}`);
  cached.headers.delete("Set-Cookie");
  await cache.put(cacheKey, cached);
}
