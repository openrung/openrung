// Unit tests for the broker-proxy request handler (stale-on-error behavior).
// Run from deploy/broker-proxy: `node --test` (or `npm test`).

import test from "node:test";
import assert from "node:assert/strict";

import { createHandler, DEFAULT_ORIGIN, STALE_TTL_SECONDS } from "../src/handler.js";

const EDGE = "https://broker.openrung.org";
const RELAYS_URL = `${EDGE}/api/v1/relays?limit=1`;
// The handler namespaces its cache key with a version param (bumped when the signing broker
// deploys, so pre-signing bodies cannot replay to signature-requiring clients).
const CACHE_KEY_URL = `${RELAYS_URL}&__or_cache_v=1`;

test("production broker origin remains HTTPS", () => {
  const origin = new URL(DEFAULT_ORIGIN);
  assert.equal(origin.protocol, "https:");
  assert.equal(origin.username, "");
  assert.equal(origin.password, "");
});

// Minimal Cache API fake: stores immutable snapshots keyed by URL, counts interactions, and —
// like the real Cache API — honors the stored copy's max-age/s-maxage on match (against an
// injectable clock), so the handler's stale-TTL bound is actually exercised by tests.
class FakeCache {
  constructor(clock = () => 0) {
    this.clock = clock; // milliseconds
    this.entries = new Map(); // url -> { status, headers, body: ArrayBuffer, storedAt }
    this.putCalls = [];
    this.matchCalls = 0;
  }

  async put(key, response) {
    const url = key instanceof Request ? key.url : String(key);
    const method = key instanceof Request ? key.method : "GET";
    const body = await response.arrayBuffer();
    const entry = {
      status: response.status,
      headers: new Headers(response.headers),
      body,
      storedAt: this.clock(),
    };
    this.putCalls.push({ url, method, ...entry });
    this.entries.set(url, entry);
  }

  async match(key) {
    this.matchCalls += 1;
    const url = key instanceof Request ? key.url : String(key);
    const hit = this.entries.get(url);
    if (!hit) return undefined;
    // Freshness check, s-maxage winning over max-age as in the real Cache API. An entry past
    // its lifetime no longer matches.
    const cacheControl = hit.headers.get("Cache-Control") ?? "";
    const ttl = /s-maxage=(\d+)/i.exec(cacheControl) ?? /max-age=(\d+)/i.exec(cacheControl);
    if (ttl && this.clock() - hit.storedAt > Number(ttl[1]) * 1000) {
      return undefined;
    }
    return new Response(hit.body.slice(0), { status: hit.status, headers: hit.headers });
  }

  seed(url, body, contentType = "application/json") {
    this.entries.set(url, {
      status: 200,
      headers: new Headers({
        "Content-Type": contentType,
        "Cache-Control": `public, max-age=${STALE_TTL_SECONDS}`,
      }),
      body: new TextEncoder().encode(body).buffer,
      storedAt: this.clock(),
    });
  }
}

// Minimal ExecutionContext fake.
class FakeCtx {
  constructor() {
    this.pending = [];
  }
  waitUntil(promise) {
    this.pending.push(promise);
  }
  async settle() {
    await Promise.all(this.pending);
  }
}

// Wraps a responder so tests can inspect what the handler fetched and with which init.
function recordingFetch(responder) {
  const impl = async (request, init) => {
    impl.calls.push({ request, init });
    return responder(request, init);
  };
  impl.calls = [];
  return impl;
}

function setup(responder, { clock } = {}) {
  const cache = new FakeCache(clock);
  const ctx = new FakeCtx();
  const fetchImpl = recordingFetch(responder);
  const handler = createHandler({ fetchImpl, cache });
  return { handler, cache, ctx, fetchImpl };
}

