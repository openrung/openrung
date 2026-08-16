# Desktop Clients

OpenRung ships a Wails desktop app for Linux, macOS, and Windows. It runs a
mixed HTTP/SOCKS proxy on loopback, requiring no administrator privileges.
This repository also contains a terminal client (`cmd/client`) that drives the
same connection engine from a TUI and can additionally route the whole device
through a TUN interface. Both are described below.

## Direct-first WSS fallback

The Wails desktop app tries the selected relay's public Reality endpoint first.
Only a genuine remote network/data-path failure can unlock that relay's own
signed WSS/CDN fronts; successful direct connections and local sing-box,
configuration, permission, or OS-integration failures never request a ticket.
The complete policy and failure model are documented in
[`wss-fallback.md`](wss-fallback.md).

The app consumes `github.com/openrung/openrung/wsscore` for strict front
validation and the opaque WebSocket/yamux transport shared with every
relay-local sidecar. It consumes
`github.com/openrung/openrung/brokerapi` for signed directory fetches, the
ticket HTTP exchange, telemetry posting, URL hardening, and the shared broker
TLS transport. Direct-first selection, multi-front scheduling, relay-health
and telemetry lifecycle decisions, sing-box configuration, recovery, and UI
state remain desktop application responsibilities. Android and iOS are
maintained and released outside this repository and implement the same
direct-first contract through their own platform orchestration over pinned
`brokerapi` and `wsscore` releases.

## Desktop App: Local Proxy

The desktop app chooses one local port on first launch and persists it under
the user's OpenRung configuration directory. Disconnecting, reconnecting, and
switching relays therefore keep the same endpoint. The terminal client in proxy
mode reads and writes the same state, so both clients offer the same endpoint.
Set a specific port before launch when needed:

```sh
OPENRUNG_PROXY_PORT=17890 ./OpenRung
```

The value must be an unused port from 1 through 65535. An explicit override is
not persisted; launches without it return to the per-install port. OpenRung
fails with a clear error when the chosen port is occupied rather than silently
changing an endpoint already configured in a browser or shell. If the config
directory cannot be written, the current connection still works and Settings
warns that the endpoint may change on the next launch.

The bind host is always `127.0.0.1`. The mixed proxy has no authentication, so
allowing a LAN-facing bind address would turn the desktop app into an open
proxy. Loopback prevents remote-network access, but other accounts on the same
multi-user computer may still be able to reach the listener.

### POSIX shell applications

macOS and Windows are configured through their system proxy settings. Linux
desktop integration is not implemented yet, and command-line applications do
not necessarily honor those OS settings. On Linux and macOS, the Settings
screen of both the desktop app and the terminal client therefore also provides
two copyable POSIX-shell commands:

1. **Enable in this shell** sources OpenRung's generated, port-qualified
   `proxy-env-<port>.sh` helper and calls `openrung_proxy_on`. The Settings
   button is enabled only while the tunnel is connected.
2. **Restore this shell** calls `openrung_proxy_off`. Run it after a disconnect,
   terminal tunnel failure, app quit, or crash so the shell does not retain a
   dead loopback proxy.

The helper preserves and restores existing values and whether each variable
was unset or exported. While enabled it sets the lowercase and uppercase
HTTP/HTTPS variables to the local HTTP proxy and the lowercase and uppercase
`ALL_PROXY` variables to its SOCKS endpoint. It does not change `NO_PROXY`.

This is proxy mode, not a fail-closed full-device VPN. Applications that ignore
the OS or shell proxy configuration connect directly, as do destinations
excluded by an existing `NO_PROXY` value. Users in environments where any
direct connection is unsafe should configure applications accordingly.

A desktop app cannot modify the environment of a shell that is already
running, which is why activation is an explicit command. The proxy endpoint is
available only while OpenRung is connected. If OpenRung itself is relaunched
from an activated shell, it recognizes and removes only its own inherited
loopback proxy values in the child process, restoring any previously exported
upstream proxy values so broker discovery can bootstrap; the parent shell
remains unchanged. Helpers are port-qualified (for example,
`proxy-env-46685.sh`) so concurrent app instances cannot rewrite each other's
copied command.

