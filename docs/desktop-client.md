# Desktop CLI Client

The current desktop client is a command-line end-user client. It fetches relay
candidates from the broker, selects the first usable direct-exit VLESS
Reality Vision relay, generates a sing-box TUN config, and runs sing-box to
route device traffic through that relay.

The first operational target is macOS, but the implementation is in Go so the
broker client, relay selection, sing-box config generation, and process runner
can be reused by Linux and Windows clients later.

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
