# OpenRung Relay Hub

The relay hub is the publicly reachable component that lets volunteer-run
relays behind **CGNAT** (carrier-grade NAT, no inbound port) join the network. A
CGNAT relay dials the hub outbound over a single TLS connection; the hub
allocates a public TCP port, registers the relay with the broker on the relay's
behalf, and forwards inbound client connections through the tunnel to the
relay's local Xray. Clients reach `hub_public_host:allocated_port` exactly
as they would any direct relay — no client changes.

The hub copies **opaque bytes** only. It never holds the Reality private key, so
it cannot decrypt the end-to-end VLESS Reality traffic flowing through it.

## ⚠️ Run this where bandwidth is cheap — NOT on AWS egress

Without a successful NAT punch, **all** client traffic for a CGNAT relay
transits the hub in both directions. The hub is therefore a pure bandwidth
mover on the tunnel/fallback path, and egress pricing dominates its cost:

| Sustained per relay | Monthly transfer | AWS egress (~$0.09/GB) | Unmetered host |
| ------------------- | ---------------- | ---------------------- | -------------- |
| 20 Mbps             | ~6.3 TB          | **~$575 / mo**         | ~$0            |

Run relay hubs on **unmetered or cheap-bandwidth providers** (Hetzner — ~20 TB
included then ~€1/TB, OVH, Fly, bare metal). Keep the broker (tiny control-plane
traffic) wherever you like. Scale horizontally and place hubs near relays to
spread load.

> The MVP does **not** enforce per-relay or per-hub bandwidth caps. Size the
> `OPENRUNG_HUB_PORT_RANGE` to the number of concurrent tunnels you intend to
> host, and run on infrastructure whose bandwidth cost you control.

Only use the hub's tunnel data path when a volunteer-run relay cannot expose a
port. Relays in `-mode auto` may still contact the hub to probe reachability; a
successful callback selects **direct** mode, so client traffic bypasses the hub.
Operators who already know the relay is publicly reachable can select
**direct** mode explicitly.

## Quick start

```sh
cp .env.example .env          # set OPENRUNG_HUB_PUBLIC_HOST + OPENRUNG_BROKER_URL
mkdir -p certs                # put hub.crt + hub.key here (see TLS below)
docker compose up -d --build
docker compose logs -f
```

Then start a CGNAT volunteer-run relay pointing at this hub (see
`deploy/relay/.env.example`):

```sh
OPENRUNG_TUNNEL=true
OPENRUNG_HUB_ADDR=hub.example.com:9443
OPENRUNG_VOLUNTEER_TOKEN=<same token as the hub/broker>
```

## Deploy to AWS Lightsail (1 GB)

`lightsail-up.sh` provisions a hub on a `micro_3_0` instance (1 GB RAM / 2 vCPU / 40 GB /
**2 TB transfer/mo**). Lightsail's bundled transfer makes the 1 GB tier a reasonable
cheap-bandwidth host for an MVP — the EC2-egress warning above does not bite within the bundle.

Prerequisites: an authenticated `aws` CLI (`aws configure`) with Lightsail permissions, and the
hub image published to GHCR and made **public** (see note below).

```sh
./deploy/relayhub/lightsail-up.sh myhub
```

This allocates a static IP, generates a self-signed TLS cert for that IP, installs Docker, pulls
`ghcr.io/openrung/openrung-relayhub:main`, runs the hub (host networking, certs mounted read-only),
and opens the control port (9443) plus the tunnel port range (20000-20100) in the firewall. It
prints the exact env to point a CGNAT relay at the hub:

```sh
OPENRUNG_TUNNEL=true OPENRUNG_HUB_ADDR=<static-ip>:9443 OPENRUNG_HUB_INSECURE=true
```

`OPENRUNG_HUB_INSECURE=true` is needed because the cert is self-signed; drop it once you switch to
a CA-issued cert. Override defaults via env, e.g. `OPENRUNG_REGION`, `OPENRUNG_BROKER_URL`,
`OPENRUNG_HUB_PORT_RANGE`, `OPENRUNG_VOLUNTEER_TOKEN`.

