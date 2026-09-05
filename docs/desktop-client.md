# Desktop Clients

OpenRung ships a Wails desktop app for Linux, macOS, and Windows. It runs a
mixed HTTP/SOCKS proxy on loopback, requiring no administrator privileges, and
installs as a single executable with the tunnel engine linked in.
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

## Desktop App: Bundled Engine

The app statically links the sing-box tunnel engine (`internal/singboxruntime`,
pinned in the root `go.mod` — the same engine and pin as the terminal client)
and starts a tunnel by re-invoking its own executable as the sing-box child
process. A release package is therefore one binary plus its license notices:
nothing to install, nothing to find on `PATH` — which is what a Finder- or
launcher-started app never reliably had. The engine's supervision is unchanged
from an external binary: it starts the child, watches its exit status, and
interrupts it (then hard-kills after a grace period) on disconnect.

`OpenRung version` prints the app version and the linked engine's version and
build tags. Every package is built with `-tags with_utls` (Reality, which every
relay endpoint needs) and `with_external_windivert`
(`desktop/scripts/versioned-wails-build.mjs` injects both, and
`verify-bundled-engine.mjs` refuses to package a binary that lost one). A plain
`go build` still compiles, but the resulting app cannot dial any relay and says
so in that version output.

Windows can deliver no interrupt to the child, so the bundled engine's stop
channel there is its stdin: the runner holds a pipe to the child's stdin open
and closes it to request a stop, and the child unwinds like it was
interrupted. The pipe also closes if the app dies without running teardown —
crash, `Stop-Process`, Task Manager — so an orphaned tunnel child stops itself
instead of running unsupervised. An external `OPENRUNG_SING_BOX` binary does
not speak the stdin protocol: on Windows its teardown is a hard kill after the
grace period and a kill-on-close job object reaps it if the app dies first —
both cost nothing in proxy mode, the only mode the desktop app offers, because
no device or routes are held. (The job object is deliberately NOT applied to
the bundled child: closing a kill-on-close job's last handle terminates the
child instantly, which would race and defeat the pipe's graceful teardown. See
[Windows](#windows) below for what all this means for the terminal client's
TUN mode.)

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

`cmd/client` is an interactive terminal client for Linux, macOS, and Windows,
with proxy and TUN capture modes on all three (TUN on Windows requires the
bundled engine and an elevated terminal; see [Windows](#windows)).
It is a view over the same `connectcore` module engine the desktop app
drives, so relay ranking, the connect ladder, direct-first WSS fallback, NAT
punching, telemetry, and mid-session failover behave identically in both — with
the two TUN-mode differences noted under [Capture modes](#capture-modes). The
view layer holds no connection logic.

### Requirements

- No sing-box install: the client bundles its own sing-box engine (the pinned
  version is in the root `go.mod`, printed by `openrung-client version`) and
  runs it by re-invoking its own binary as the tunnel child process. Build
  from source with `-tags with_utls,with_external_windivert` — as the
  Makefile and the release workflow do — or the bundled engine cannot dial
  any relay's Reality endpoint (with_utls), and Windows builds embed a driver
  the client never uses (with_external_windivert keeps WinDivert out; see
  THIRD_PARTY_NOTICES.md §8). Pass `-sing-box <path>` to substitute an
  external sing-box binary
  (1.14 or newer, so the generated TUN config can install native DNS settings
  for the tunnel).
- Nothing else for proxy mode. TUN mode additionally needs root (an elevated
  terminal on Windows) — see [Capture modes](#capture-modes).
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

Connecting is a keypress, not a flag: the client starts disconnected, and
`enter` on the Relays list connects — to the highlighted relay, or ranked
automatically via the **Auto select** row at the top of the list. There is no
stored relay target in the interactive client at all: what you highlight is
what you get, every time. Settings controls the broker and capture mode;
`-relay-id`/`-relay-label` apply to `-headless`, `check`, and `config` only.

### Views and keys

Three views, switched with `1`–`3`, `tab`, and `shift+tab`:

| View | What it shows |
| --- | --- |
| **Relays** | The ranked relay directory — country, measured latency, and node class — under an **Auto select** row. `↑`/`↓` moves; `enter` connects to the highlighted row: a relay connects to exactly that relay, Auto select takes the ranked pick. The connected relay's row renders bold |
| **Logs** | Engine and sing-box output in a scrollable ring buffer |
| **Settings** | Broker URL override, capture mode, and the shell proxy helper |

There is no Status view. Connection state lives in the **status bar**, the
line directly above the key help, so it is readable from whichever view you
are on rather than only the one you navigated to. It carries everything the
old Status view did — state, relay and its foundation/volunteer class,
country, transport path (direct, punched, or WSS front), health-probe state,
the latest failover or fallback activity, capture mode with its local proxy
endpoint, broker, and the last error — on one line that scrolls
horizontally when it overflows. While connected, the relay label with its
country flag and the session duration are pinned to the bar's right edge
instead of scrolling, so a glance at the corner always answers "to what" and
"for how long"; on a terminal too narrow for both, the label yields and the
duration stays. The bar is the connection signal too: red while disconnected or
failed, yellow through every transition, green while connected. The key-help
line below it never changes color, so the bar is the only thing on screen
that does.

Global keys: `d` disconnect, `r` refresh the relay directory,
`0` cycles English/中文/русский, and `q` (or `ctrl+c`) quit. The language key
is a digit on purpose: a Cyrillic or Greek layout carries no Latin letters, so
a letter would be untypeable for the reader who most needs to switch away from
a language they cannot read. Scrolling the Logs pager, moving with `↑`/`↓`, and
acting with `enter` are left out of the footer help as conventions the reader
already has. If the help is still too narrow to fit, it scrolls the same way
the status bar does. Quitting tears down the tunnel, restores the system proxy,
and flushes telemetry before the process exits.

In Settings, `↑`/`↓` moves between fields and `enter` acts on one: Broker URL
opens an inline editor (`enter` applies, `esc` cancels), Mode toggles the
capture mode, and Shell proxy prints the copyable commands.

### Capture modes

**Proxy mode is the default** and needs no privileges. It runs a loopback mixed
HTTP/SOCKS inbound and points the system proxy at it, exactly like the desktop
app — including the stable per-install port and the `OPENRUNG_PROXY_PORT`
override described under [Desktop App: Local Proxy](#desktop-app-local-proxy),
and the shell helper under [POSIX shell applications](#posix-shell-applications).
Only proxy-aware applications are carried.

**TUN mode** captures the whole device instead. It is available on macOS,
Linux, and Windows (with two Windows-specific requirements; see
[Windows](#windows) below). Pass `--tun`, or toggle the Settings Mode field
while disconnected:

```sh
sudo go run ./cmd/client connect --tun
```

The generated config uses:

- `tun` inbound with `auto_route: true` and `strict_route: true`.
- `dns_mode: hijack`, so DNS queries addressed to the tunnel's own DNS
  address (172.19.0.2) are answered by sing-box's resolver. sing-box points
  the system at that address only on Windows and on Linux hosts running
  systemd-resolved (it shells out to `resolvectl`); other Linux setups and
  macOS keep their existing resolver.
- A route rule that hijacks *any* port 53 traffic into the same resolver, so
  DNS still works, and stays inside the tunnel, when the system resolver was
  not repointed. Without it those queries reach the TCP-only relay outbound
  as UDP, sing-box drops them (`UDP is not supported by outbound: proxy`),
  and every name lookup on the machine fails. This rule runs ahead of the
  split-tunnel LAN bypass, so a LAN resolver (the router, a Pi-hole, a
  local AdGuard) is never consulted while connected, even with the LAN
  bypass on: names only that resolver knows, such as router-served `.lan`
  hosts, do not resolve, and its blocklists do not apply. That is the
  behaviour Windows, Android and iOS already had, because they point the
  system at the tunnel resolver; the rule makes macOS and other Linux
  setups match, and keeps a bypassed LAN from becoming a plaintext DNS leak.
- DNS servers detoured through the proxy over TCP. The relay outbound carries
  no UDP at all; UDP 443 (QUIC) is rejected outright so browsers fall back to
  TCP immediately, and every other UDP flow is dropped.
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

(On Windows the refusal names an elevated terminal instead of sudo.)

The capture mode is fixed for the length of a session: changing it while
connected is refused, so disconnect first.

A connect is reported as connected only once the kernel actually routes
internet-bound traffic out of the tunnel address — not merely once the device
appears. The tunnel address sits in the range Docker carves bridge networks
from, so "an interface holds this address" would be satisfied on a host that
has never run OpenRung, and the end-to-end probe would then pass over the
ordinary network and report an untunneled session as connected.

#### Windows

TUN mode on Windows needs two things the other platforms express as just
"root":

- **An elevated terminal.** Creating the wintun adapter requires
  Administrator, so run the client from a terminal started with "Run as
  administrator". A non-elevated `--tun` is refused before anything is dialed,
  with that rerun guidance. No driver install is needed: the engine embeds
  `wintun.dll` and loads it from memory.
- **The bundled engine.** Removing the routes and DNS settings sing-box
  installed requires asking it to stop, and Windows offers no signal for that
  (`os.Interrupt` is unsupported; no console control event reaches a
  `CREATE_NO_WINDOW` child). The bundled runtime is stopped through its stdin
  instead — the engine closes the pipe it holds to the child's stdin, and the
  child unwinds its routes and DNS before exiting, including when the client
  itself died without running teardown. An external `-sing-box` binary does
  not speak that protocol, would end every stop in a force-kill that leaves
  the host routing traffic at an interface that is gone, and is therefore
  refused in combination with `--tun` on Windows.

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

Common flags: `-broker` (empty races the built-in HTTPS fronts), `-relay-id`,
`-relay-label`, and `-relay-country` to pin a relay or scope the connect to a
country — a country target keeps mid-session failover within that country
(`-headless`, `check`, and `config`; the interactive client selects from the
Relays list and warns if they are passed), `-relay-family` for
`check`/`config`,
`-mtu` for the TUN device, and `-sing-box` to substitute an external sing-box
binary for the bundled engine. Run `go run ./cmd/client help` for the full
list.

Some pre-rewrite flags are still parsed but no longer honored, and say so on
startup rather than failing: `-limit` and `-config-out` are the engine's
concern now, and `-mtu` applies only to a TUN device.

### Reuse notes

Everything platform-specific reaches the engine through the narrow interfaces
in the `connectcore` module’s `interfaces.go`, so Linux, macOS, and Windows share one
implementation.

Windows TUN mode is disabled pending graceful shutdown support, as described
under [Windows](#windows) above; proxy mode is unaffected. Re-enabling it needs
a way to ask sing-box to stop and unwind its routes and DNS, and only then the
install checks around the tunnel driver it uses.
