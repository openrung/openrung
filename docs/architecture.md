# Architecture

## Goals

OpenRung provides temporary volunteer relays in unrestricted regions so clients behind internet censorship can reach blocked public websites and apps.

The current version optimizes for learning:

- Require each volunteer to expose a public reachable TCP port.
- Use Xray-core for VLESS + Reality + Vision transport.
- Keep the broker out of the data path.
- Let the mobile client route all device traffic through a VPN tunnel.

## Components

### Broker

The broker is a control-plane service. It stores short-lived relay descriptors and returns candidates to clients.

The broker does not:

- Proxy user traffic.
- Store browsing destinations.
- Terminate VLESS sessions.
- Know client traffic contents.

The broker does:

- Accept volunteer registration.
- Track volunteer heartbeats.
- Expire stale volunteers.
- Return a small ranked candidate set to clients using recent relay load, success, latency, and speed-test signals.
- Optionally persist relay state in PostgreSQL so multiple broker instances can share one relay view behind a load balancer.
- Keep experimental relay/session/metric fields in JSONB columns until they are stable enough to promote into indexed columns.

### Volunteer CLI

The volunteer CLI runs on desktop systems. In the current version it starts an Xray-core inbound listener and registers the relay with the broker. It defaults to an IPv6 listener and auto-advertises the first global IPv6 address it can find unless the operator supplies `-public-host`. With connection logging enabled, `-listen-host dual` opens both IPv6 and IPv4 public listeners and forwards both to one loopback Xray listener. The CLI also wraps Xray with a local TCP observer by default so it can print client connect and disconnect events without changing the broker descriptor.

The CLI produces an Xray server config with:

- VLESS inbound.
- Reality transport.
- Vision flow: `xtls-rprx-vision`.
- Freedom outbound, meaning the volunteer is the direct exit.

The volunteer also supports a **CGNAT reverse-tunnel mode** for hosts that cannot
expose a public port; see the Relay Hub section below.

**Mode auto-detection.** The volunteer picks direct vs tunnel via `-mode`
(`auto` | `direct` | `tunnel`). In `auto` (the default whenever a `-hub` is
configured) it runs a startup **reachability probe**: it opens its listener and
asks the hub's HTTP API to dial it back at its observed public IP with a nonce
handshake. If the callback succeeds it registers **directly** (advertising the
observed IP), otherwise it falls back to **tunnel** mode — so operators no longer
have to know in advance whether a host is behind CGNAT. A probe that can't run
(hub HTTP API down) is treated as inconclusive and defaults to tunnel. `-mode
direct`/`tunnel` (and the legacy `-tunnel`) force a mode and skip the probe.

### Relay Hub (CGNAT volunteers)

Most volunteers expose a public reachable port. Volunteers behind **CGNAT**
(carrier-grade NAT) have no inbound port and cannot. The relay hub (`cmd/relayhub`)
is a separate, publicly reachable component that brings these volunteers online:

- The volunteer runs Xray bound to **loopback** and dials the hub outbound over a
  single TLS connection (`-tunnel -hub <addr>`), authenticating with the same
  registration token.
- The hub allocates one public TCP port per volunteer, registers the relay with
  the broker over the existing `POST /api/v1/volunteers/register` API (with
  `transport: "tunnel"` and `public_host`/`public_port` pointing at the hub), and
  multiplexes inbound client connections to the volunteer over the tunnel using
  yamux. Clients connect to `hub:port` exactly as they would any direct relay — no
  client changes.
- Descriptor liveness is tied to the tunnel: the hub heartbeats while the tunnel
  is healthy and stops when it drops, so the relay expires via the broker's normal
  lease TTL.

The hub is a **data-plane** component, deliberately distinct from the control-plane
broker — the broker stays out of the data path (see Goals). The hub only copies
**opaque bytes**; it never holds the Reality private key and cannot decrypt the
end-to-end traffic, so the volunteer-as-untrusted-network trust boundary is
preserved across the hub too.

Because all CGNAT-volunteer traffic transits the hub, the relay path is opt-in
(public-IP/IPv6 volunteers stay direct and never touch the hub), and hubs should
run where bandwidth is cheap rather than on metered cloud egress (see
`deploy/relayhub/README.md`). To keep the hub out of the hot path when possible,
CGNAT relays also support **direct NAT hole punching** (below); the hub tunnel
remains as the always-available fallback. Per-relay/per-hub bandwidth caps are
still future work.

