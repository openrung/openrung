// Unit tests for the broker-proxy request handler (stale-on-error behavior).
// Run from deploy/broker-proxy: `node --test` (or `npm test`).

import test from "node:test";
import assert from "node:assert/strict";

import { createHandler, DEFAULT_ORIGIN } from "../src/handler.js";

const EDGE = "https://broker.openrung.org";
const RELAYS_URL = `${EDGE}/api/v1/relays?limit=1`;

// Minimal Cache API fake: stores immutable snapshots keyed by URL, counts interactions.
class FakeCache {
  constructor() {
    this.entries = new Map(); // url -> { status, headers, body: ArrayBuffer }
    this.putCalls = [];
    this.matchCalls = 0;
  }

  async put(key, response) {
    const url = key instanceof Request ? key.url : String(key);
    const method = key instanceof Request ? key.method : "GET";
    const body = await response.arrayBuffer();
    const entry = { status: response.status, headers: new Headers(response.headers), body };
    this.putCalls.push({ url, method, ...entry });
    this.entries.set(url, entry);
  }

  async match(key) {
    this.matchCalls += 1;
    const url = key instanceof Request ? key.url : String(key);
    const hit = this.entries.get(url);
    if (!hit) return undefined;
    return new Response(hit.body.slice(0), { status: hit.status, headers: hit.headers });
  }

  seed(url, body, contentType = "application/json") {
    this.entries.set(url, {
      status: 200,
      headers: new Headers({
        "Content-Type": contentType,
        "Cache-Control": "public, s-maxage=180",
      }),
      body: new TextEncoder().encode(body).buffer,
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

function setup(responder) {
  const cache = new FakeCache();
  const ctx = new FakeCtx();
  const fetchImpl = recordingFetch(responder);
  const handler = createHandler({ fetchImpl, cache });
  return { handler, cache, ctx, fetchImpl };
}

test("fresh 2xx: returned unchanged, proxied with timeout, cache populated via waitUntil", async () => {
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

  // The cached copy is written via ctx.waitUntil, keyed on the bare-GET edge URL, with the
  // origin's no-store replaced by a 180 s cap and the Content-Type preserved.
  await ctx.settle();
  assert.equal(cache.putCalls.length, 1);
  const put = cache.putCalls[0];
  assert.equal(put.url, RELAYS_URL);
  assert.equal(put.method, "GET");
  assert.equal(put.status, 200);
  assert.equal(put.headers.get("Cache-Control"), "public, s-maxage=180");
  assert.equal(put.headers.get("Content-Type"), "application/json");
  const stored = await cache.match(new Request(RELAYS_URL));
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
  cache.seed(RELAYS_URL, staleBody);

  const response = await handler(new Request(RELAYS_URL), undefined, ctx);

  assert.equal(response.status, 200);
  assert.equal(await response.text(), staleBody);
  assert.equal(response.headers.get("X-OpenRung-Stale"), "1");
  assert.equal(response.headers.get("Cache-Control"), "no-store");
  assert.equal(response.headers.get("Content-Type"), "application/json");
  await ctx.settle();
  assert.equal(cache.putCalls.length, 0, "a failed response must not be cached");
});

test("origin timeout (fetch rejects) with warm cache: stale copy served", async () => {
  const staleBody = '{"relays":[{"id":"stale"}]}';
  const { handler, cache, ctx } = setup(() => {
    throw new DOMException("The operation timed out.", "TimeoutError");
  });
  cache.seed(RELAYS_URL, staleBody);

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
    cache.seed(RELAYS_URL, '{"relays":[{"id":"stale"}]}');

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
