# OpenRung Volunteer (desktop)

A cross-platform GUI (macOS / Linux / Windows) that lets home users volunteer
their computer as an OpenRung relay — the same relay that powers the Docker
deployment (`deploy/volunteer/`), wrapped in a point-and-click app with
start/stop, live status, and settings.

## Architecture

The UI is a Wails v2 app with a React frontend (`frontend/`), the same stack
as the sibling desktop client (`desktop/`). `volunteerservice/` is the
Wails-bound bridge — it owns settings persistence, state events, and log
capture, and stays free of Wails imports so it is unit-testable.
Underneath, the embedded relay engine from `internal/volunteer/engine`
registers with the broker and drives a bundled, external
[Xray-core](https://github.com/XTLS/Xray-core) (`xray`) process for the
VLESS + REALITY data plane.

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
(macOS: inside the .app), plus a `THIRD_PARTY_NOTICES.txt`. Point `XRAY` at
the binary to bundle, or have `xray` on PATH:

```sh
XRAY=/path/to/xray scripts/package-macos.sh                   # OpenRungVolunteer.app (ad-hoc signed)
XRAY=/path/to/xray scripts/package-linux.sh -tags webkit2_41  # OpenRungVolunteer-linux-x86_64.tar.gz
# Windows (pwsh):
$env:XRAY = 'C:\path\to\xray.exe'; scripts\package-windows.ps1  # OpenRungVolunteer-windows-amd64.zip
```

Licensing: the app is GPL-3.0-or-later; Xray-core is MPL-2.0, bundled
unmodified and run as a separate process (aggregation, not linking). See
[`../THIRD_PARTY_NOTICES.md`](../THIRD_PARTY_NOTICES.md).

## Release

CI (`.github/workflows/volunteer-desktop-release.yml`) builds all three
platforms with a pinned Xray-core (v26.3.27, same pin as
`deploy/volunteer/Dockerfile`), SHA-256-verified against the release `.dgst`
on every platform. Push a `volunteer-vX.Y.Z` tag to publish a GitHub release
with all three artifacts; a manual `workflow_dispatch` run builds artifacts
only.

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

No relay hub is deployed yet, so the app currently works for homes whose
machine is publicly reachable — in practice, connections with public IPv6
(direct mode: clients connect straight to the volunteer). Hub support
(auto/tunnel mode) is already built in and activates as soon as a hub
address is configured in Settings → Advanced; once a production hub exists,
NAT'd homes will be able to volunteer too.