> Lightsail can only run punch in single-IP degraded mode (one public IPv4 per instance). For the
> **two-IP** setup that gives real NAT classification, use `ec2-up.sh` instead (below).

## Deploy to AWS EC2 (two public IPs — full NAT punch)

`ec2-up.sh` provisions a hub on EC2 with a secondary private IP + **two Elastic IPs**, so the punch
reflector has two vantage points (RFC 5780 classification). It binds the on-NIC private IPs and
advertises the EIPs (the bind/advertise split), self-signs a TLS cert covering both EIPs, installs a
boot-time unit that keeps the secondary private IP on the interface across reboots, and runs the hub
with punch enabled.

```sh
./deploy/relayhub/ec2-up.sh hub-ec2-seoul
```

Defaults: `ap-northeast-2`, `t4g.micro` (ARM). Override via `OPENRUNG_REGION`, `OPENRUNG_EC2_TYPE`,
`OPENRUNG_EC2_SUBNET`, etc. Verify with `curl -k https://<eip1>:9444/api/v1/punch/config` (returns
the two advertised reflector EIPs). Note EC2 egress is metered and each in-use public IPv4 is billed
hourly — run it where that is acceptable, or fall back to Lightsail single-IP degraded mode.

> **GHCR package visibility:** the first time the `relayhub-image` workflow publishes, the GHCR
> package defaults to **private**, and the instance pulls anonymously. Make it public once: GitHub →
> org **Packages** → `openrung-relayhub` → *Package settings* → *Change visibility* → *Public*.
> (Alternatively, have the instance `docker login ghcr.io` with a read token.)

## TLS

The control channel carries the shared auth token, so TLS is strongly
recommended. Provide a certificate and key via `OPENRUNG_HUB_TLS_CERT` /
`OPENRUNG_HUB_TLS_KEY` (mounted read-only into the container). With a real CA
certificate, relay runtimes verify it with their system roots out of the box.
For a self-signed cert during testing, relay runtimes can set
`OPENRUNG_HUB_INSECURE=true` to skip verification.

Without a cert/key the hub logs a warning and runs the control channel in
plaintext (local development only).

## Configuration

All settings are read from `OPENRUNG_*` environment variables (or the equivalent
flags — run `relayhub -h`). See `.env.example` for the full list. The essentials:

| Variable                      | Required | Default        | Purpose                                            |
| ----------------------------- | -------- | -------------- | -------------------------------------------------- |
| `OPENRUNG_HUB_PUBLIC_HOST`    | yes      | —              | Host advertised to clients for tunneled relays     |
| `OPENRUNG_BROKER_URL`         | yes      | —              | Broker the hub registers relays with               |
| `OPENRUNG_HUB_CONTROL_ADDR`   | no       | `:9443`        | Address dialed by volunteer-run relays              |
| `OPENRUNG_HUB_PORT_RANGE`     | no       | `20000-20100`  | Public ports allocated to tunnels (one each)       |
| `OPENRUNG_VOLUNTEER_TOKEN`    | yes\*    | —              | Shared auth token (must match the broker). Required: the hub refuses to start without it unless `OPENRUNG_ALLOW_ANONYMOUS_VOLUNTEERS=true` |
| `OPENRUNG_HUB_TLS_CERT/KEY`   | no       | —              | TLS for the control channel                        |
| `OPENRUNG_HUB_HEARTBEAT_INTERVAL` | no   | `30s`          | How often live relays are re-heartbeated           |
| `OPENRUNG_HUB_HTTP_ADDR`      | no       | —              | HTTP API address (e.g. `:9444`) — serves the reachability prober and, with reflectors, the punch coordinator; empty disables both |
| `OPENRUNG_HUB_REFLECTOR_ADDRS`| no       | —              | Comma-separated UDP reflector `host:port` addresses; required to enable punch |
| `OPENRUNG_HUB_PUNCH_TTL`      | no       | `6s`           | Punch time budget handed to peers                  |

