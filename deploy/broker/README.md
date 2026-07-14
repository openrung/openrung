# OpenRung Broker

The broker is the **control plane**: it matches clients with healthy relays and
records operational telemetry. It never carries user traffic and never holds any
Reality key — it only serves the relay directory (`GET /api/v1/relays`), accepts
relay registrations/heartbeats, ingests client telemetry, and (optionally)
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

The broker listens on `:8080` (host networking). Point hubs and relay runtimes
at it with `OPENRUNG_BROKER_URL`, and check it:

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
`OPENRUNG_BROKER_URL=http://<ip>:8080` to point hubs and relay runtimes at. Front the
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
  and volunteer-run relays), **or**
- explicitly opt into an open, unauthenticated broker with
  `OPENRUNG_ALLOW_ANONYMOUS_REGISTRATION=true` (the `.env.example` default).
  OpenRung's public network intentionally runs open so any volunteer can
  contribute a relay without a shared secret.

A token, when set, takes precedence and enforces auth; the anonymous flag then
becomes a no-op. Generate a token with `openssl rand -hex 32`. Send it only over
TLS — see Cloudflare below.

Keep whichever auth line you choose **in the env file** (`.env` / the
`--env-file` broker.env), never as an ad-hoc `docker run -e
OPENRUNG_ALLOW_ANONYMOUS_REGISTRATION=true`. Because the broker fails closed at
startup, a redeploy that forgets an inline `-e` flag crash-loops; keeping the
opt-in in the env file means it travels with every `--env-file` recreate.

