# Desktop Clients

OpenRung ships a Wails desktop app for Linux, macOS, and Windows. It runs a
mixed HTTP/SOCKS proxy on loopback, requiring no administrator privileges.
This repository also contains the original command-line TUN client described
below.

## Desktop App: Local Proxy

The desktop app chooses one local port on first launch and persists it under
the user's OpenRung configuration directory. Disconnecting, reconnecting, and
switching relays therefore keep the same endpoint. Set a specific port before
launch when needed:

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
screen therefore also provides two copyable POSIX-shell commands:

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

## Desktop CLI Client

The command-line client fetches relay candidates from the broker, selects the
first usable direct-exit VLESS Reality Vision relay, generates a sing-box TUN
config, and runs sing-box to route device traffic through that relay.

The implementation is in Go so the broker client, relay selection, sing-box
config generation, and process runner can be reused across platforms.

## Requirements

- A running OpenRung broker.
- At least one registered relay.
- A local `sing-box` binary. Use sing-box 1.14 or newer so the generated TUN
  config can install native DNS settings for the tunnel.
- macOS privileges for TUN routing. In practice, run `connect` with `sudo`.

Install sing-box with Homebrew if needed:

```sh
brew install sing-box
```

## Check Relay Selection

```sh
go run ./cmd/client check -broker http://localhost:8080
```

This fetches candidates from:

```http
GET /api/v1/relays?limit=5
```

Then it prints the selected usable relay.

## Generate Config Only

```sh
go run ./cmd/client config \
  -broker http://localhost:8080 \
  -out openrung-sing-box.json
```

The generated config uses:

- `tun` inbound.
- `auto_route: true`.
- `strict_route: true`.
- `dns_mode: hijack`, so sing-box installs tunnel DNS settings and intercepts
  port 53 DNS requests.
- DNS servers detoured through the proxy.
- `route_exclude_address` for literal relay IPs, so the client's own TCP
  connection to the relay stays on the real network interface instead of
  being routed back into the TUN.
- VLESS Reality Vision outbound from the selected relay descriptor.
- Route final set to the proxy outbound.

## Connect on macOS

```sh
sudo go run ./cmd/client connect \
  -broker http://localhost:8080 \
  -sing-box /opt/homebrew/bin/sing-box
```

The client writes a temporary sing-box config, prints the chosen relay, and then
runs:

```sh
sing-box run -c <generated-config>
```

Press `Ctrl-C` to stop. The client forwards the interrupt to sing-box and removes
the temporary config file.

## Reuse Notes

Linux should be able to reuse most of the Go code directly. Windows should reuse
the broker, relay selection, config generation, and command contract, but may
need additional install checks around the Windows tunnel driver used by sing-box.
