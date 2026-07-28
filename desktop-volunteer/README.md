# OpenRung Volunteer (desktop)

A cross-platform GUI (macOS / Linux / Windows) that lets home users volunteer
their computer as an OpenRung relay — the same relay that powers the Docker
deployment (`deploy/relay/`), wrapped in a point-and-click app with
start/stop, live status, and settings.

## Architecture

The UI is a Wails v2 app with a React frontend (`frontend/`), the same stack
as the sibling desktop client (`desktop/`). `volunteerservice/` is the
Wails-bound bridge — it owns settings persistence, state events, and log
capture, and stays free of Wails imports so it is unit-testable.
Underneath, the embedded relay engine from `internal/relayruntime/engine`
registers with the broker and drives a bundled, external
[Xray-core](https://github.com/XTLS/Xray-core) (`xray`) process for the
VLESS + REALITY data plane.

ConnectionObserver owns the public TCP listener and forwards accepted traffic
to Xray on loopback. The optional `directsetup/` package performs only the
least-privilege local preparation needed for that listener; Xray itself never
needs permission to bind TCP 443.

## Development

Prereqs: Go 1.25, Node 22, the Wails CLI
(`go install github.com/wailsapp/wails/v2/cmd/wails@v2.12.0`), and an `xray`
binary on PATH. In dev the app resolves `xray` from PATH (plus common install
dirs); packaged builds find it next to the executable, or in
`Contents/Resources` inside the macOS .app — see `toolpath.go`.

```sh
wails dev     # live-reload development
wails build   # bare binary — xray NOT bundled; use the packaging scripts below
```

## Packaging

Each script builds the app and bundles a platform-matching `xray` next to it
(macOS: inside the .app), plus license notices and
[`DIRECT_CONNECTIONS.md`](DIRECT_CONNECTIONS.md). Point `XRAY` at the binary
to bundle, or have `xray` on PATH:

```sh
XRAY=/path/to/xray scripts/package-macos.sh                   # OpenRungVolunteer.app (ad-hoc signed)
XRAY=/path/to/xray scripts/package-linux.sh -tags webkit2_41  # OpenRungVolunteer-linux-x86_64.tar.gz
# Windows (pwsh):
$env:XRAY = 'C:\path\to\xray.exe'; scripts\package-windows.ps1  # OpenRungVolunteer-windows-amd64.zip
```

Licensing: the app is GPL-3.0-or-later; Xray-core is MPL-2.0, bundled
unmodified and run as a separate process (aggregation, not linking). See
[`../THIRD_PARTY_NOTICES.md`](../THIRD_PARTY_NOTICES.md).

The scripts never elevate or alter the packaging machine. Windows and Linux
setup happens only after the volunteer clicks the action in the installed app.
The current macOS app remains ad-hoc signed and does not contain the privileged
helper required for safe TCP 443 setup. See [`PACKAGING.md`](PACKAGING.md) for
artifact-specific release gates and the exact macOS blocker.

## Release

CI (`.github/workflows/desktop-volunteer-release.yml`) builds all three
platforms with a pinned Xray-core (v26.3.27, same pin as
`deploy/relay/Dockerfile`), SHA-256-verified against the release `.dgst`
on every platform. [`VERSION`](VERSION) is the canonical version source for
the Go relay runtime (`desktop-volunteer/X.Y.Z` as reported to the broker) and
the About screen; `wails.json` carries an `info.productVersion` copy for the
native package metadata, and CI rejects drift between the two. Push the
exactly matching `desktop-volunteer-vX.Y.Z` tag to publish a GitHub release
with all three artifacts; CI rejects a mismatched tag. A manual
`workflow_dispatch` run builds artifacts only. (The 0.1.0 release predates
this scheme and lives on the retired `volunteer-v0.1.0` tag.)

CI compiles and packages but cannot validate a real UAC/polkit elevation path,
host firewall, capability-supporting target filesystem, router, or cloud
firewall. Do not call a platform's direct setup production-ready until the
actual packaged artifact passes the checklist in
[`PACKAGING.md`](PACKAGING.md). In particular, macOS TCP 443 setup is not
complete in the current ad-hoc-signed artifact.

## Volunteering means being an exit

Traffic from people in censored regions exits to the internet from the
volunteer's IP address — destination sites and abuse desks see the volunteer
as the source. The app exists to make that an informed, revocable choice:
volunteering only happens after an explicit start, status is visible while
the relay runs, and stopping or quitting tears it down. Read
[`../docs/security-abuse.md`](../docs/security-abuse.md) for the current
risk posture and the planned volunteer-protection controls before running a
relay.

## Network reality (today)

The app ships with the project's RelayHub configured by default, so it runs in
**Automatic** mode. It probes direct TCP 443 first, then the configured
alternate port (8443 for default and existing installations). Candidates are
deduplicated if the alternate is already 443. Only after every viable direct
candidate fails does it tunnel through RelayHub, which lets NAT'd/IPv4-only
homes volunteer too. The hub's self-signed certificate is pinned in the binary
(see `DefaultHubCertFingerprint`), so the connection is authenticated without
a CA.

In Automatic mode, direct is chosen only when RelayHub's nonce callback
**positively confirms** the selected host and port — never guessed — so
Automatic never advertises a merely local or possibly firewalled listener. A
permission failure, an occupied port, an external callback failure, and an
unavailable probe API remain distinct outcomes. An external failure is not
mislabeled as an OS permission problem.

Automatic mode re-probes periodically in the same 443-then-alternate order, so
a tunnel can be promoted when a candidate becomes reachable. A direct relay
keeps serving through a hub outage. Users who want to run fully independently
of the shared hub can pick **Direct only** under Settings → Advanced. Direct
only honors exactly the configured port and never falls back through RelayHub.
It also does not use RelayHub's reachability check, so choose it only when the
computer already has a publicly reachable address and inbound TCP is
configured.
Point `Hub address` at your own hub to use a different one (its own TLS trust
applies; the built-in pin is dropped).

## One-time TCP 443 setup

Where the current platform/install is eligible, Settings exposes **Enable
direct connections**; the app never elevates on open or runs the GUI as
Administrator/root. Declining or failing setup does not stop volunteering in
Automatic mode: the app continues with any distinct configured alternate port
and then RelayHub.

- **Windows:** UAC manages one fixed-name, executable-scoped Windows Defender
  Firewall rule for inbound TCP 443. Binding 443 itself needs no elevation.
- **Linux:** when the GUI binary and every ancestor in its installed path are
  root-owned and non-writable, `pkexec` grants only
  `CAP_NET_BIND_SERVICE` to that exact file. Xray receives no capability. If
  the host still treats 443 as privileged, a user-writable portable tarball
  copy is deliberately ineligible for elevation and Automatic continues with
  the alternate port/RelayHub. Reopen after setup; removal also requires an
  app restart, and replacing the binary normally requires setup again.
- **macOS:** safe support requires a signed/notarized minimal
  ServiceManagement helper. The current ad-hoc-signed package cannot install
  one, so TCP 443 setup is reported unavailable without a `sudo` or setuid
  workaround.

Local setup does not configure a router/NAT mapping or a Tencent Cloud, AWS, or
other cloud firewall and does not guarantee Internet reachability. Detailed
setup, removal, update, firewall, router, IPv6, and cloud guidance is in
[`DIRECT_CONNECTIONS.md`](DIRECT_CONNECTIONS.md).
