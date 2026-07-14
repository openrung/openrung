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
[![Platforms](https://img.shields.io/badge/platforms-iOS%20%C2%B7%20Android%20%C2%B7%20macOS%20%C2%B7%20Linux-4a5568)](#quick-start)
[![PRs welcome](https://img.shields.io/badge/PRs-welcome-1d8a4f)](CONTRIBUTING.md)

[Website](https://openrung.org) · [Architecture](docs/architecture.md) · [Broker API](docs/api.md) · [Report an issue](https://github.com/openrung/openrung/issues)

</div>

---

## How it works

OpenRung connects censored users with relays in unrestricted regions, similar in
spirit to Tor's [Snowflake](https://snowflake.torproject.org/):

- **Clients** (the mobile app and a desktop CLI) route device traffic through a
  VPN tunnel to a relay.
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
active sessions, connection success, observed latency, and speed tests — so
clients are steered toward relays that actually work.

## Highlights

- 🙌 **Simple volunteering** — one CLI plus an Xray binary; IPv6-first with
  IPv4 and dual-stack options.
- 🕳️ **Works behind CGNAT** — volunteer-run relays with no inbound port can join
  through a reverse-tunnel relay hub.
- 📱 **Full-device mobile client** — the OpenRung app routes all device
  traffic in VPN mode (developed in a separate React Native repository).
- 🧭 **Privacy-aware control plane** — the broker matchmakes but never carries
  user traffic.
- 🗄️ **Production-friendly broker** — optional shared PostgreSQL state for safe
  restarts and load-balanced deployments.
- 📊 **Operational visibility** — colored per-connection logs for relay
  operators and an opt-in, token-protected telemetry dashboard.

## Quick start

You need Go 1.25+, and relay operators also need an
[Xray-core](https://github.com/XTLS/Xray-core) binary that supports
`xray x25519` and `xray run -config`.

### Start the broker

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

### Run a relay

```sh
go run ./cmd/volunteer \
  -broker http://localhost:8080 \
  -public-port 443 \
  -listen-port 443 \
  -xray /path/to/xray
```

Useful to know:

- The relay listens on IPv6 (`::`) by default and advertises the first
  global IPv6 address it finds. Pass `-public-host` (and `-listen-host` if
  needed) to use a DNS name, an IPv4 address, or a specific IPv6 address, or
  `-listen-host dual` to listen on both stacks in one process.
- A global IPv6 address still needs inbound firewall/router rules that allow
  clients to reach the relay port.
- Client connection events print in color by default — green on open, red on
  close, with client IP, duration, and byte counts. Pass
  `-connection-log=false` to let Xray bind the public port directly.
- The broker currently advertises one `public_host` per relay. For both
  IPv4 and IPv6 discovery, advertise a DNS name with A and AAAA records, or
  run separate registrations.

> [!IMPORTANT]
> **Before you volunteer:** relays currently act as direct exits, so the
> websites a user visits can see your IP address — much like a Tor exit node.
> Please read the [security and abuse notes](docs/security-abuse.md) and make
> sure you are comfortable with that before relaying. Letting volunteers act
> as entry relays in front of dedicated exit servers is on the
> [roadmap](#roadmap).

### Volunteer-run relays behind CGNAT

Volunteer-run relays with no inbound port (carrier-grade NAT) can join through
a relay hub. Run the hub on a publicly reachable host where bandwidth is cheap:

```sh
go run ./cmd/relayhub \
  -broker http://localhost:8080 \
  -public-host hub.example.com \
  -port-range 20000-20100
```

Then run the relay in tunnel mode — it binds Xray to loopback and dials the hub
instead of exposing a port (no `-public-host` needed):

```sh
go run ./cmd/volunteer -tunnel -hub hub.example.com:9443 -xray /path/to/xray
```

All traffic for a CGNAT relay transits the hub, so keep the relay path opt-in
(public-IP relays should stay in direct mode) and run hubs off
metered cloud egress. See [`deploy/relayhub/README.md`](deploy/relayhub/README.md)
for cost details and TLS setup.

### Try a client

List relay candidates and run the desktop CLI relay check:

```sh
curl http://localhost:8080/api/v1/relays
go run ./cmd/client check -broker http://localhost:8080
```

For macOS full-device routing, see [`docs/desktop-client.md`](docs/desktop-client.md).
The mobile app (React Native with native VPN modules) is developed in a
separate repository; the original native iOS and Android clients have been
retired from this one.

## Repository layout

```text
cmd/broker/          Broker HTTP API (control plane).
cmd/volunteer/       Relay CLI for Xray-backed registration (legacy path name).
cmd/relayhub/        Relay hub for CGNAT volunteer-run relays
                     (reverse-tunnel data plane).
cmd/client/          Desktop CLI client.
internal/broker/     Broker store and HTTP handlers.
internal/client/     Client engine, relay selection, and sing-box config.
internal/clienttelemetry/  Client metrics reporting to the broker.
internal/punch/      NAT hole-punch QUIC layer (session, transport, bridges) over punchcore.
internal/relay/      Shared relay descriptor models.
internal/relayhub/   Relay hub configuration.
internal/tunnel/     Reverse-tunnel transport (hub + relay client, yamux).
internal/volunteer/  Relay Xray config helpers (legacy package name).
punchcore/           Shared NAT hole-punch protocol core (nested Go module
                     github.com/openrung/openrung/punchcore) consumed by the
                     servers, the desktop client, and the mobile app's binding.
deploy/              Broker proxy, relay hub, and relay deployment assets.
docs/                Architecture, API, client, and operations docs + website.
```

## Documentation

| Document | What it covers |
| --- | --- |
| [Architecture](docs/architecture.md) | Goals, components, and trust boundaries |
| [Broker API](docs/api.md) | HTTP API reference (`/api/v1`) |
| [Desktop client](docs/desktop-client.md) | macOS/CLI client and full-device routing |
| [Security and abuse](docs/security-abuse.md) | Threat model, volunteer risk, and abuse handling |
| [Relay hub deployment](deploy/relayhub/README.md) | Running a hub: TLS, ports, and cost |

## Roadmap

- **Dedicated exit servers** — let volunteer operators choose to act as entry
  relays instead of direct exits.
- **Abuse and rate controls** — exit policies, rate limits, and abuse
  reporting ahead of a broad public rollout.
- **NAT hole punching** — direct client↔relay paths for volunteer-run relays
  behind CGNAT, without a hub in the middle (core protocol in the `punchcore`
  module,
  `github.com/openrung/openrung/punchcore`; shipped in the desktop and Android
  clients, iOS still planned).
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

The mobile app (maintained in its own repository) statically links
[sing-box](https://github.com/SagerNet/sing-box) (GPL-3.0-or-later), so the
combined app — and the project as a whole — is GPL-3.0-or-later. The relay
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
- [sing-box](https://github.com/SagerNet/sing-box) — mobile tunnel engine
- [MapLibre](https://maplibre.org/) — maps in the mobile app
- Tor's [Snowflake](https://snowflake.torproject.org/) — inspiration for
  volunteer-powered circumvention

OpenRung is not affiliated with or endorsed by the sing-box, Xray, or MapLibre
projects; their names are used descriptively.
