# Network lab and end-to-end scenarios

`make e2e` runs the real broker, relay hub, relays (with Xray) and the
command-line client against each other on a simulated internet, and checks
that a user's traffic actually leaves through the relay it should.

Everything runs inside **one container that has no network of its own**. The
simulated internet is built from Linux network namespaces inside it, so
scenarios can put the user and the relays on any kind of network — behind NAT,
on a slow or lossy link, behind a firewall — without touching the host or the
real internet. The product binaries run unmodified.

```sh
make e2e                                    # build the image, run every scenario
make e2e E2E_ARGS='-test.short'             # skip the scenarios that wait minutes
make e2e E2E_ARGS='-test.run Volunteer'     # run some scenarios
make e2e-lab                                # leave a lab running to explore by hand
```

You need Docker on a Linux kernel (Docker Desktop's VM counts). Nothing else
is installed on the host, including Go.

## The simulated internet

```text
                         inet  192.0.2.1
                 DNS + web for every name and address
                                 |
   broker  198.51.100.10 ---+    |
   hub     198.51.100.30 ---+--[core]--+--- gw-user   203.0.113.10 --- user  192.168.10.10
   relay-a 198.51.100.41 ---+ backbone +--- gw-mobile 203.0.113.40 --- user  10.20.0.10 (2nd uplink)
   relay-b 198.51.100.42 ---+          +--- gw-vol    203.0.113.20 --- vol   192.168.20.10
                                       +--- cgn       203.0.113.30 --- gw-vol2 100.64.0.10 --- vol2 192.168.30.10
```

- Every box is a network namespace ("node"). Public nodes hang off the
  backbone router `core` on their own link; `gw-*` and `cgn` are NAT routers
  with one private node behind each.
- `user` runs the client. It has two uplinks: a home router and a mobile one.
- `relay-a` and `relay-b` are relays with public addresses. `vol` is a
  volunteer behind a home router, published through the relay hub. `vol2`
  (started on demand) is a volunteer behind a home router behind a
  carrier-grade NAT.
- `inet` stands in for the rest of the internet. Its DNS resolves every
  hostname to itself, it answers on every address the lab does not assign, and
  it serves HTTPS for any hostname with certificates from a lab CA that the
  client is told to trust. `http://<anything>/ip` returns the address the
  request came from, which is how scenarios see where traffic exits.
- Public addresses come from the documentation ranges (RFC 5737).

### How the stock binaries are kept happy

No product code knows about the lab. The lab meets the binaries' existing
requirements from the outside:

| Requirement | How the lab meets it |
| --- | --- |
| The client accepts a plain-HTTP, non-production broker only on loopback. | `127.0.0.1:8080` in `user` is address-translated to the broker, so the connection still crosses the simulated network and fails when that network does. |
| The client reports "connected" only after an HTTPS check through the tunnel. | `inet` answers the check for any hostname; the client trusts the lab CA through `SSL_CERT_FILE`. |
| A relay's Reality handshake needs a TLS 1.3 site to borrow. | The default site resolves to `inet`. |
| Relays refuse to forward to private addresses. | The internet stand-in has a public (documentation) address. |
| The broker looks up relay locations online. | Switched off with `OPENRUNG_GEOIP_ENDPOINT=off`. |

Relays are started through `deploy/relay/entrypoint.sh` with the Xray pinned
by `deploy/relay/Dockerfile`, so the lab also covers the entrypoint's
environment mapping. Leases and heartbeats are shortened (20 s and 5 s) so a
relay disappears from the broker in seconds.

## Scenarios

Each scenario is a Go test (build tag `e2e`) named after the situation it
puts a user or a relay in.

| Situation | Test |
| --- | --- |
| **User** | |
| Behind a home router, through a relay with a public address | `TestConnectThroughDirectRelay` |
| Through a volunteer that is itself behind NAT | `TestConnectThroughVolunteerBehindNAT` |
| Network refuses one relay (silently, with a reset, with ICMP) | `TestUserNetworkBlocksOneRelay` |
| Network refuses every relay with a public address | `TestUserNetworkBlocksAllDirectRelays` |
| Firewall allows web ports only | `TestUserBehindWebOnlyFirewall` |
| Broker unreachable, then back | `TestBrokerUnreachable` |
| Relay dies mid-session *(slow)* | `TestRelayDiesMidSession` |
| Own link drops and returns mid-session *(slow)* | `TestUserLinkOutageIsNotARelayFailure` |
| Moves from one access network to another mid-session | `TestUserChangesNetwork` |
| Slow, lossy or high-latency access link | `TestConnectOverDegradedUserLink` |
| Rate-limited link bounds tunnel throughput | `TestRateLimitBoundsTunnelThroughput` |
| Whole-device (TUN) mode | `TestWholeDeviceTunnel` |
| **Relay** | |
| Crashes and restarts, keeps its identity | `TestRelayCrashAndRestart` |
| Loses its path to the broker, then regains it | `TestRelayLosesBroker` |
| Volunteer's home link drops and returns | `TestVolunteerLinkOutage` |
| Volunteer's router reboots and forgets its mappings | `TestVolunteerRouterReboot` |
| Volunteer behind carrier-grade NAT | `TestVolunteerBehindCarrierGradeNAT` |
| Volunteer with a forwarded port, serving directly | `TestVolunteerWithForwardedPort` |
| Volunteer on a poor uplink | `TestVolunteerOnDegradedUplink` |

Scenarios marked *slow* wait on the client's real health-check timers and take
one to two minutes each; `-test.short` skips them. Two kinds of scenario skip
themselves when the kernel cannot support them: added latency needs the
`netem` qdisc, and TUN mode needs IPv6.

### Writing a scenario

```go
// The user's network refuses every relay with a public address.
func TestUserNetworkBlocksAllDirectRelays(t *testing.T) {
	scenario(t) // stop the client and restore the lab when the test ends
	for _, addr := range []string{lab.RelayAAddr, lab.RelayBAddr} {
		_, err := lab.Block("gw-user", lab.Match{Dst: addr}, lab.Drop)
		must(t, err)
	}
	client, via := connect(t) // start the client, wait for "connected"
	if via != "vol" {
		t.Fatalf("connected via %q, want vol", via)
	}
	exitIP(t, client, lab.GwVolAddr) // where does the internet see the traffic from?
}
```

Describe the real-world situation in the comment, change the network with the
`lab` package, and assert on what a user would observe: which relay the client
reports, where traffic exits, what the client logs.

## Exploring by hand

`make e2e-lab` leaves the lab running. From another terminal:

```sh
docker exec -it openrung-lab sh
lab status                          # services, relays the broker lists, impairments
lab client                          # connect, print the relay and exit address
lab fetch https://example.org/      # fetch through the client's proxy
lab logs client                     # any service's log; also in /var/log/lab/
lab sh user                         # a shell inside a node (ip, curl, tcpdump, ...)
lab -v net shape user mobile-3g     # -v prints the ip/tc/iptables commands used
```

Ways to change the network (`lab net list` shows nodes, links and profiles):

| Situation | Command |
| --- | --- |
| A link of a given quality | `lab net shape <link> <profile>` or `lab net shape <link> delay=80ms jitter=20ms loss=2 rate=2mbit` |
| A node refuses a destination | `lab net block <node> <address>[:port] [tcp\|udp] [drop\|reset\|unreachable]` |
| A firewall that allows only some ports | `lab net allow-only <node> tcp:80,tcp:443,udp:53` |
| A link goes dead / comes back | `lab net down <link>` / `lab net up <link>` |
| A router with symmetric NAT | `lab net nat <router> symmetric` (back with `cone`) |
| A port forwarded on a router | `lab net forward <router> <port> <host:port>` |
| A relay or the broker crashes | `lab kill <service>`, later `lab start <service>` |
| Back to a clean network | `lab net clear` |

A link is named after the node on its far side from the backbone: `user` is
the user's home LAN, `gw-user` is the home router's uplink, `relay-a` is that
relay's uplink. Shaping applies in both directions. Blocking applies at the
node you name, so `lab net block gw-user 198.51.100.41` is the user's network
refusing a relay, while `lab net block relay-b 198.51.100.10` is a relay that
cannot reach the broker.

The link profiles (`broadband`, `dsl`, `mobile-4g`, `mobile-3g`, `mobile-edge`,
`satellite`, `lossy-wifi`, `congested`) are round illustrative numbers, not
measurements.

## Layout

| Path | What it holds |
| --- | --- |
| `Dockerfile` | The lab image: every binary under test, Xray, and the network tools. |
| `lab/` | The library: topology, network conditions, service supervision, client driver, internet stand-in. |
| `cmd/lab/` | The `lab` command, the image's entrypoint. |
| `*_test.go` | The scenarios. |

## Running elsewhere

The lab needs a privileged container on a Linux kernel with network
namespaces, veth and iptables NAT; it does not need outbound network access
at run time, and concurrent labs on one host do not interfere. That makes the
same `make e2e` usable in CI and in sandboxed agent sessions. Building the
image does need network access (base images, Go modules, the Xray release);
pass a proxy or other build options with `E2E_BUILD_FLAGS`.

## What the lab does not cover

- The published container images' runtime settings (user, capabilities,
  read-only filesystem): the lab runs the same binaries and entrypoint, not
  the images.
- The desktop and mobile apps. They share `connectcore` with the command-line
  client, which is what runs here.
- Host-level tooling such as `volunteer-up.sh` and `egress-limit.sh`, which
  have their own tests under `deploy/relay/`.