test("fresh 200: returned unchanged, proxied with timeout, cache populated via waitUntil", async () => {
  const body = JSON.stringify({ server_time: "2026-07-10T00:00:00Z", relays: [{ id: "r1" }] });
  const { handler, cache, ctx, fetchImpl } = setup(
    () =>
      new Response(body, {
        status: 200,
        headers: { "Content-Type": "application/json", "Cache-Control": "no-store" },
      }),
  );

  const request = new Request(RELAYS_URL, { headers: { "CF-Connecting-IP": "203.0.113.7" } });
  const response = await handler(request, undefined, ctx);

  // Client sees the origin response unchanged (origin's no-store intact, no stale marker).
  assert.equal(response.status, 200);
  assert.equal(await response.text(), body);
  assert.equal(response.headers.get("Cache-Control"), "no-store");
  assert.equal(response.headers.get("X-OpenRung-Stale"), null);

  // Proxied to the default origin with path+query preserved, forwarded headers, and a timeout.
  assert.equal(fetchImpl.calls.length, 1);
  const { request: proxied, init } = fetchImpl.calls[0];
  const originUrl = new URL(DEFAULT_ORIGIN);
  const proxiedUrl = new URL(proxied.url);
  assert.equal(proxiedUrl.protocol, originUrl.protocol);
  assert.equal(proxiedUrl.hostname, originUrl.hostname);
  assert.equal(proxiedUrl.port, originUrl.port);
  assert.equal(proxiedUrl.pathname, "/api/v1/relays");
  assert.equal(proxiedUrl.search, "?limit=1");
  assert.ok(init.signal instanceof AbortSignal, "relays fetch must carry a timeout signal");
  assert.deepEqual(init.cf, { cacheTtl: 0, cacheEverything: false });
  assert.equal(proxied.headers.get("X-Forwarded-For"), "203.0.113.7");
  assert.equal(proxied.headers.get("X-Forwarded-Proto"), "https");

  // The cached copy is written via ctx.waitUntil, keyed on the version-namespaced bare-GET edge
  // URL, with the origin's no-store replaced by the stale-TTL cap and the Content-Type
  // preserved.
  await ctx.settle();
  assert.equal(cache.putCalls.length, 1);
  const put = cache.putCalls[0];
  assert.equal(put.url, CACHE_KEY_URL);
  assert.equal(put.method, "GET");
  assert.equal(put.status, 200);
  assert.equal(put.headers.get("Cache-Control"), `public, max-age=${STALE_TTL_SECONDS}`);
  assert.equal(put.headers.get("Content-Type"), "application/json");
  const stored = await cache.match(new Request(CACHE_KEY_URL));
  assert.equal(await stored.text(), body);
});

test("ORIGIN env var overrides the proxy target", async () => {
  const { handler, ctx, fetchImpl } = setup(() => new Response("{}", { status: 200 }));

  await handler(new Request(RELAYS_URL), { ORIGIN: "http://127.0.0.1:19999" }, ctx);

  const proxiedUrl = new URL(fetchImpl.calls[0].request.url);
  assert.equal(proxiedUrl.protocol, "http:");
  assert.equal(proxiedUrl.host, "127.0.0.1:19999");
  assert.equal(proxiedUrl.pathname, "/api/v1/relays");
});

test("origin 500 with warm cache: 200 + X-OpenRung-Stale, Content-Type preserved", async () => {
  const staleBody = '{"server_time":"2026-07-10T00:00:00Z","relays":[{"id":"stale"}]}';
  const { handler, cache, ctx } = setup(() => new Response("boom", { status: 500 }));
  cache.seed(CACHE_KEY_URL, staleBody);

  const response = await handler(new Request(RELAYS_URL), undefined, ctx);

  assert.equal(response.status, 200);
  assert.equal(await response.text(), staleBody);
  assert.equal(response.headers.get("X-OpenRung-Stale"), "1");
  assert.equal(response.headers.get("Cache-Control"), "no-store, no-transform");
  assert.equal(response.headers.get("Content-Type"), "application/json");
  await ctx.settle();
  assert.equal(cache.putCalls.length, 0, "a failed response must not be cached");
});

test("origin timeout (fetch rejects) with warm cache: stale copy served", async () => {
  const staleBody = '{"relays":[{"id":"stale"}]}';
  const { handler, cache, ctx } = setup(() => {
    throw new DOMException("The operation timed out.", "TimeoutError");
  });
  cache.seed(CACHE_KEY_URL, staleBody);

  const response = await handler(new Request(RELAYS_URL), undefined, ctx);

  assert.equal(response.status, 200);
  assert.equal(response.headers.get("X-OpenRung-Stale"), "1");
  assert.equal(await response.text(), staleBody);
});

test("origin network error with cold cache: JSON 502, not an unhandled exception", async () => {
  const { handler, cache, ctx } = setup(() => {
    throw new TypeError("fetch failed");
  });

  const response = await handler(new Request(RELAYS_URL), undefined, ctx);

  assert.equal(response.status, 502);
  assert.equal(response.headers.get("Content-Type"), "application/json");
  assert.equal(response.headers.get("Cache-Control"), "no-store");
  const parsed = JSON.parse(await response.text());
  assert.ok(parsed.error, "502 body must carry a JSON error field");
  assert.equal(cache.matchCalls, 1, "the stale copy must have been looked up first");
});

test("origin 5xx with cold cache: origin response passed through", async () => {
  const { handler, cache, ctx } = setup(() => new Response("upstream sad", { status: 503 }));

  const response = await handler(new Request(RELAYS_URL), undefined, ctx);

  assert.equal(response.status, 503);
  assert.equal(await response.text(), "upstream sad");
  assert.equal(response.headers.get("X-OpenRung-Stale"), null);
  assert.equal(cache.matchCalls, 1);
});