Open the control port and the entire public port range in your firewall.

## Hub HTTP API: reachability probe + auto-detect

Set `OPENRUNG_HUB_HTTP_ADDR` (e.g. `:9444`) to enable the hub's HTTP API. Even
without punch, this powers **reachability auto-detection for volunteer-run
relays**: a relay started with `-mode auto` (the default when it has a `-hub`)
opens its listener and asks the hub to dial it back at its observed public IP.
If the callback succeeds the relay registers **directly**; if not (CGNAT /
firewalled) it falls back to
**tunnel** mode automatically — no manual `-tunnel` guesswork. The prober only
ever dials the caller's own source IP (never a caller-chosen host), so it is not
an SSRF vector; it requires the shared token and is rate-limited. Open the HTTP
API's TCP port in the firewall, and do **not** put it behind a proxy: the hub
must observe the relay connection's public source IP directly.

## NAT hole punching (optional, cuts hub egress)

When enabled, the hub coordinates a **direct** path between the client and a
CGNAT volunteer-run relay, so the bytes bypass the hub entirely (it only
signals). See
[`docs/architecture.md`](../../docs/architecture.md) for the full design. Punching
is **off** unless both `OPENRUNG_HUB_HTTP_ADDR` and `OPENRUNG_HUB_REFLECTOR_ADDRS`
are set.

- **Bind vs advertise (important on NAT'd hosts).** `OPENRUNG_HUB_REFLECTOR_ADDRS`
  is the reflector's **bind** list — addresses that exist on the host's NIC. On
  AWS (EC2/Lightsail) the public IP is 1:1-NAT'd and is **not** on the NIC, so
  binding it fails (`cannot assign requested address`). Bind the on-NIC private IP
  (or a wildcard `:19302`) and set `OPENRUNG_HUB_REFLECTOR_ADVERTISE` to the public
  IP(s) that peers should probe — positionally matched to the bind list.
- **Reflector IPs — two are better than one.** Correct NAT classification (is a
  peer punchable, or symmetric and best skipped?) needs **two distinct public
  vantage points**, each a distinct on-NIC bind socket. With a single IP the hub
  still works but classifies every peer as "unknown" and attempts-then-falls-back,
  wasting up to the punch budget on unpunchable NATs.
  - **Lightsail** gives only one public IPv4 per instance, so it can only do
    single-IP degraded mode: bind a wildcard, advertise the static IP, e.g.
    `OPENRUNG_HUB_REFLECTOR_ADDRS=:19302` +
    `OPENRUNG_HUB_REFLECTOR_ADVERTISE=<static-ip>:19302`.
  - **EC2** is the way to get two same-family public IPs on one box: add a second
    private IP to the ENI + a second Elastic IP, then bind the two private IPs and
    advertise the two EIPs, e.g.
    `OPENRUNG_HUB_REFLECTOR_ADDRS=10.0.0.10:19302,10.0.0.11:19302` +
    `OPENRUNG_HUB_REFLECTOR_ADVERTISE=<eip1>:19302,<eip2>:19302`.
- **Firewall.** In addition to the control port and public port range, open the
  reflector **UDP** port(s) on every reflector IP, and the punch coordinator's
  **TCP** port (`OPENRUNG_HUB_HTTP_ADDR`). With `network_mode: host` (the default
  compose setup) no extra Docker port mapping is needed.
- **Exposure.** The punch coordinator HTTP endpoint is direct-internet (not
  Cloudflare-fronted) and carries its own per-source-IP + per-relay rate limiting;
  the reflector enforces a request-size floor so it can never amplify traffic.
- **Volunteer-run relays** offer punching by default (`-punch`, tunnel mode). A
  relay on a public IP or global IPv6 should keep using direct/registration mode
  instead — punching only helps genuinely NAT'd relays.