### Direct NAT Hole Punching (client ↔ CGNAT volunteer)

When both the client and a CGNAT volunteer are behind NAT, they can still reach
each other **directly** by punching a UDP hole, taking the hub out of the data
path. The hub coordinates but never carries the bytes.

- **Rendezvous + reflector = the hub.** The hub already holds the only live,
  authenticated control connection to each CGNAT volunteer (the yamux tunnel), so
  it is the one component that can push a punch request to the volunteer. It also
  runs a small UDP **reflector** (STUN-like) so each peer learns its
  server-reflexive `ip:port`. The broker is untouched; it only gains an additive
  `punch_capable` descriptor flag so clients know to try.
- **Discovery + NAT classification.** Each peer probes the reflector from the same
  UDP socket it will punch and carry QUIC on. Binding the reflector on **two
  distinct public IPs** lets the hub classify the peer's NAT mapping: a stable
  reflexive port across both IPs is endpoint-independent (punchable); a differing
  port is symmetric (skipped). With a single reflector IP the class degrades to
  "unknown" and the client attempts anyway, then falls back.
- **Signalling.** The client asks the hub over a dedicated HTTP endpoint
  (`POST /api/v1/punch/request`, on the hub's own listener, not the broker and not
  Cloudflare-fronted, so it is separately rate-limited). The hub relays a
  `PunchDirective` to the volunteer over the yamux control connection, using a
  one-byte stream-type discriminator that is only emitted when both ends negotiate
  it in the HELLO/HELLO_ACK handshake — so the tunnel control-protocol version is
  unchanged and old volunteers/hubs keep working.
- **Transport.** After a token-authenticated simultaneous-open UDP punch, the two
  peers run **QUIC** (quic-go) over the punched socket: it gives the reliable,
  ordered, multiplexed byte stream that VLESS/Reality-over-TCP needs. The client
  exposes a loopback TCP bridge that sing-box dials in place of the relay; the
  volunteer bridges each QUIC stream to its loopback Xray. Reality still
  terminates only at client and volunteer — the QUIC layer carries opaque bytes,
  so the E2E trust boundary is preserved (QUIC's TLS is a transport/pinning layer,
  not a new decryption point).
- **Authentication.** The hub issues a per-session HMAC token (delivered over the
  authenticated HTTP and control channels); the UDP probes and the first QUIC
  stream carry it, verified in constant time. The QUIC certificate is pinned by a
  fingerprint the volunteer reports through the hub.
- **Fallback is invisible and fail-closed.** If the relay is not punch-capable,
  both NATs are symmetric, the relay id is stale, or any step times out within the
  budget, the client silently uses today's path (sing-box dials the hub's public
  TCP port). QUIC's bidirectional handshake means a half-open hole can never
  false-succeed.

Honest scope: this reliably serves endpoint-independent-mapping (home-broadband /
full-cone) volunteers; **double-symmetric CGNAT is deliberately not solved** and
stays on the hub relay. The shared protocol core (wire format, discovery, punch
mechanics, reflector, policies) lives in the nested `punchcore/` Go module
(`github.com/openrung/openrung/punchcore`) — the single source of truth with no
hand-mirrored copies. The servers and the desktop client consume it in-repo via
`internal/punch` (the quic-go session/transport/bridge layer); the Android app's
gomobile binding (`android/punchbridge` in `openrung-mobile-app`) consumes the
punchcore module at a pinned, tagged version. iOS remains a follow-up: it embeds
stock sing-box without an app-layer punch client, so it ignores `punch_capable`
and uses the hub relay with no regression.

#### punchcore pin/upgrade procedure (wire changes)

1. Edit `punchcore/` in an openrung PR — the hub, volunteers, and desktop
   clients consume it via the in-repo `replace`, so servers and desktop stay
   atomically consistent.
2. Merge, then tag `punchcore/vX.Y.Z` on `main` (the nested-module tag makes it
   fetchable through the Go proxy).
3. A mobile PR bumps the require in `android/punchbridge/go.mod` (+`go.sum`),
   which automatically busts the AAR CI caches (their hash keys include
   go.mod/go.sum).