test("origin 4xx passes through unmasked, even with a warm cache", async () => {
  for (const status of [404, 429]) {
    const { handler, cache, ctx } = setup(
      () => new Response(`err ${status}`, { status, headers: { "Retry-After": "30" } }),
    );
    cache.seed(CACHE_KEY_URL, '{"relays":[{"id":"stale"}]}');

    const response = await handler(new Request(RELAYS_URL), undefined, ctx);

    assert.equal(response.status, status);
    assert.equal(await response.text(), `err ${status}`);
    assert.equal(response.headers.get("Retry-After"), "30");
    assert.equal(response.headers.get("X-OpenRung-Stale"), null);
    assert.equal(cache.matchCalls, 0, `a ${status} must not trigger a stale lookup`);
    await ctx.settle();
    assert.equal(cache.putCalls.length, 0);
  }
});

test("non-200 2xx (204, 206) passes through and is never cached", async () => {
  // A 206 partial body would poison the fallback with a truncated relay list; a 204 has no
  // body worth serving. Both reach the client unchanged but must not trigger cache.put.
  for (const status of [204, 206]) {
    const body = status === 204 ? null : "partial body";
    const { handler, cache, ctx } = setup(() => new Response(body, { status }));

    const response = await handler(new Request(RELAYS_URL), undefined, ctx);

    assert.equal(response.status, status);
    if (body !== null) {
      assert.equal(await response.text(), body);
    }
    assert.equal(response.headers.get("X-OpenRung-Stale"), null);
    await ctx.settle();
    assert.equal(cache.putCalls.length, 0, `a ${status} must never be cached`);
    assert.equal(cache.matchCalls, 0, `a ${status} must not trigger a stale lookup`);
  }
});

test("stored copy preserves ALL origin headers; signature survives a stale serve intact", async () => {
  // The broker will soon send X-OpenRung-Relays-Signature over the exact body bytes; a
  // stale-served response must keep body + signature intact or signature-requiring clients
  // will reject it.
  const body = '{"server_time":"2026-07-10T00:00:00Z","relays":[{"id":"r1"}]}';
  let calls = 0;
  const { handler, cache, ctx } = setup(() => {
    calls += 1;
    if (calls === 1) {
      return new Response(body, {
        status: 200,
        headers: {
          "Content-Type": "application/json",
          "Cache-Control": "no-store",
          "X-OpenRung-Relays-Signature": "test-sig",
          "Set-Cookie": "session=abc",
        },
      });
    }
    return new Response("boom", { status: 500 });
  });

  // Warm the cache through the real fresh path.
  const fresh = await handler(new Request(RELAYS_URL), undefined, ctx);
  assert.equal(await fresh.text(), body);
  await ctx.settle();

  // Stored copy: every origin header preserved except the Cache-Control override and the
  // dropped Set-Cookie.
  assert.equal(cache.putCalls.length, 1);
  const put = cache.putCalls[0];
  assert.equal(put.headers.get("X-OpenRung-Relays-Signature"), "test-sig");
  assert.equal(put.headers.get("Content-Type"), "application/json");
  assert.equal(put.headers.get("Cache-Control"), `public, max-age=${STALE_TTL_SECONDS}`);
  assert.equal(put.headers.get("Set-Cookie"), null, "cookies must never be stored");

  // Origin now fails: the stale serve must carry the identical body byte-for-byte alongside
  // the signature header.
  const stale = await handler(new Request(RELAYS_URL), undefined, ctx);
  assert.equal(stale.status, 200);
  assert.equal(await stale.text(), body);
  assert.equal(stale.headers.get("X-OpenRung-Relays-Signature"), "test-sig");
  assert.equal(stale.headers.get("Content-Type"), "application/json");
  assert.equal(stale.headers.get("X-OpenRung-Stale"), "1");
  assert.equal(stale.headers.get("Cache-Control"), "no-store, no-transform");
  await ctx.settle();
});

test("stale copy no longer matches once older than STALE_TTL_SECONDS", async () => {
  // The FakeCache honors max-age against an injected clock, and the entry is written by the
  // real fresh path — coupling the stored Cache-Control header to the exported TTL constant so
  // neither can drift without this test failing.
  let nowMs = 0;
  let calls = 0;
  const { handler, cache, ctx } = setup(
    () => {
      calls += 1;
      return calls === 1
        ? new Response('{"relays":[{"id":"r1"}]}', {
            status: 200,
            headers: { "Content-Type": "application/json" },
          })
        : new Response("boom", { status: 500 });
    },
    { clock: () => nowMs },
  );

  await handler(new Request(RELAYS_URL), undefined, ctx);
  await ctx.settle();
  assert.equal(cache.putCalls.length, 1);

  // Just inside the TTL: the warm copy is still served on failure.
  nowMs = (STALE_TTL_SECONDS - 1) * 1000;
  const inside = await handler(new Request(RELAYS_URL), undefined, ctx);
  assert.equal(inside.status, 200);
  assert.equal(inside.headers.get("X-OpenRung-Stale"), "1");
  await inside.text();

  // Just past the TTL: the entry no longer matches, so the origin failure passes through.
  nowMs = (STALE_TTL_SECONDS + 1) * 1000;
  const past = await handler(new Request(RELAYS_URL), undefined, ctx);
  assert.equal(past.status, 500);
  assert.equal(await past.text(), "boom");
  assert.equal(past.headers.get("X-OpenRung-Stale"), null);
  await ctx.settle();
});

