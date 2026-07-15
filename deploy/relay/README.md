# OpenRung relay — Docker deployment

A self-contained image for running an OpenRung relay on a cloud VPS (AWS EC2,
DigitalOcean, Hetzner, Linode, …). The same runtime serves Foundation-operated
and volunteer-run relays. The image bundles the `relay` binary and a pinned
[Xray-core](https://github.com/XTLS/Xray-core); it is configured entirely
through environment variables.

## Migrating from `deploy/volunteer`

The repository rename cannot move an ignored `.env` file. Preserve the existing
registration credentials and stable relay identity **before** stopping the old
container; do not replace them with a fresh `.env.example`:

```sh
# Run from the repository root while the old container is still serving.
install -m 0600 deploy/volunteer/.env deploy/relay/.env
docker compose -f deploy/relay/docker-compose.yml config -q
docker compose -f deploy/relay/docker-compose.yml build

# Only cut over after the canonical config and image build both succeed.
docker rm -f openrung-volunteer 2>/dev/null || true
docker compose -f deploy/relay/docker-compose.yml up -d
```

Keep the old `.env` securely until the canonical container has registered and
heartbeats normally, so it remains available for rollback.

## TL;DR

```sh
cd deploy/relay
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

### Foundation-operated relays

A Foundation-operated relay uses the same data plane, but attests its operator
provenance to the broker. The `OPENRUNG_FOUNDATION_TOKEN` credential is
self-contained — presenting it is all you need. Put these values in a root-owned
mode-`0600` env file:

```sh
OPENRUNG_FOUNDATION_TOKEN=<foundation-registration-token>
OPENRUNG_BROKER_URL=https://broker.example.com
OPENRUNG_PUBLIC_HOST=2001:db8::1234
```

The token is the same secret configured as `OPENRUNG_FOUNDATION_TOKEN` on the
broker. Setting it alone forces the entire Foundation posture — you do **not**
also set `OPENRUNG_NODE_CLASS` or `OPENRUNG_MODE`:

- **Foundation class** is forced; `OPENRUNG_NODE_CLASS` is unnecessary, and
  setting it to anything but `foundation` alongside the token is a startup error.
- **Direct mode** is forced. `auto` and `tunnel` never run, because the hub path
  would expose the Foundation bearer and the hub always registers the community
  exit operator as `volunteer`.
- The broker URL **must use HTTPS** (loopback HTTP is allowed only for local
  tests), and broker API redirects are refused so the bearer cannot follow a
  downgrade.
- Never put the token in cloud-init/user-data, provider metadata, inline
  `docker -e` arguments, or traced shell commands. The bundled Lightsail and
  Hetzner bootstrap helpers intentionally provision anonymous volunteer-class
  relays only and reject registration tokens; provision the host first,
  transfer the env file over an authenticated channel, and recreate the
  container with `--env-file`.

`node_class` describes the operator of the exit relay, not the infrastructure it
traverses. A Foundation-operated relay hub therefore does not make the
volunteer-run relays tunneled through it Foundation-operated.

#### Automating the post-boot step

[`foundation-up.sh`](foundation-up.sh) automates the two-step dance above. It
**wraps** `lightsail-up.sh` rather than changing it: the token still reaches the
host only over SSH, after boot, and never through user-data — `create` even
strips the token variables from the provisioning child's environment, so no bug
in `lightsail-up.sh` could ever embed the credential.

```sh
# Provision a new Lightsail host and install credentials on it.
OPENRUNG_FOUNDATION_TOKEN_CMD='pass show openrung/foundation-token' \
  deploy/relay/foundation-up.sh create

# Promote hosts that are already serving as volunteer-class relays.
OPENRUNG_FOUNDATION_TOKEN_CMD='pass show openrung/foundation-token' \
  deploy/relay/foundation-up.sh convert 203.0.113.10 203.0.113.11

# Roll a new image across the fleet. Needs no token: the credentials already on
# each host are reused as-is.
OPENRUNG_IMAGE=ghcr.io/openrung/openrung-relay:sha-abc1234 \
  deploy/relay/foundation-up.sh update 203.0.113.10 203.0.113.11
```

| Variable | Default | Meaning |
|----------|---------|---------|
| `OPENRUNG_FOUNDATION_TOKEN_CMD` | — | Command printing the token (`pass show …`, `aws secretsmanager get-secret-value …`, `op read …`). Preferred. Run via `bash -c`; the first output line is the token. |
| `OPENRUNG_FOUNDATION_TOKEN` | — | The token itself. Fallback for CI; prefer the command form so the secret is not sitting in your environment. |
| `OPENRUNG_IMAGE` | `…/openrung-relay:main` | Image to run. Pin a `sha-…` tag (or an `@sha256:…` digest) for reproducible rolls. |
| `OPENRUNG_BROKER_URL` | `https://broker-origin.openrung.org` | The broker's **direct TLS origin** (see `deploy/broker/origin-tls.md`), not a CDN front: a front would decrypt the Foundation bearer at every edge POP and collapse per-relay rate limiting onto shared edge IPs. Must be HTTPS; the script fails fast otherwise. |
| `OPENRUNG_ENV_FILE` | `/etc/openrung/relay.env` | Where credentials live. `convert` writes the canonical path and, once the relay verifies, removes a legacy `volunteer.env` so exactly one copy of the token remains on disk. |
| `OPENRUNG_SSH_KEY` / `OPENRUNG_SSH_USER` | `~/.ssh/id_ed25519_openrung` / `ubuntu` | SSH access to the host. |
| `OPENRUNG_REGION` | `ap-northeast-1` | Region used to look up Lightsail-published SSH host keys for pinning. |

Notes on the design:

- **The token is never a command-line argument.** `argv` is world-readable via
  `/proc` and is retained in shell history, so it is read from the environment or
  a command (run via `bash -c`, never `eval`) and streamed to the host over
  stdin. The script keeps shell tracing (`bash -x`) disabled so the token cannot
  leak into trace output, and the env-file write is atomic (tmp + rename).
- **SSH host keys are pinned out-of-band when possible.** Before the first
  connection that will carry the token, the host's SSH keys are fetched over the
  authenticated Lightsail API (`get-instance-access-details`) and written to
  `known_hosts`, upgrading first contact from trust-on-first-use to verified.
  Non-Lightsail hosts fall back to `accept-new` with a warning.
- **Everything read back from a host is allowlist-validated** (container names,
  env-file paths, identity values) before it is reused in a privileged command
  or echoed to the terminal, so a compromised relay cannot inject shell or
  terminal escapes into the operator's session.
- **`update` needs no token**, because it reuses the env file the host already
  has — but it refuses to run against an env file that lacks
  `OPENRUNG_FOUNDATION_TOKEN` (presence check only), so it cannot silently
  "verify" a non-Foundation host.
- **`update` is sequential and fails fast.** A bad image stops the roll at the
  first host rather than taking down every relay, and failures print an in-place
  rollback command (the previous container is kept, stopped, as
  `openrung-relay-old`).
- **Verification is self-contained.** A relay that presents the Foundation token
  forces `node_class=foundation` and exits during startup if the broker attests
  any other class, so a container that is still running and has logged a
  registration *is* the proof — no broker query needed.
- Legacy `openrung-volunteer` container and `volunteer.env` names are detected,
  so hosts predating the rename are handled without manual cleanup.

### Stable relay identity (recommended)

Without an explicit identity, the relay generates a fresh one on every restart.
Generate one once and paste the values into `.env`
(`OPENRUNG_CLIENT_ID`, `OPENRUNG_REALITY_PRIVATE_KEY`, `OPENRUNG_REALITY_PUBLIC_KEY`,
`OPENRUNG_SHORT_ID`):

```sh
docker run --rm --entrypoint xray openrung-relay:latest x25519
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

If this host previously used the legacy Compose project, follow the migration
procedure above; the old and new host-network containers cannot share port 443.

### Plain `docker run`

```sh
docker build -f deploy/relay/Dockerfile -t openrung-relay .   # from repo root
docker run -d --name openrung-relay --restart unless-stopped \
  --network host --cap-drop ALL --cap-add NET_BIND_SERVICE \
  --read-only --tmpfs /tmp \
  -e OPENRUNG_BROKER_URL=https://broker.example.com \
  -e OPENRUNG_PUBLIC_HOST=2001:db8::1234 \
  openrung-relay
```

### Pull a pre-built image

If you publish via the CI workflow, pull instead of building:

```sh
docker pull ghcr.io/openrung/openrung-relay:main
```

The cloud provisioning helpers pull this public canonical package by default.

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

Shutdown is graceful: `docker stop` sends SIGTERM, the relay runtime stops xray
and the connection observer and exits cleanly.

## Updating Xray-core

The Xray version is pinned via the `XRAY_VERSION` build arg (default in the
Dockerfile and `docker-compose.yml`). To bump it, change the version in both
places and rebuild:

```sh
docker compose build --build-arg XRAY_VERSION=v26.3.27
```

The build downloads the matching release and verifies it against the release's
published `SHA2-256` digest before extracting the binary.