4. Rebuild the AAR via `android/build-libbox-release.sh` and ship.

Local cross-repo development uses
`PUNCHCORE_SRC=/path/to/openrung/punchcore android/build-libbox-release.sh`
and/or an uncommitted `go.work` — never in releases (GPL §6 pins the module
version).

```mermaid
sequenceDiagram
    participant C as Client (behind NAT)
    participant H as Relay Hub (reflector + coordinator)
    participant V as CGNAT Volunteer

    C->>H: STUN to reflector (2 IPs) → learn reflexive ip:port
    C->>H: POST /punch/request (relay_id, candidates)
    H->>V: PunchDirective over yamux control stream
    V->>H: STUN to reflector → PunchAck (reflexive, cert fp)
    H-->>C: PunchResponse (volunteer candidates, token, cert fp)
    par simultaneous open
        C->>V: token-authed UDP probes
        V->>C: token-authed UDP probes
    end
    C->>V: QUIC over punched socket (VLESS/Reality inside)
    V->>V: Reality terminates at volunteer → Xray exits
    Note over C,V: hub carries no data, falls back to hub relay on failure
```

```mermaid
sequenceDiagram
    participant V as CGNAT Volunteer
    participant H as Relay Hub
    participant B as Broker
    participant C as Mobile Client

    V->>V: Start Xray VLESS Reality on loopback
    V->>H: Dial outbound (TLS) + authenticate + announce relay metadata
    H->>B: Register relay (public_host=hub, public_port=P, transport=tunnel)
    H-->>V: Allocated public endpoint, tunnel established
    C->>B: Request relay candidates
    B-->>C: Ranked descriptors incl. hub:P
    C->>H: Connect using VLESS Reality Vision
    H->>V: Forward opaque bytes over tunnel
    V->>V: Xray exits to destination
```

### Mobile Client

The mobile client is an iOS/Android app using VPN mode. It asks the broker for relay candidates, configures a compatible VLESS Reality client, and routes device traffic through the selected volunteer.

It is developed in a separate React Native repository with native VPN modules:

- Android uses `VpnService` plus the embedded tunnel engine.
- iOS uses a `NetworkExtension` packet tunnel provider.

### Future Dedicated Exit Mode

In a later phase, volunteers should be able to choose one of two modes:

- Direct exit mode: volunteer connects directly to destination websites.
- Entry relay mode: volunteer accepts client traffic and routes it to a dedicated exit server.

Entry relay mode protects volunteers from being the destination-visible exit IP. The broker descriptor should therefore keep `exit_mode` as an explicit field from the start.

## Current Data Flow

```mermaid
sequenceDiagram
    participant V as Volunteer CLI
    participant B as Broker
    participant C as Mobile Client
    participant W as Public Website/App

    V->>V: Start Xray VLESS Reality listener
    V->>B: Register relay descriptor
    loop Every heartbeat interval
        V->>B: Heartbeat
    end
    C->>B: Request relay candidates
    B-->>C: Ranked short-lived VLESS Reality descriptors
    C->>V: Connect using VLESS Reality Vision
    V->>W: Direct exit traffic
    W-->>V: Response
    V-->>C: Proxied response
```

## Trust Boundaries

Volunteer relays are not inherently trusted. The client should treat each volunteer as a network provider:

- Use HTTPS/TLS to destination sites whenever possible.
- Avoid sending broker credentials to volunteers.
- Rotate relay credentials frequently.
- Prefer short-lived relay descriptors.

The broker should treat volunteer registrations as untrusted input:

- Require authentication for volunteers outside local development.
- Validate ports, hostnames, protocol fields, and advertised capabilities.
- Expire inactive relays aggressively.

The current scaffold advertises one generated VLESS client ID per volunteer process. That is acceptable for early private testing, but public versions should issue short-lived per-client credentials and push them into Xray dynamically.

## Open Design Decisions

- Public reachability probing: the broker should eventually verify the advertised endpoint from outside the volunteer network, especially for IPv6 hosts where residential firewalls may block inbound connections even without CGNAT.
- Relay selection: global score is implemented; later add country/ASN-specific scoring and reputation.
- Abuse controls: add per-relay limits, destination policy, reporting, and blocklists before public rollout.
- Mobile engine: choose whether to embed a maintained Xray-compatible engine or call into a separate core.
