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
install -m 0600 .env.example .env  # edit: set OPENRUNG_BROKER_URL and OPENRUNG_PUBLIC_HOST
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

Optional: `OPENRUNG_OPERATOR_CONTACT` (or `-operator-contact`) is an email
address sent with the registration so the broker operator can reach you about
this relay (at most 128 characters; the relay refuses to start on a value the
broker would reject).
The broker keeps it private (operator inventory and admin relays page only) and
never publishes it in the relay directory. Direct mode sends it; leaving it
unset is fine.

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
  `docker -e` arguments, or traced shell commands. Provision the host first,
  transfer the env file over an authenticated channel, and recreate the
  container with `--env-file`. The Foundation's provisioning and fleet-roll
  helpers, which automate this (and the host firewall and access-address
  units described under Networking notes), live in the private
  `openrung/ops` repository.

`node_class` describes the operator of the exit relay, not the infrastructure it
traverses. A Foundation-operated relay hub therefore does not make the
volunteer-run relays tunneled through it Foundation-operated.

### Stable relay identity (recommended)

Without an explicit identity, the relay generates a fresh one on every restart.
WSS advertisement therefore requires a separately generated, long-lived
Ed25519 relay-identity seed in `.env`:

```sh
openssl rand -base64 32
# paste the one-line result as OPENRUNG_IDENTITY_SEED=...
```

The VLESS/Reality connection values are independent. Pin those as well when a
stable client configuration is desired (`OPENRUNG_CLIENT_ID`,
`OPENRUNG_REALITY_PRIVATE_KEY`, `OPENRUNG_REALITY_PUBLIC_KEY`, and
`OPENRUNG_SHORT_ID`):

```sh
docker run --rm --entrypoint xray openrung-relay:latest x25519
```

(`x25519` prints the Reality key pair; pick a random 16-hex-char `short-id` and any
UUID for the client id, or let the first run generate them and copy from the logs.)

**Rotating credentials (default in direct mode).** A direct-mode relay does not
serve `OPENRUNG_CLIENT_ID`: it derives a fresh VLESS credential every hour from
its identity seed, applies it to the running xray through the management API
(loopback-only, and unreachable from client traffic: the relay's routing
refuses client connections to its own host, private ranges, and the
management port), and announces it to the broker on its
heartbeat, so a copied directory entry stops admitting within two hours (a
credential the broker last confirmed is kept through a broker outage of up to
an hour, then retired too). A pinned `OPENRUNG_CLIENT_ID` is then ignored (the
relay logs this once). `-skip-xray-run` always serves the static credential:
an xray this relay does not run cannot be rotated.
`OPENRUNG_CREDENTIAL_ROTATION=off` restores the single static credential;
`OPENRUNG_CREDENTIAL_EPOCH=<any string>` re-keys every derived credential at
once without changing the relay identity. Rotation needs a broker that
accepts `client_id` on heartbeat (broker 0.4.0 or later); against an older
broker the relay keeps its registration-time credential accepted and
otherwise behaves as before.

**Access leases (default with rotation).** A rotating direct-mode relay also
declares the `access_leases` capability at registration. When the broker
(0.8.0 or later) echoes it, the relay long-polls the broker for the set of
broker-issued per-client credentials it must accept, installs them through
the same management API under `lease-<id>` user labels, removes the ones the
broker revokes or that expire, and acknowledges each applied revision; the
relay reports itself online once the first set is acknowledged. An operator
resolves a `lease-<id>` label seen in relay logs through the broker's
operational API (`GET /admin/api/leases/{id}`). Leases ride rotation's
managed xray, so `OPENRUNG_CREDENTIAL_ROTATION=off` and `-skip-xray-run`
leave them off as well, and a broker without lease support leaves the relay
serving the shared credential only. `OPENRUNG_ACCESS_LEASES=off` disables the
capability explicitly.

## Run

### One-line volunteer VPS bring-up

For volunteers with any Linux VPS with a public IPv4 address, one line brings
up a volunteer-class relay from the public image:

```sh
curl -fsSL https://raw.githubusercontent.com/openrung/openrung/main/deploy/relay/volunteer-up.sh | sudo sh
```

