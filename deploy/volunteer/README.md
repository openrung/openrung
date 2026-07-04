# OpenRung volunteer relay — Docker deployment

A self-contained image for running an OpenRung volunteer relay on a cloud VPS
(AWS EC2, DigitalOcean, Hetzner, Linode, …). The image bundles the `volunteer`
binary and a pinned [Xray-core](https://github.com/XTLS/Xray-core); it is
configured entirely through environment variables.

## TL;DR

```sh
cd deploy/volunteer
cp .env.example .env          # edit: set OPENRUNG_BROKER_URL and OPENRUNG_PUBLIC_HOST
docker compose up -d --build
docker compose logs -f        # expect: registered relay … / heartbeat ok
```

## Prerequisites

- Docker (with Compose v2) on a Linux host with a **public IP**.
- Inbound **TCP 443** open to the world (security group / firewall — see below).
- The broker already running and reachable from this host.

## Configure

Every setting is an `OPENRUNG_*` variable (see [.env.example](.env.example)).
Only two are required:

| Variable | Meaning |
|----------|---------|
| `OPENRUNG_BROKER_URL` | Broker base URL the relay registers with. |
| `OPENRUNG_PUBLIC_HOST` | Public IP / DNS name clients use to reach **this** relay. Must be set — a container cannot auto-detect it. |

### Stable relay identity (recommended)

Without an explicit identity, the relay generates a fresh one on every restart.
Generate one once and paste the values into `.env`
(`OPENRUNG_CLIENT_ID`, `OPENRUNG_REALITY_PRIVATE_KEY`, `OPENRUNG_REALITY_PUBLIC_KEY`,
`OPENRUNG_SHORT_ID`):

```sh
docker run --rm --entrypoint xray openrung-volunteer:latest x25519
```

(`x25519` prints the Reality key pair; pick a random 16-hex-char `short-id` and any
UUID for the client id, or let the first run generate them and copy from the logs.)

## Run

### Compose (recommended)

[`docker-compose.yml`](docker-compose.yml) uses **host networking**, drops all
Linux capabilities except `NET_BIND_SERVICE`, and runs read-only. Host networking
is the right default for an exit relay: it exposes the server's real IPv6/IPv4 and
lets the connection log show real client IPs (Docker's bridge NAT would mask them).

```sh
docker compose up -d --build
```

### Plain `docker run`

```sh
docker build -f deploy/volunteer/Dockerfile -t openrung-volunteer .   # from repo root
docker run -d --name openrung-volunteer --restart unless-stopped \
  --network host --cap-drop ALL --cap-add NET_BIND_SERVICE \
  --read-only --tmpfs /tmp \
  -e OPENRUNG_BROKER_URL=https://broker.example.com \
  -e OPENRUNG_PUBLIC_HOST=2001:db8::1234 \
  openrung-volunteer
```

### Pull a pre-built image

If you publish via the CI workflow, pull instead of building:

```sh
docker pull ghcr.io/<owner>/openrung-volunteer:latest
```

## Networking notes

- **Firewall / security group:** allow inbound **TCP 443** (the relay's public
  port). On AWS, add an inbound rule for TCP 443 from `0.0.0.0/0` **and** `::/0`.
  With `ufw`: `sudo ufw allow 443/tcp`.
- **IPv6:** OpenRung prefers IPv6. With host networking the container uses the
  host's IPv6 directly. Make sure the VPS actually has a routable global IPv6
  address and that inbound 443 is allowed over IPv6.
- **Bridge networking (IPv4-only fallback):** comment out `network_mode: host`,
  set `OPENRUNG_LISTEN_HOST=0.0.0.0`, and publish the port with `ports: ["443:443"]`.
  Client IPs in the connection log will then show Docker's gateway rather than the
  real client.
- **Binding 443 as non-root:** the binary carries a `cap_net_bind_service` file
  capability and the container adds `NET_BIND_SERVICE`. Do **not** add
  `no-new-privileges` (it disables file capabilities). To run with zero
  capabilities instead, use a public port ≥ 1024 (`OPENRUNG_PUBLIC_PORT` /
  `OPENRUNG_LISTEN_PORT`).

## Operations

```sh
docker compose logs -f            # follow logs
docker compose restart            # restart the relay
docker compose down               # stop and remove
```

Shutdown is graceful: `docker stop` sends SIGTERM, the volunteer stops xray and
the connection observer and exits cleanly.

## Updating Xray-core

The Xray version is pinned via the `XRAY_VERSION` build arg (default in the
Dockerfile and `docker-compose.yml`). To bump it, change the version in both
places and rebuild:

```sh
docker compose build --build-arg XRAY_VERSION=v26.3.27
```

The build downloads the matching release and verifies it against the release's
published `SHA2-256` digest before extracting the binary.