## Terminal Client

`cmd/client` is an interactive terminal client for Linux, macOS, and Windows
(proxy mode everywhere; TUN mode on macOS and Linux).
It is a view over the same `internal/connectcore` engine the desktop app
drives, so relay ranking, the connect ladder, direct-first WSS fallback, NAT
punching, telemetry, and mid-session failover behave identically in both — with
the two TUN-mode differences noted under [Capture modes](#capture-modes). The
view layer holds no connection logic.

### Requirements

- A local `sing-box` binary. Use sing-box 1.14 or newer so the generated TUN
  config can install native DNS settings for the tunnel. Install it with
  Homebrew if needed:

  ```sh
  brew install sing-box
  ```

- Nothing else for proxy mode. TUN mode additionally needs root, and is macOS
  and Linux only — see [Capture modes](#capture-modes).
- Against the public fleet, no broker URL is needed: the client races the
  built-in HTTPS broker fronts. Working against a local deployment needs a
  running broker with at least one registered relay, selected with `-broker`.

### Launch

```sh
go run ./cmd/client
```

A bare invocation and `connect` are the same thing; flags seed the initial
settings. To work against a local broker:

```sh
go run ./cmd/client connect -broker http://localhost:8080
```

Connecting is a keypress, not a flag: the client starts disconnected, and `c`
connects with whatever the Settings view holds.

### Views and keys

Four views, switched with `1`–`4`, `tab`, and `shift+tab`:

| View | What it shows |
| --- | --- |
| **Status** | Connection state, relay label, country, foundation/volunteer class, transport path (direct, punched, or WSS front), session duration, health-probe state, the latest failover/fallback activity, and the capture mode with its local proxy endpoint |
| **Relays** | The ranked relay directory — country, measured latency, node class — plus the persisted recents row. `↑`/`↓` moves, `enter` pins the highlighted relay and connects to it, `x` clears the pin |
| **Logs** | Engine and sing-box output in a scrollable ring buffer |
| **Settings** | Broker URL override, capture mode, relay targeting by id/label/country, and the shell proxy helper |

Global keys: `c` connect, `d` disconnect, `r` refresh the relay directory,
`q` (or `ctrl+c`) quit. Quitting tears down the tunnel, restores the system
proxy, and flushes telemetry before the process exits.

In Settings, `↑`/`↓` moves between fields and `enter` acts on one: text fields
open an inline editor (`enter` applies, `esc` cancels), the Mode field toggles
the capture mode, and the Shell proxy field prints the copyable commands.

### Capture modes

**Proxy mode is the default** and needs no privileges. It runs a loopback mixed
HTTP/SOCKS inbound and points the system proxy at it, exactly like the desktop
app — including the stable per-install port and the `OPENRUNG_PROXY_PORT`
override described under [Desktop App: Local Proxy](#desktop-app-local-proxy),
and the shell helper under [POSIX shell applications](#posix-shell-applications).
Only proxy-aware applications are carried.

**TUN mode** captures the whole device instead. It is available on macOS and
Linux; see [Windows](#windows) below. Pass `--tun`, or toggle the Settings Mode
field while disconnected:

```sh
sudo go run ./cmd/client connect --tun
```

The generated config uses:

- `tun` inbound with `auto_route: true` and `strict_route: true`.
- `dns_mode: hijack`, so sing-box installs tunnel DNS settings and intercepts
  port 53 DNS requests.
- DNS servers detoured through the proxy.
- `route_exclude_address` for the literal relay IP (and, on a punched path, the
  relay's reflexive UDP IP), so the client's own connection to the relay stays
  on the real network interface instead of being routed back into the TUN.
- VLESS Reality Vision outbound from the selected relay descriptor.
- Route final set to the proxy outbound.
- `-mtu` when given; otherwise 1500.

TUN mode needs the privileges to create the tunnel device and rewrite the
routing table. Without them the client refuses before it dials anything, and
says how to rerun:

```text
TUN mode needs root privileges to create the tunnel device: rerun as
`sudo client connect --tun`, or drop --tun to use proxy mode (no privileges
needed)
```

The capture mode is fixed for the length of a session: changing it while
connected is refused, so disconnect first.

A connect is reported as connected only once the kernel actually routes
internet-bound traffic out of the tunnel address — not merely once the device
appears. The tunnel address sits in the range Docker carves bridge networks
from, so "an interface holds this address" would be satisfied on a host that
has never run OpenRung, and the end-to-end probe would then pass over the
ordinary network and report an untunneled session as connected.

#### Windows

TUN mode is refused on Windows, whatever the process token. Elevation is not
the blocker; graceful teardown is. Removing the routes and DNS settings
sing-box installed requires asking it to stop, and this client has no way to do
that on Windows: `os.Interrupt` is unsupported there, and the console control
events that would substitute cannot reach a child started with
`CREATE_NO_WINDOW`, which has no console. Every disconnect would end in a
force-kill, leaving the host routing traffic at an interface that is gone.

Proxy mode is unaffected — it holds no device and no routes, so a forced stop
costs nothing — and remains the full-featured mode on Windows. The refusal will
be lifted once the client can stop sing-box gracefully there.

#### Divergences from proxy mode

Two engine behaviors differ in TUN mode, both because a full-device tunnel owns
the default route:

- **The WSS/CDN fallback is unavailable.** The WSS bridge dials its front from
  the client process, which the TUN would capture back into the tunnel the
  bridge is carrying. Only the relay's own address can be excluded from the
  routes, not a CDN's, so a relay whose direct path fails is left failed rather
  than looped. Use proxy mode where the fallback matters.
- **Mid-session health failures always trigger a failover.** Proxy mode first
  checks whether the local network is alive at all, and rides out a Wi-Fi blip
  or a laptop sleep without churning relays. In TUN mode every reference point
  is reached through the tunnel under suspicion, so that check cannot answer;
  the recovery pass tears the tunnel down first, which also restores the normal
  network if the outage was local.

### Headless subcommands

For scripts and service managers, three non-interactive commands keep their
historical flags:

```sh
# Connect and stream engine logs until interrupted (SIGINT or SIGTERM).
go run ./cmd/client connect -headless -broker http://localhost:8080

# Fetch relay candidates and print the selected usable relay.
go run ./cmd/client check -broker http://localhost:8080

# Write a sing-box TUN config for the selected relay without connecting.
go run ./cmd/client config \
  -broker http://localhost:8080 \
  -out openrung-sing-box.json
```

`connect -headless` drives the same engine as the TUI — full candidate ladder,
WSS fallback, punching, and failover — and combines with `--tun`. It exits
non-zero on a terminal failure, and an interrupt disconnects cleanly, restoring
the system proxy. `check` and `config` are fetch-and-print only: they open no
telemetry session and start no tunnel.

Common flags: `-broker` (empty races the built-in HTTPS fronts), `-relay-id`
and `-relay-label` to pin a target, `-relay-family` for `check`/`config`,
`-mtu` for the TUN device, and `-sing-box` to point at a specific binary. Run
`go run ./cmd/client help` for the full list.

Some pre-rewrite flags are still parsed but no longer honored, and say so on
startup rather than failing: `-limit` and `-config-out` are the engine's
concern now, and `-mtu` applies only to a TUN device.

### Reuse notes

Everything platform-specific reaches the engine through the narrow interfaces
in `internal/connectcore/interfaces.go`, so Linux, macOS, and Windows share one
implementation.

Windows TUN mode is disabled pending graceful shutdown support, as described
under [Windows](#windows) above; proxy mode is unaffected. Re-enabling it needs
a way to ask sing-box to stop and unwind its routes and DNS, and only then the
install checks around the tunnel driver it uses.
