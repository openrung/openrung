<div align="center">

<a href="https://openrung.org">
  <img src="docs/openrung-mark.svg" alt="OpenRung logo" width="120">
</a>

# OpenRung

**Reach the open internet.**

OpenRung is a relay network that helps people living behind internet censorship
reach blocked websites and apps through Foundation-operated and volunteer-run
relays in unrestricted regions.

[![Website](https://img.shields.io/badge/website-openrung.org-1d8a4f)](https://openrung.org)
[![License: GPL-3.0-or-later](https://img.shields.io/badge/license-GPL--3.0--or--later-blue)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go&logoColor=white)](go.mod)
[![Platforms](https://img.shields.io/badge/platforms-iOS%20%C2%B7%20Android%20%C2%B7%20macOS%20%C2%B7%20Windows%20%C2%B7%20Linux-4a5568)](#quick-start)
[![PRs welcome](https://img.shields.io/badge/PRs-welcome-1d8a4f)](CONTRIBUTING.md)

[Website](https://openrung.org) · [Architecture](docs/architecture.md) · [WSS fallback](docs/wss-fallback.md) · [Broker API](docs/api.md) · [Report an issue](https://github.com/openrung/openrung/issues)

</div>

---

## How it works

OpenRung connects censored users with relays in unrestricted regions, similar in
spirit to Tor's [Snowflake](https://snowflake.torproject.org/):

- **Clients** (mobile VPN apps, the desktop proxy app, and a terminal client
  with proxy and full-device TUN modes) route user traffic through a relay.
- **Relay operators** — the OpenRung Foundation and community volunteers — run
  a small command-line app that relays that traffic to the open internet.
- **The broker** is a control plane only: it matches clients with healthy
  relays and never proxies user traffic.

```mermaid
flowchart LR
    client["📱 Client app<br/>(VPN mode)"]
    relay["🔁 Relay"]
    broker["🧭 Broker<br/>(control plane)"]
    web["🌐 Open internet"]

    client -. "relay discovery" .-> broker
    relay -. "register + heartbeats" .-> broker
    client == "VLESS + REALITY + Vision" ==> relay
    relay ==> web
```

Relay transport uses [Xray-core](https://github.com/XTLS/Xray-core)'s
VLESS + REALITY + Vision, designed to be hard to distinguish from ordinary TLS
traffic. The broker ranks relay candidates using recent shared metrics —
connection success, active sessions, observed latency, and speed tests — so
clients are steered toward relays that actually work.

End-user broker-facing clients share the independently versioned
[`brokerapi`](brokerapi/README.md) Go module. It is the single source for relay
directory requests and raw-response signature verification, identity and
cache-control headers, telemetry and WSS-ticket requests, the loopback-only
cleartext exception, and the broker TLS transport. For the Cloudflare front
that transport opportunistically uses a compiled-in ECH configuration,
refreshes it from authenticated retry configurations, and quickly retries with
ordinary TLS when ECH is blocked. It never bootstraps ECH through DNS and never
applies ECH to the CloudFront front. That front omits the TLS server name
instead, letting the encrypted HTTP `Host` header select the distribution while
the same certificate verification runs against its exact hostname. On a direct
connection the CloudFront front therefore never puts its hostname in a
cleartext ClientHello, while the Cloudflare front conceals its own only as long
as ECH survives — the ordinary-TLS fallback sends it. A configured proxy
tunnels both fronts outside this transport and keeps sending the name.

The desktop app and both mobile clients still try direct Reality first. When a
genuine network failure blocks a direct-mode Foundation relay, that relay may
advertise its own signed WSS fronts. Each CDN front terminates at the same
relay's local sidecar, which carries opaque Reality bytes only to that relay's
loopback Reality listener. The broker issues relay- and front-bound
authorization tickets but remains outside the user data path. Those clients
and the relay-local sidecar share the transport mechanics from the
independently versioned [`wsscore`](wsscore/README.md) module; ticket authority,
origin authentication, deployment policy, telemetry orchestration, and user
interfaces remain in their owning applications.

## Highlights

- 🙌 **Simple volunteering** — one line on any Linux VPS, or one CLI plus an
  Xray binary; IPv6-first with IPv4 and dual-stack options.
- 🕳️ **Works behind CGNAT** — volunteer-run relays with no inbound port can join
  through a reverse-tunnel relay hub.
- ⚡ **Direct paths when possible** — compatible clients and tunneled relays
  attempt NAT hole punching first, with the relay hub as the fallback.
- 📱 **Full-device mobile client** — the OpenRung app routes all device
  traffic in VPN mode (developed in a separate React Native repository).
- 🛡️ **Direct-first censorship fallback** — the desktop app and both mobile
  clients can carry the existing end-to-end Reality connection through a
  relay-owned WSS/CDN front when the direct route is blocked.
- 🧭 **Privacy-aware control plane** — the broker matchmakes but never carries
  user traffic.
- 🗄️ **Production-friendly broker** — optional shared PostgreSQL state for safe
  restarts and load-balanced deployments.
- 📊 **Operational visibility** — colored per-connection logs for relay
  operators and an opt-in, token-protected telemetry dashboard.

## Quick start

### Volunteer: run a relay on your VPS

The fastest way to help people reach the open internet is to turn a Linux VPS
with a public IPv4 address into a volunteer relay. One command sets everything
up (on Debian/Ubuntu it installs Docker for you):

```sh
curl -fsSL https://raw.githubusercontent.com/openrung/openrung/main/deploy/relay/volunteer-up.sh | sudo sh
```

The script pulls the official relay image, runs it with the same hardened
container setup the Foundation fleet uses, auto-detects your server's public
IP, mints a stable relay identity and a public adjective-noun relay name,
registers with the public OpenRung broker, and confirms the relay is actually
serving before it declares success. No account or token is needed.

To choose the public relay name on the first run (letters, digits, `.`, `_`,
and `-`; at most 63 characters), pass `OPENRUNG_LABEL` through `sudo`:

```sh
curl -fsSL https://raw.githubusercontent.com/openrung/openrung/main/deploy/relay/volunteer-up.sh | sudo env OPENRUNG_LABEL=my-relay sh
```

Once `/etc/openrung/relay.env` exists, it is authoritative. Edit that file and
re-run the installer to change an existing relay's name or other settings.

Afterwards:

- **Allow inbound TCP 443** in your provider's firewall (and
  `sudo ufw allow 443/tcp` if you use ufw) so clients can reach the relay.
- **Update** by re-running the same command — your relay keeps its identity,
  and a failed update rolls back to the running version automatically.
- **Watch it**: `docker logs -f openrung-relay`
- **Stop volunteering**: `docker rm -f openrung-relay` (also delete
  `/etc/openrung/relay.env` to forget the relay's identity).

> [!IMPORTANT]
> **Before you volunteer:** relays currently act as direct exits, so the
> websites a user visits can see your server's IP address — much like a Tor
> exit node. Please read the [security and abuse notes](docs/security-abuse.md)
> and make sure you are comfortable with that before relaying. Letting
> volunteers act as entry relays in front of dedicated exit servers is on the
> [roadmap](#roadmap).

Prefer a home computer over a VPS? The one-click
[desktop volunteer app](desktop-volunteer/) works behind home NAT. For
configuration options and other setups, see
[`deploy/relay/README.md`](deploy/relay/README.md).

### Run the stack from source (development)

Everything below runs the broker, relays, and clients from source for
development and self-hosting. Volunteers don't need any of it — volunteer
relays register with the public OpenRung broker automatically.

You need Go 1.25+. Running a relay also requires an
[Xray-core](https://github.com/XTLS/Xray-core) binary that supports
`xray x25519` and `xray run -config`. The terminal client and the desktop app
bundle their own sing-box engine — no separate install; build them with
`-tags with_utls,with_external_windivert` (as the Makefile, the desktop
packaging scripts, and the release workflows do: uTLS to dial Reality relays,
and no embedded WinDivert driver), and pass the client's `-sing-box <path>`
only to substitute an external binary.

#### Start the broker

The broker fails closed: it refuses to start unless you either set a shared
registration token (`OPENRUNG_VOLUNTEER_TOKEN`, matched by hubs and
volunteer-run relays) or
explicitly opt into an open, unauthenticated broker. Running open lets anyone
register a relay into the directory, so only do it on a trusted/private network.
It also requires `OPENRUNG_RELAY_SIGNING_KEY` — standard base64 of the 32-byte
Ed25519 seed that signs every relay-list response (generate one with
`openssl rand -base64 32`):

```sh
OPENRUNG_ALLOW_ANONYMOUS_REGISTRATION=true \
OPENRUNG_RELAY_SIGNING_KEY="$(openssl rand -base64 32)" \
  go run ./cmd/broker -addr :8080
```

For safer restarts or multiple brokers behind a load balancer, run the broker
with shared PostgreSQL relay state (keep the token / anonymous flag):

```sh
OPENRUNG_ALLOW_ANONYMOUS_REGISTRATION=true \
OPENRUNG_RELAY_SIGNING_KEY="$(openssl rand -base64 32)" \
OPENRUNG_RELAY_STORE=postgres \
OPENRUNG_RELAY_DATABASE_URL='postgres://openrung:change-me@localhost:5432/openrung?sslmode=disable' \
  go run ./cmd/broker -addr :8080
```

Relay ranking uses live metrics by default; pass `-relay-ranking=legacy` only
as a rollback path for the old IPv6-first ordering.

To enable the protected telemetry dashboard, set a separate administrator token
before starting the broker, then open `/admin/telemetry`:

```sh
OPENRUNG_ALLOW_ANONYMOUS_REGISTRATION=true \
OPENRUNG_RELAY_SIGNING_KEY="$(openssl rand -base64 32)" \
OPENRUNG_DASHBOARD_TOKEN='replace-with-a-long-random-token' \
  go run ./cmd/broker -addr :8080
```

When the variable is unset, the dashboard and its data API return 404. In
production, serve the broker over HTTPS so the administrator session cookie is
protected in transit.

#### Run a relay

```sh
go run ./cmd/relay \
  -broker http://localhost:8080 \
  -public-host 127.0.0.1 \
  -public-port 8443 \
  -listen-host 127.0.0.1 \
  -listen-port 8443 \
  -xray /path/to/xray
```

Useful to know:

- With connection logging enabled (the default), `-listen-host ::` opens both
  the IPv6 and IPv4 wildcard listeners. When `-public-host` is omitted, the
  relay advertises the first global IPv6 address it finds. Set `-public-host`
  to advertise a DNS name, IPv4 address, or specific IPv6 address, and set
  `-listen-host` when the local bind address should differ.
- A global IPv6 address still needs inbound firewall/router rules that allow
  clients to reach the relay port.
- Client connection events print in color by default — green on open, red on
  close, with client IP, duration, and byte counts. Pass
  `-connection-log=false` to let Xray bind the public port directly.
- The broker currently advertises one `public_host` per relay. For both
  IPv4 and IPv6 discovery, advertise a DNS name with A and AAAA records, or
  run separate registrations.

#### Volunteer-run relays behind CGNAT

Volunteer-run relays with no inbound port (carrier-grade NAT) can join through
a relay hub. For local development, start an anonymous hub bound to loopback:

```sh
OPENRUNG_ALLOW_ANONYMOUS_VOLUNTEERS=true \
go run ./cmd/relayhub \
  -broker http://localhost:8080 \
  -control-addr 127.0.0.1:9443 \
  -public-host 127.0.0.1 \
  -public-bind-host 127.0.0.1 \
  -port-range 20000-20100
```

Then run the relay in tunnel mode — it binds Xray to loopback and dials the hub
instead of exposing a port (no `-public-host` needed):

```sh
go run ./cmd/relay \
  -tunnel \
  -hub 127.0.0.1:9443 \
  -hub-tls=false \
  -xray /path/to/xray
```

These loopback commands deliberately use an anonymous, plaintext hub for local
development only. A public hub should require the shared registration token
and TLS described in [`deploy/relayhub/README.md`](deploy/relayhub/README.md).

When NAT punching is unavailable or fails, all traffic for a CGNAT relay
transits the hub. Keep the relay path opt-in (public-IP relays should stay in
direct mode) and run public hubs away from metered cloud egress. See
[`deploy/relayhub/README.md`](deploy/relayhub/README.md) for cost details and
TLS setup.

#### Try a client

Inspect the raw relay directory response and confirm that the terminal client
can select a usable relay:

```sh
curl http://localhost:8080/api/v1/relays
go run ./cmd/client check -broker http://localhost:8080
```

Then connect with the interactive client (`c` connects, `q` quits):

```sh
go run ./cmd/client connect -broker http://localhost:8080
```

For the zero-privilege desktop proxy app, and for the terminal client's views,
keys, and full-device TUN mode, see
[`docs/desktop-client.md`](docs/desktop-client.md).
The separately maintained Android and iOS clients implement the same
direct-first WSS/CDN fallback through pinned `brokerapi` and `wsscore` modules.

## Repository layout

```text
cmd/broker/          Broker HTTP API (control plane).
cmd/relay/           Relay CLI for Xray-backed registration.
cmd/relayhub/        Relay hub for CGNAT volunteer-run relays
                     (reverse-tunnel data plane).
cmd/wsssidecar/      Relay-local WSS/CDN origin with a fixed loopback target.
cmd/client/          Terminal client (TUI; proxy and TUN capture modes).
internal/broker/     Broker store and HTTP handlers.
internal/punch/      NAT hole-punch QUIC layer (session, transport, bridges) over punchcore.
internal/relay/      Server-side relay registration and identity models.
internal/relayhub/   Relay hub configuration.
internal/tunnel/     Reverse-tunnel transport (hub + relay client, yamux).
internal/relayruntime/  Relay runtime, Xray config, and broker client helpers.
internal/wssbridge/  Relay-side tickets, replay/origin authentication,
                     admission limits, and sidecar orchestration over wsscore.
brokerapi/           Shared broker control-plane Go client and exported relay
                     schema (nested module github.com/openrung/openrung/brokerapi)
                     for desktop, Android, and iOS bindings.
connectcore/         Client policy engine — connect ladder, ranking, WSS
                     fallback and punch policy, telemetry classifier/outbox,
                     sing-box config builder, discovery, and the cross-repo
                     contract vectors (nested Go module
                     github.com/openrung/openrung/connectcore) consumed by the
                     terminal client, desktop app, and mobile bindings.
punchcore/           Shared NAT hole-punch protocol core (nested Go module
                     github.com/openrung/openrung/punchcore) consumed by the
                     servers and the desktop, Android, and iOS clients.
wsscore/             Shared opaque Reality-over-WSS transport core (nested Go
                     module github.com/openrung/openrung/wsscore) consumed by
                     the desktop, Android, and iOS clients and relay sidecar.
desktop/             Desktop GUI client (Wails v2 + React; own Go module).
desktop-volunteer/   One-click volunteer relay GUI (Wails v2 + React; own
                     Go module).
deploy/              Broker proxy, relay hub, and relay deployment assets.
docs/                Architecture, API, client, and operations docs.
```

## Documentation

| Document | What it covers |
| --- | --- |
| [Architecture](docs/architecture.md) | Goals, components, and trust boundaries |
| [Broker API](docs/api.md) | HTTP API reference (`/api/v1`) |
| [Component versioning](docs/versioning.md) | Independent release identities and compatibility contracts |
| [Desktop clients](docs/desktop-client.md) | GUI proxy mode, Linux shell setup, and the terminal client's proxy and TUN modes |
| [WSS fallback](docs/wss-fallback.md) | Per-relay protocol, trust boundaries, failure policy, and rollout |
| [Per-relay CloudFront deployment](deploy/relay/cloudfront-wss.md) | CloudFront and relay-local sidecar configuration |
| [Security and abuse](docs/security-abuse.md) | Threat model, volunteer risk, and abuse handling |
| [Relay hub deployment](deploy/relayhub/README.md) | Running a hub: TLS, ports, and cost |

## Roadmap

- **Dedicated exit servers** — let volunteer operators choose to act as entry
  relays instead of direct exits.
- **Abuse and rate controls** — exit policies, rate limits, and abuse
  reporting ahead of a broad public rollout.
- **NAT hole-punch coverage** — direct client↔relay paths for volunteer-run
  relays behind CGNAT are implemented in the desktop, Android, and iOS clients
  using `github.com/openrung/openrung/punchcore`; broader production rollout
  and harder NAT mappings remain planned.
- **Dual-stack relay discovery** — multiple public endpoints per relay.

Have an opinion on what should come first?
[Open an issue](https://github.com/openrung/openrung/issues) — roadmap feedback
is welcome.

## Testing and feedback

OpenRung is under active development, and reports from real networks are the
most valuable contribution:

- 🐛 **Found a bug or a rough edge?**
  [Open an issue](https://github.com/openrung/openrung/issues) — relay IDs,
  connection logs, and network conditions help a lot.
- 💡 **Feature ideas and questions** are welcome as issues too.
- 🙋 **Want to run a relay or help test the mobile app?** Email
  [admin@openrung.org](mailto:admin@openrung.org).

## Contributing

Contributions are accepted under GPL-3.0-or-later with a Developer Certificate
of Origin sign-off. See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the details,
and thank you for helping people reach the open internet.

## License

OpenRung is licensed under the **GNU General Public License v3.0 or later**
(GPL-3.0-or-later). See [`LICENSE`](LICENSE).

The mobile app (maintained in its own repository), the terminal client
(`cmd/client`), and the desktop app statically link
[sing-box](https://github.com/SagerNet/sing-box) (GPL-3.0-or-later), so the
combined apps — and the project as a whole — are GPL-3.0-or-later. The relay
transport's VLESS + REALITY + Vision support comes
from [Xray-core](https://github.com/XTLS/Xray-core) (MPL-2.0), which the
relay runs as a separate process.

Third-party components bundled or linked into distributed artifacts (Docker
images, server binaries), and the attribution and source-offer
obligations they carry, are documented in
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md). Complete corresponding
source for any released binary is available from this repository.

## Acknowledgements

OpenRung builds on excellent open source work:

- [Xray-core](https://github.com/XTLS/Xray-core) — VLESS + REALITY + Vision
  relay transport
- [sing-box](https://github.com/SagerNet/sing-box) — client tunnel and proxy
  engine
- [MapLibre](https://maplibre.org/) — maps in the mobile app
- Tor's [Snowflake](https://snowflake.torproject.org/) — inspiration for
  volunteer-powered circumvention

OpenRung is not affiliated with or endorsed by the sing-box, Xray, or MapLibre
projects; their names are used descriptively.
