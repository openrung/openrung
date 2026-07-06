# OpenRung Broker

The broker is the **control plane**: it matches clients with healthy relays and
records operational telemetry. It never carries user traffic and never holds any
Reality key — it only serves the relay directory (`GET /api/v1/relays`), accepts
volunteer registrations/heartbeats, ingests client telemetry, and (optionally)
serves a protected telemetry dashboard.

Because its traffic is tiny (control-plane only), the broker can run anywhere —
unlike relay hubs, egress cost is not a concern. In production it is typically
fronted by Cloudflare with a direct-IP origin fallback on `:8080`.

## Quick start

```sh
cp .env.example .env          # edit: token/anonymous, dashboard, store
docker compose up -d --build
docker compose logs -f
```

The broker listens on `:8080` (host networking). Point hubs and volunteers at it
with `OPENRUNG_BROKER_URL`, and check it:

```sh
curl http://localhost:8080/healthz
curl http://localhost:8080/api/v1/relays
```

## ⚠️ Auth: the broker fails closed

The broker **refuses to start without a registration token** so that not just
anyone can register a relay into the directory clients route their VPN traffic
through. You must either:

- set `OPENRUNG_VOLUNTEER_TOKEN` to a long random string (shared with your hubs
  and volunteers), **or**
- explicitly opt into an open, unauthenticated broker with
  `OPENRUNG_ALLOW_ANONYMOUS_REGISTRATION=true` (the `.env.example` default).

A token, when set, takes precedence and enforces auth; the anonymous flag then
becomes a no-op. Generate a token with `openssl rand -hex 32`. Send it only over
TLS — see Cloudflare below.

## Telemetry persistence

The broker appends client telemetry to a JSONL file and periodically compacts it
in place (7-day retention). The image points `OPENRUNG_TELEMETRY_FILE` at
`/var/lib/openrung/telemetry.jsonl` on a persistent named volume, so dashboard
history survives restarts. The root filesystem is otherwise read-only.

- **Named volume (default):** works out of the box — Docker gives the fresh
  volume the image dir's `openrung` ownership.
- **Bind mount instead?** `chown` the host directory to the container's
  `openrung` uid first, or the broker will fail to open the telemetry file.
- **Don't need history?** Point `OPENRUNG_TELEMETRY_FILE` at `/tmp/…` (tmpfs) and
  drop the volume.

## Telemetry dashboard

Set `OPENRUNG_DASHBOARD_TOKEN` to a long random string to enable the protected
dashboard at `/admin/telemetry`. When unset, the dashboard and its data API
return 404. Always serve it over HTTPS so the admin session cookie is protected.

## Shared PostgreSQL state (optional)

For safer restarts or multiple brokers behind a load balancer, use Postgres
instead of the default in-memory store:

```sh
OPENRUNG_RELAY_STORE=postgres
OPENRUNG_RELAY_DATABASE_URL=postgres://openrung:change-me@db:5432/openrung?sslmode=disable
```

## Fronting with Cloudflare

Put the broker behind Cloudflare (or another proxy) for TLS and DDoS absorption.
The broker trusts Cloudflare's published ranges for `CF-Connecting-IP` /
`X-Forwarded-For` automatically; add any additional proxy CIDRs with
`OPENRUNG_TRUSTED_PROXY_CIDRS`. The container uses **host networking** so the
broker sees the real Cloudflare edge IP as the peer — do not switch it to bridge
networking without reading the note in `docker-compose.yml`, or per-IP rate
limiting and telemetry source IPs will all collapse onto the docker gateway.

Firewall the raw origin `:8080` to Cloudflare's ranges so clients cannot bypass
the edge and spoof forwarded headers.

## Configuration

| Variable                             | Required | Default                             | Purpose                                                        |
| ------------------------------------ | -------- | ----------------------------------- | -------------------------------------------------------------- |
| `OPENRUNG_VOLUNTEER_TOKEN`           | yes\*    | —                                   | Shared registration token (must match hubs/volunteers)         |
| `OPENRUNG_ALLOW_ANONYMOUS_REGISTRATION` | yes\* | —                                   | Set `true` to run open when no token is set (\*one of these)   |
| `OPENRUNG_DASHBOARD_TOKEN`           | no       | —                                   | Enables the protected `/admin/telemetry` dashboard             |
| `OPENRUNG_ADDR`                      | no       | `:8080`                             | HTTP listen address                                            |
| `OPENRUNG_TRUSTED_PROXY_CIDRS`       | no       | Cloudflare ranges                   | Extra trusted proxy CIDRs for forwarded client IPs             |
| `OPENRUNG_RELAY_STORE`               | no       | `memory`                            | Relay state backend: `memory` or `postgres`                    |
| `OPENRUNG_RELAY_DATABASE_URL`        | if pg    | —                                   | PostgreSQL URL when `OPENRUNG_RELAY_STORE=postgres`            |
| `OPENRUNG_RELAY_RANKING`             | no       | `global`                            | Relay ranking mode: `global` or `legacy`                       |
| `OPENRUNG_GEOIP_ENDPOINT`            | no       | ipwho.is                            | IP-geolocation endpoint for relay city/country; `off` disables |
| `OPENRUNG_TELEMETRY_FILE`            | no       | `/var/lib/openrung/telemetry.jsonl` | Append-only telemetry JSONL path (its dir must be writable)    |

\* The broker refuses to start unless either `OPENRUNG_VOLUNTEER_TOKEN` or
`OPENRUNG_ALLOW_ANONYMOUS_REGISTRATION=true` is set.

## Build the image directly

```sh
# from the repo root
docker build -f deploy/broker/Dockerfile -t openrung-broker .
```