Separately, `OPENRUNG_FOUNDATION_TOKEN` (optional) authorizes registrations
that claim `node_class: foundation`, marking relays the foundation operates
itself apart from community volunteers in the signed relay list. It works with
either auth mode above, must differ from `OPENRUNG_VOLUNTEER_TOKEN` (the
broker refuses to start otherwise), and belongs only on foundation-operated
relays. Heartbeats for a foundation relay must also present this token; the
broker refuses to extend the lease otherwise. At the origin store, an unattended
foundation row therefore disappears after one lease TTL. That is not an instant
client-visible revocation guarantee: an ordinary API directory has a 30-minute
signed freshness window, the Worker may serve its last healthy response for up
to 15 minutes (still bounded by that response's `not_after`), and a mirror has a
24-hour signed freshness window. Clients and their local caches must enforce each
snapshot's `not_after` with only the protocol's bounded clock-skew allowance;
operators should treat `node_class` as provenance captured when it was signed.

Treat the Foundation token like the relay-list signing seed. Never place it in
cloud-init/user-data, provider metadata, an inline `docker -e` argument, or a
shell command that tracing or history can retain. Transfer it only after boot
over an authenticated channel, store it in the root-owned mode-`0600` broker env
file, then **recreate** the container with `--env-file` (a Docker restart does
not reload a changed env file). `lightsail-up.sh` intentionally rejects
`OPENRUNG_FOUNDATION_TOKEN` for this reason.

> **Rolling back past `node_class`:** a broker image that predates the
> `node_class` column does not guard registrations or heartbeats, so it can
> overwrite a foundation relay's `host:port` row — replacing its id, keys, and
> endpoint with one controlled by an attacker or volunteer-run relay — while
> leaving `node_class` stuck at `'foundation'`. An upgraded broker would then
> sign that forged descriptor as Foundation.
>
> **Never run pre-`node_class` and `node_class`-aware broker binaries against
> the same database while any foundation row exists** — a mixed-version fleet
> (a partial rollback, or a blue/green deploy straddling this change) lets the
> old writer poison rows that the new writer signs. Keep the whole fleet on one
> side of this change.
>
> To roll back and later re-upgrade safely, order it strictly so no
> `node_class`-aware broker ever serves a row an old writer could have touched:
>
> 1. **Roll back:** stop every `node_class`-aware broker first, *then* start the
>    old image. (The forged-class window exists only while both run.)
> 2. **Re-upgrade:** stop and drain every old (pre-`node_class`) broker so
>    nothing can still write the poisoned state — confirm none remain.
> 3. With every broker still stopped (the new one not yet started), **delete**
>    all descriptors so every relay is forced to re-attest:
>
>    ```sql
>    DELETE FROM relay_descriptors;
>    ```
> 4. Only after that `DELETE` commits, start the new broker. Each relay's next
>    heartbeat now returns `404`, so it re-registers and re-attests: foundation
>    relays present the foundation token and are re-signed as `foundation`;
>    every other relay registers as `volunteer`.
>
> Use `DELETE`, not `UPDATE ... SET node_class = 'volunteer'`. An `UPDATE` clears
> the column, but heartbeats never rewrite `node_class` — a live foundation relay
> would keep heartbeating its downgraded row and never regain its class until it
> happened to restart. Deleting forces the `404` → re-register → re-attest cycle
> across the whole fleet, and deleting *before* the new broker starts guarantees
> it never signs a stale or forged `foundation` label.
>
> **Old brokers do not understand the foundation token.** A pre-`node_class`
> broker knows only its single `OPENRUNG_VOLUNTEER_TOKEN`, so while the old image
> runs, a foundation relay's bearer (`OPENRUNG_FOUNDATION_TOKEN`) is not a
> privileged credential to it: a token-gated old broker rejects it (`401`, since
> it differs from the volunteer token), and an anonymous old broker accepts it
> but its insert leaves `node_class` at the column default (`volunteer`).
> Foundation relays are therefore rejected or silently downgraded for the whole
> rollback window; the re-upgrade `DELETE` above is what restores them.

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

### End-to-end TLS to the origin

For a front to reach the origin over HTTPS (so tokens like the Foundation
registration token are never in cleartext on the edge → origin leg), terminate
TLS on the broker box with a Let's Encrypt reverse proxy on `:443` that forwards
to the broker on `:8080`. The production setup for the AWS CloudFront front —
Caddy config, cert/renewal, firewall, the required CloudFront origin settings,
verification, and rollback — is documented in
[`origin-tls.md`](./origin-tls.md), with the deployed config in
[`Caddyfile`](./Caddyfile). It is additive: the broker container and the
plaintext `:8080` path are untouched.

## Container hardening

Both `docker-compose.yml` and `lightsail-up.sh` run the broker with a
least-privilege container posture (verified 2026-07-13 by running the published
image under the full flag set — it starts, serves `/healthz`, and enforces the
read-only rootfs):

- `--cap-drop ALL` — the broker binds `:8080` (≥ 1024) and never changes user or
  mounts, so it needs no Linux capabilities.
- `--security-opt no-new-privileges` — nothing in the image escalates via setuid
  or file capabilities. (Unlike the relay runtime, which must **not** set this:
  it binds 443 through a `cap_net_bind_service` file capability that
  `no-new-privileges` would disable.)
- `--read-only` root filesystem with `--tmpfs /tmp` — the only writable paths are
  `/tmp` and the telemetry named volume (`jsonl` mode appends there; `postgres`
  mode writes nothing to disk).

These flags take effect **only** for a container created by the script or compose
file. A broker recreated by hand — for example a manual `docker run` that adds an
extra `-e` override — does not inherit them unless the flags are re-typed. Verify
a running container and recreate it (with the full flag set and `--env-file`) if
it has drifted:

```sh
docker inspect openrung-broker \
  --format '{{.HostConfig.ReadonlyRootfs}} {{.HostConfig.CapDrop}} {{.HostConfig.SecurityOpt}}'
# want: true [ALL] [no-new-privileges]
```

## Configuration

| Variable                             | Required | Default                             | Purpose                                                        |
| ------------------------------------ | -------- | ----------------------------------- | -------------------------------------------------------------- |
| `OPENRUNG_VOLUNTEER_TOKEN`           | yes\*    | —                                   | Shared registration token (must match hubs/relay runtimes)     |
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