[`volunteer-up.sh`](volunteer-up.sh) applies the exact container posture of
the Foundation fleet — host networking, `--cap-drop ALL --cap-add
NET_BIND_SERVICE`, read-only rootfs with a `/tmp` tmpfs, settings in a
root-owned mode-`0600` `/etc/openrung/relay.env`, registration against the
broker's direct TLS origin — except that no `OPENRUNG_FOUNDATION_TOKEN` is
installed, so the broker attests the relay volunteer-class. It auto-detects
the public IPv4 address, mints a stable `OPENRUNG_IDENTITY_SEED` once, and
verifies the relay registers before declaring success. The first run also
names the relay with a generated adjective-noun name in the fleet style (the
name is public in the relay directory); to choose your own, pass
`OPENRUNG_LABEL` in the one-line command as shown below (letters, digits,
`.`, `_`, `-`; at most 63 characters — the relay's own label rules). The
only question is on the first run with a terminal attached: an optional contact
email (`OPENRUNG_OPERATOR_CONTACT`), read from the terminal device because
standard input is the script under `curl | sh`. Enter skips it, or accepts a
suggestion the script shows in brackets (the sudo user's git email); setting
`OPENRUNG_OPERATOR_CONTACT` — even to an empty value — answers it in advance.
Without a terminal (cloud-init, CI) nothing is asked, so automation can run the
script as-is, and later runs never ask. A relay set up before contacts existed
takes `OPENRUNG_OPERATOR_CONTACT` from a re-run once; an existing contact is
changed only by editing the env file. Re-running
against a relay set up before naming existed pins a name into its env file
the same way — such relays otherwise draw a fresh random name from the relay
binary on every restart. Re-running the same
line is the update path: the image is pulled, the serving container is retained
as a stopped backup, and a separately named candidate must register and remain
running without restarts before it is promoted. A failed start, crash loop, or
registration timeout automatically restores the previous container. The env
file (and with it the relay identity) is preserved throughout.

Overrides pass through `sudo env`, e.g.:

```sh
curl -fsSL https://raw.githubusercontent.com/openrung/openrung/main/deploy/relay/volunteer-up.sh | sudo env OPENRUNG_PUBLIC_HOST=203.0.113.7 OPENRUNG_LABEL=my-relay sh
```

To change settings later, edit `/etc/openrung/relay.env` and re-run the same
command. A host set up this way is already in the canonical layout the
Foundation's fleet tooling expects, should it ever be promoted.

After promotion, this volunteer-only helper refuses the Foundation credential
in `/etc/openrung/relay.env` without pulling an image or touching the relay.
Use the Foundation's fleet tooling for every subsequent Foundation-managed update.

### Compose (recommended)

[`docker-compose.yml`](docker-compose.yml) uses **host networking**, drops all
Linux capabilities except `NET_BIND_SERVICE`, and runs read-only. Host networking
is the right default for an exit relay: it exposes the server's real IPv6/IPv4 and
lets the optional connection log show real client IPs (Docker's bridge NAT would mask them).

```sh
docker compose up -d --build
```

If this host previously used the legacy Compose project, follow the migration
procedure above; the old and new host-network containers cannot share port 443.

### Per-relay WSS sidecar (Foundation only)

The same relay image and compose project contain an optional `wss-sidecar`
profile. This is one sidecar per destination relay, never a standalone gateway
fleet: it listens only on `127.0.0.1:8081` and can open Reality streams only to
this host's loopback `127.0.0.1:443` listener.

For an eligible direct-mode Foundation relay on public/local port 443:

1. Pin `OPENRUNG_IDENTITY_SEED`, set `OPENRUNG_LISTEN_HOST=0.0.0.0` and
   `OPENRUNG_CONNECTION_LOG=false`, and add this relay's canonical per-front
   `OPENRUNG_WSS_FRONTS` entries to `.env` only when ready to advertise them.
   WSS advertisement fails closed if the per-connection observer is enabled;
   Xray must own `:443` directly so opaque loopback streams create no address
   or byte-count records.
2. Copy [`.wss.env.example`](.wss.env.example) to `.wss.env`, set mode `0600`,
   and configure the exact relay ID, broker ticket public-key ring, and a unique
   current/overlap origin-token ring for every advertised front.
3. Configure the CDN front to use this relay's origin TLS listener on `8443`,
   with an origin-facing firewall that admits only the front's egress ranges
   and a fixed loopback proxy to the sidecar (the provider runbooks live in
   the private `openrung/ops` repository). The compose file intentionally does not invent or store origin certificate
   secrets.
4. Start the colocated services:

   ```sh
   docker compose --profile wss up -d --build
   docker compose --profile wss logs -f relay wss-sidecar
   ```

The sidecar keeps a read-only root filesystem. Its only persistent write is the
hashed single-use ticket journal in the project-scoped `wss-replay-state` volume
mounted at `/var/lib/openrung`; preserve that volume across container and image
recreation. Failure to open, lock, write, or synchronize the journal makes the
sidecar fail closed. Never share the volume between relay hosts.

For a staged rollout, first run the stable relay without
`OPENRUNG_WSS_FRONTS`, prepare `.wss.env`, start and verify the sidecar/origin,
then add the fronts and recreate the `relay` service so the broker begins
advertising them. Roll back in the reverse safety order: remove the fronts and
recreate `relay` to stop ticket issuance; wait out issued tickets and the
bounded session drain; stop `wss-sidecar` and origin `8443`; retain the replay
volume. Public direct Reality on 443 remains available throughout.

The Foundation's fleet-roll tooling currently manages and rolls only the single Reality relay
container. It does **not** coordinate the sidecar, its secret env, replay
volume, or origin TLS process. WSS-enabled Foundation hosts must use this
colocated compose profile (or explicitly extend that per-host workflow with the
same ordering) rather than treating the sidecar as a separate fleet service or
assuming a fleet update or rollback is an atomic WSS rollout.

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

### Relay releases and build identity

The standalone relay has its own semantic version in `cmd/relay/VERSION`; it is
not tied to a broker, relay-hub, desktop, or mobile release. A release tag must
be named `relay-vX.Y.Z` and match that file. The image workflow publishes the
corresponding `X.Y.Z` image tag, while ordinary `main` images report a
development build containing the source commit (for example,
`relay/0.1.0-dev+sha.c4b2c65`).

The version and full revision are compiled into the binary and attached to the
image as OCI metadata. Inspect a binary directly with:

```sh
relay --version
```

The relay sends this artifact identity to the broker as `relay_version`.
Compatibility is controlled separately by the tunnel/punch protocol versions
and negotiated capability flags, so relay and relay-hub semantic versions do
not need to match.

## Networking notes

- **Firewall / security group:** allow inbound **TCP 443** (the relay's public
  port). On AWS, add an inbound rule for TCP 443 from `0.0.0.0/0` **and** `::/0`.
  With `ufw`: `sudo ufw allow 443/tcp`. The broker connects to the port
  before it accepts a registration and refuses one it cannot reach (`public
  endpoint unreachable`, which the relay logs verbatim while it retries, in
  rounds backed off to a minute apart); a relay that registered and later
  went dark is kept
  registered but withheld from clients. `volunteer-up.sh` opens ufw/firewalld
  itself, prints the provider's fix when the registration is refused, and
  waits for the port to open (`OPENRUNG_REACHABILITY_WAIT`, default 600 s on
  a terminal, 0 otherwise).
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

- **New-destination rate limit:** the relay image ships
  [`egress-limit`](egress-limit.sh), an nftables ruleset that lets the host open
  connections to at most 10 new destination addresses per second (burst 300),
  where "new" means not contacted in the last 10 minutes. Addresses already
  contacted are never limited. New addresses in Cloudflare's ranges (the
  published list plus `104.28.0.0/14`) also draw on a separate budget of 1 per
  second (burst 120) per address family. `OPENRUNG_NEW_DEST_RATE` sets the rate;
  `off` removes the limit.

  The script ships in the relay image as `/usr/local/bin/egress-limit`, so the
  rules version with each relay release, but it runs on the host, as root, with
  the host's own `nft`: a capability-less container only copies it out to
  `/usr/local/sbin/openrung-egress-limit`. No container ever changes the host
  firewall, and the relay container keeps no capabilities. The script saves the
  ruleset to `/etc/openrung/egress-limit.nft`, which its boot unit reloads.
  The Foundation's fleet tooling does all of this after every verified roll,
  from the image now serving, and at first boot. To load or change it by hand on a host (the host
  needs its `nftables` package; on other distributions, its equivalent), stage
  each file and check it before it replaces the installed one, so a failed
  `docker run` can never leave an empty script or unit behind. Use `apply off`
  to remove the limit:

  ```sh
  command -v nft >/dev/null || { sudo apt-get update && sudo apt-get install -y nftables; }
  tmp="$(mktemp)"
  sudo docker run --rm --network none --cap-drop ALL --entrypoint cat \
      ghcr.io/openrung/openrung-relay:X.Y.Z /usr/local/bin/egress-limit > "$tmp" \
    && head -n 1 "$tmp" | grep -qx '#!/bin/sh' \
    && sudo install -m 0755 "$tmp" /usr/local/sbin/openrung-egress-limit \
    && sudo /usr/local/sbin/openrung-egress-limit apply 10 \
    && sudo /usr/local/sbin/openrung-egress-limit unit > "$tmp" \
    && sudo install -m 0644 "$tmp" /etc/systemd/system/openrung-egress-limit.service \
    && sudo systemctl daemon-reload \
    && sudo systemctl enable openrung-egress-limit.service \
    || echo "egress-limit: a step failed; nothing after it was installed" >&2
  rm -f "$tmp"
  ```

  The Foundation's fleet audit reports, per host, the relay version,
  whether the saved rules and the installed script match that relay's image,
  whether the host has `nftables`, whether the table is loaded, whether the
  boot unit is enabled, and the access address state (below). Volunteer
  relays (`volunteer-up.sh`, the desktop app) never get host firewall
  changes.

- **Access address (Foundation, optional):** `OPENRUNG_ACCESS_HOST` names one
  extra IPv6 address, distinct from the relay's primary address and its
  `OPENRUNG_PUBLIC_HOST`, that the relay serves on the same listener and
  sends privately at registration as `access_host`. The broker then names
  that address instead of the public host in the per-user access answer
  (`POST /api/v1/access`, see [docs/api.md](../../docs/api.md)); the
  anonymous relay list keeps the public host. The Foundation's fleet tooling,
  given `OPENRUNG_ACCESS_HOST=<IPv6>`, writes the variable into the env file
  and installs
  `openrung-access-address.service`, a host unit that adds the address
  before docker starts the relay and removes it when stopped, plus
  `openrung-access-address-reassert.timer`. A network manager that
  reconfigures the interface (systemd-networkd restarted by unattended
  upgrades, for one) drops addresses it does not own. While the unit is
  active, the timer runs `/usr/local/sbin/openrung-access-address-reassert`
  every minute: it re-adds the address the unit names when it is missing (or
  no longer deprecated) and logs one warning
  (`re-added access address … on …`); while the address is in place it does
  and logs nothing. The unit's command:

  ```
  ip -6 address replace <IPv6>/128 dev <default-route interface> preferred_lft 0 nodad
  ```

  `preferred_lft 0` makes the address *deprecated*: the kernel accepts and
  answers connections to it but never picks it as the source of a new one,
  so registration, heartbeats and client egress stay on the primary address
  (and the broker's reachability probe keeps targeting the public endpoint).
  Before every registration the
  relay checks, with a read-only netlink query (no capability needed), that
  the address is on a host interface, past duplicate address detection and
  deprecated; when it is not, the relay logs `not advertising access host …`
  with the reason and registers without it. The relay repeats the check
  every 30 seconds: when the address goes missing it logs
  `access host … is no longer usable` and re-registers without it, so the
  broker stops handing it out, and when it comes back it re-registers with
  it. The session, xray and the per-user credentials keep running across
  these re-registrations. xray keeps one wildcard listener
  — Go opens it as a single dual-stack socket — so the access address needs
  no second listener; the relay refuses an access host with a literal
  `OPENRUNG_LISTEN_HOST`, with connection logging, or outside direct mode.
  Without an explicit public host, address detection skips the access
  address, and a relay never advertises an access host equal to the public
  host it registers with.
  A roll adds the address before the roll, beside any previous one, and
  swaps the unit only after the roll is verified: a roll whose candidate never
  went live removes just the address this run added (a promoted candidate
  keeps it even if removing the old container fails), and a new unit that
  does not start is replaced by the previous one again, so neither the
  previous nor the promoted relay loses its address.
  `OPENRUNG_ACCESS_HOST=off` on a roll removes the unit, the address and
  the variable once the roll is verified; leaving it unset keeps whatever the
  host has. The address is static: changing it is a roll with the new
  value, which re-registers the relay. The fleet audit reports `access=none`, or the address
  with `access_state=present|missing|preferred`, whether its unit is enabled
  and whether its re-assert timer is active (`access_reassert=`). Volunteer relays (`volunteer-up.sh`, the desktop app) never set it.

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