test("non-relays path: exact passthrough, no timeout, cache never touched", async () => {
  for (const path of ["/healthz", "/api/v1/speedtest", "/api/v1/relays/extra"]) {
    const { handler, cache, ctx, fetchImpl } = setup(() => new Response("ok", { status: 200 }));

    const response = await handler(new Request(`${EDGE}${path}`), undefined, ctx);

    assert.equal(response.status, 200);
    assert.equal(await response.text(), "ok");
    const { request: proxied, init } = fetchImpl.calls[0];
    assert.equal(new URL(proxied.url).pathname, path);
    assert.equal(init.signal, undefined, `${path} must not get a timeout signal`);
    assert.deepEqual(init.cf, { cacheTtl: 0, cacheEverything: false });
    assert.equal(cache.matchCalls, 0);
    assert.equal(cache.putCalls.length, 0);
    assert.equal(ctx.pending.length, 0);
  }
});

test("WSS ticket POST is bounded, uncached, streamed, and never follows redirects", async () => {
  const { handler, cache, ctx, fetchImpl } = setup(
    () => new Response('{"error":"use another front"}', {
      status: 307,
      headers: { Location: "https://unexpected.example/tickets", "Retry-After": "7" },
    }),
  );
  const request = new Request(`${EDGE}/api/v1/wss/tickets`, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-OpenRung-Client-ID": "client-1" },
    body: '{"relay_id":"relay_a","front_id":"front-a"}',
  });
  const response = await handler(request, undefined, ctx);

  assert.equal(response.status, 307);
  assert.equal(response.headers.get("Location"), "https://unexpected.example/tickets");
  assert.equal(response.headers.get("Retry-After"), "7");
  assert.equal(response.headers.get("Cache-Control"), "no-store");
  assert.equal(response.headers.get("Pragma"), "no-cache");
  assert.equal(fetchImpl.calls.length, 1);
  const { request: proxied, init } = fetchImpl.calls[0];
  assert.equal(new URL(proxied.url).pathname, "/api/v1/wss/tickets");
  assert.equal(proxied.method, "POST");
  assert.equal(init.redirect, "manual");
  assert.ok(init.signal instanceof AbortSignal);
  assert.deepEqual(init.cf, { cacheTtl: 0, cacheEverything: false });
  assert.equal(cache.matchCalls, 0);
  assert.equal(cache.putCalls.length, 0);
  assert.equal(ctx.pending.length, 0);
});

test("WSS ticket origin timeout returns an uncached JSON 502", async () => {
  const { handler, cache, ctx } = setup(() => {
    throw new DOMException("timed out", "TimeoutError");
  });
  const response = await handler(new Request(`${EDGE}/api/v1/wss/tickets`, {
    method: "POST",
    body: '{"relay_id":"relay_a","front_id":"front-a"}',
  }), undefined, ctx);
  assert.equal(response.status, 502);
  assert.equal(response.headers.get("Cache-Control"), "no-store");
  assert.equal((await response.json()).error, "broker ticket origin unreachable");
  assert.equal(cache.matchCalls, 0);
  assert.equal(cache.putCalls.length, 0);
});

test("non-relays 5xx: passed through with no stale substitution", async () => {
  const { handler, cache, ctx } = setup(() => new Response("down", { status: 500 }));
  cache.seed(`${EDGE}/healthz`, "should never be served");

  const response = await handler(new Request(`${EDGE}/healthz`), undefined, ctx);

  assert.equal(response.status, 500);
  assert.equal(await response.text(), "down");
  assert.equal(cache.matchCalls, 0);
});

test("non-GET on /api/v1/relays: passthrough, no timeout, cache never touched", async () => {
  const { handler, cache, ctx, fetchImpl } = setup(() => new Response("created", { status: 200 }));

  const request = new Request(`${EDGE}/api/v1/relays`, { method: "POST" });
  const response = await handler(request, undefined, ctx);

  assert.equal(response.status, 200);
  const { request: proxied, init } = fetchImpl.calls[0];
  assert.equal(proxied.method, "POST");
  assert.equal(init.signal, undefined, "non-GET must not get a timeout signal");
  assert.equal(cache.matchCalls, 0);
  assert.equal(cache.putCalls.length, 0);
  assert.equal(ctx.pending.length, 0);
});
