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
cp .env.example .env          # edit: signing seed, token/anonymous, dashboard, store
docker compose up -d --build
docker compose logs -f
```

The broker listens on `:8080` (host networking). Point hubs and volunteers at it
with `OPENRUNG_BROKER_URL`, and check it:

```sh
curl http://localhost:8080/healthz
curl http://localhost:8080/api/v1/relays
```

## Deploy to AWS Lightsail (one command)

`lightsail-up.sh` provisions a broker on a `micro_3_0` instance (1 GB RAM /
2 vCPU): it allocates a static IP, installs Docker, pulls
`ghcr.io/openrung/openrung-broker:main`, runs it (host networking, read-only
rootfs, persistent telemetry volume), and opens the HTTP port.

Prerequisites: an authenticated `aws` CLI (`aws configure`) with Lightsail
permissions, and the broker image published to GHCR and made **public** (see note
below).

```sh
./deploy/broker/lightsail-up.sh mybroker
```

With no `OPENRUNG_VOLUNTEER_TOKEN` set it runs open (anonymous registration); set
one to require auth. Optional overrides: `OPENRUNG_REGION`, `OPENRUNG_BUNDLE`,
`OPENRUNG_DASHBOARD_TOKEN`, `OPENRUNG_RELAY_STORE` / `OPENRUNG_RELAY_DATABASE_URL`,
`OPENRUNG_GEOIP_ENDPOINT`. The script prints the health URL and the
`OPENRUNG_BROKER_URL=http://<ip>:8080` to point hubs and volunteers at. Front the
origin with Cloudflare for TLS (below) and set the client apps' HTTPS broker URL
to the Cloudflare hostname.

> **GHCR package visibility:** the first time the broker image is published, the
> GHCR package defaults to **private** and the instance pulls anonymously. Make it
> public once: GitHub → org **Packages** → `openrung-broker` → *Package settings* →
> *Change visibility* → *Public*. (Or have the instance `docker login ghcr.io`.)

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

Separately, `OPENRUNG_FOUNDATION_TOKEN` (optional) authorizes registrations
that claim `node_class: foundation`, marking relays the foundation operates
itself apart from community volunteers in the signed relay list. It works with
either auth mode above, must differ from `OPENRUNG_VOLUNTEER_TOKEN` (the
broker refuses to start otherwise), and belongs only on foundation-operated
relays. Heartbeats for a foundation relay must also present this token; the
broker refuses to extend the lease otherwise, so a foundation label can never
outlive its authorized registrant by more than one lease TTL.

> **Rolling back past `node_class`:** a broker image that predates the
> `node_class` column neither rewrites the class on re-registration upserts
> nor guards heartbeats, so while a rollback is running, a re-registration at
> a foundation relay's `host:port` would keep the `foundation` label alive.
> After any such rollback, clear the column before (or right after)
> re-upgrading — foundation relays re-attest when they restart:
>
> ```sql
> UPDATE relay_descriptors SET node_class = 'volunteer';
> ```

## Relay-list signing

Every 2xx relay-list response (`/api/v1/relays` and `/api/v1/relays.mirror`) is
signed with an Ed25519 key — a detached signature over the exact body bytes in
the `X-OpenRung-Relays-Signature` header — so clients can verify the directory
over non-TLS channels (the direct-IP fallback, static mirrors). The broker
**refuses to start** without `OPENRUNG_RELAY_SIGNING_KEY` (standard base64 of
the 32-byte seed): serving unsigned lists would keep healthz green while every
verifying client rejected discovery. Generate a seed with
`openssl rand -base64 32`, keep it in the env file (root-owned, `0600`), and
redeploy with `--env-file` — never hand-typed inline `-e` vars. Clients pin the
matching public keys, so production must run the operator's active seed; the
startup log line `relay list signing enabled key_id=…` and the `signing_key_id`
field on `/healthz` confirm which key is live.

## Telemetry persistence

By default (`OPENRUNG_TELEMETRY_STORE=jsonl`) the broker appends client
telemetry to a JSONL file and periodically compacts it in place (7-day
retention). The image points `OPENRUNG_TELEMETRY_FILE` at
`/var/lib/openrung/telemetry.jsonl` on a persistent named volume, so dashboard
history survives restarts. The root filesystem is otherwise read-only.

- **Named volume (default):** works out of the box — Docker gives the fresh
  volume the image dir's `openrung` ownership.
- **Bind mount instead?** `chown` the host directory to the container's
  `openrung` uid first, or the broker will fail to open the telemetry file.
- **Don't need history?** Point `OPENRUNG_TELEMETRY_FILE` at `/tmp/…` (tmpfs) and
  drop the volume.

For production, set `OPENRUNG_TELEMETRY_STORE=postgres` to write events to a
partitioned `telemetry_events` table (one partition per day, created
automatically) instead of the JSONL file. It uses
`OPENRUNG_TELEMETRY_DATABASE_URL`, falling back to
`OPENRUNG_RELAY_DATABASE_URL` so a broker already on the Postgres relay store
needs no extra configuration; the broker refuses to start if neither is set.
In postgres mode the admin dashboard aggregates in SQL, bounded by the
selected time window, so dashboard cost and broker memory stay flat as event
history grows.

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
| `OPENRUNG_FOUNDATION_TOKEN`          | no       | —                                   | Privileged token for `node_class=foundation` registrations; must differ from the volunteer token |
| `OPENRUNG_RELAY_SIGNING_KEY`         | yes      | —                                   | Std-base64 32-byte Ed25519 seed; signs every relay-list response |
| `OPENRUNG_DASHBOARD_TOKEN`           | no       | —                                   | Enables the protected `/admin/telemetry` dashboard             |
| `OPENRUNG_ADDR`                      | no       | `:8080`                             | HTTP listen address                                            |
| `OPENRUNG_TRUSTED_PROXY_CIDRS`       | no       | Cloudflare ranges                   | Extra trusted proxy CIDRs for forwarded client IPs             |
| `OPENRUNG_RELAY_STORE`               | no       | `memory`                            | Relay state backend: `memory` or `postgres`                    |
| `OPENRUNG_RELAY_DATABASE_URL`        | if pg    | —                                   | PostgreSQL URL when `OPENRUNG_RELAY_STORE=postgres`            |
| `OPENRUNG_RELAY_RANKING`             | no       | `global`                            | Relay ranking mode: `global` or `legacy`                       |
| `OPENRUNG_GEOIP_ENDPOINT`            | no       | ipwho.is                            | IP-geolocation endpoint for relay city/country; `off` disables |
| `OPENRUNG_TELEMETRY_STORE`           | no       | `jsonl`                             | Telemetry backend: `jsonl` or `postgres`                       |
| `OPENRUNG_TELEMETRY_DATABASE_URL`    | no       | relay database URL                  | PostgreSQL URL when `OPENRUNG_TELEMETRY_STORE=postgres`        |
| `OPENRUNG_TELEMETRY_FILE`            | no       | `/var/lib/openrung/telemetry.jsonl` | Telemetry JSONL path in `jsonl` mode (its dir must be writable) |

\* The broker refuses to start unless either `OPENRUNG_VOLUNTEER_TOKEN` or
`OPENRUNG_ALLOW_ANONYMOUS_REGISTRATION=true` is set.

## Build the image directly

```sh
# from the repo root
docker build -f deploy/broker/Dockerfile -t openrung-broker .
```
