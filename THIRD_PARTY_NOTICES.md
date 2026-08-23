# Third-Party Notices

OpenRung is distributed with, links against, or bundles the third-party
components listed below. This file reproduces the copyright notices and license
information those components require us to carry when we distribute them.

It is the single source of truth for attribution. Every distribution channel
must surface it:

- **`openrung-relay` / `openrung-relayhub` Docker images** — the file is
  copied to `/usr/local/share/openrung/THIRD_PARTY_NOTICES.md` in each image.
- **Server binaries on the host** — ship this file alongside the binary.
- **Client CLI release archives (`openrung-client-*.tar.gz` / `.zip`)** — the
  file is copied into each archive alongside `LICENSE`
  (`.github/workflows/client-release.yml`).
- **Desktop app (Wails GUI)** — the packaging scripts
  (`desktop/scripts/package-{windows.ps1,linux.sh,macos.sh}`) copy this file
  and `LICENSE` into every staged package
  (`desktop/scripts/package-notices.test.mjs` enforces it), and the in-app
  "Open-source licenses" screen renders these notices
  (`desktop/frontend/src/licenses/notices.ts` mirrors sections 7–8 of this
  file plus `LICENSE`; a frontend test pins the bundled GPL text to `LICENSE`
  and the Windows driver license texts).

The OpenRung mobile app is developed and distributed from its own repository
and must carry its own third-party notices (in-app "Open Source Licenses"
screen), including the sing-box / Libbox transitive set for the exact engine
commit each release is built against.

> Maintenance: regenerate the Go sections from tooling so the transitive set
> stays accurate as dependencies drift:
> `go-licenses report ./cmd/broker ./cmd/relay ./cmd/relayhub ./cmd/wsssidecar ./cmd/client`
> (for `./cmd/client` set `GOFLAGS=-tags=with_utls,with_external_windivert`
> and take the union across `GOOS={linux,windows,darwin}`)
> for the server binaries and the client CLI;
> for the desktop app, the union of
> `GOOS={darwin,windows,linux} go list -deps -tags desktop,production .`
> run inside `desktop/`, plus the runtime `dependencies` of
> `desktop/frontend/package.json`.

---

## 1. Strong copyleft (GPL) — controls the project license

### sing-box (libbox) — GPL-3.0-or-later

- **Component:** `github.com/SagerNet/sing-box` (the `libbox` mobile library,
  statically linked into the OpenRung mobile app — developed in its own
  repository —; since the desktop GUI, the sing-box engine **binary
  bundled into desktop release packages** and run as a supervised subprocess
  (the pinned version lives in `.github/workflows/desktop-release.yml`); and,
  since client CLI 0.5.0, the sing-box engine **statically linked into
  `openrung-client`** through `internal/singboxruntime` — the pinned version
  lives in the root `go.mod`)
- **License:** GNU General Public License v3.0 or later (**GPL-3.0-or-later**),
  with an additional permitted term.
- **Upstream:** https://github.com/SagerNet/sing-box
- **License text:** https://github.com/SagerNet/sing-box/blob/main/LICENSE
  (the full GPL-3.0 text is also bundled in this repository as `LICENSE`).
- **Additional term (GPL-3.0 §7, must be preserved):**
  *"In addition, no derivative work may use the name or imply association with
  this application without prior consent."*

sing-box is **statically linked** into the OpenRung mobile app and into the
`openrung-client` terminal client. Under GPL-3.0 §5, each resulting combined
work — including OpenRung's own first-party code in those binaries — is
licensed to recipients under **GPL-3.0-or-later**. OpenRung as a whole is
licensed under GPL-3.0-or-later (see `LICENSE`).

The mobile app's repository must carry the full sing-box notices, including
the libbox transitive set (`gvisor`, `quic-go`, `wireguard-go`, `utls`,
`sagernet/*`, …) captured from the exact sing-box commit each release is
built against.

The `openrung-client` static link brings sing-box's GPL-3.0-or-later family
into the client binary as well: `github.com/sagernet/sing`, `…/sing-mux`,
`…/sing-shadowsocks`, `…/sing-shadowsocks2`, `…/sing-shadowtls`,
`…/sing-snell`, `…/sing-tun`, `…/sing-vmess`, `…/fswatch` (all
Copyright (C) 2022 by nekohasekai), and `github.com/anytls/sing-anytls`
(Copyright (C) 2025 anytls) — same GPL-3.0-or-later terms; the text is this
repository's `LICENSE`. The permissively licensed remainder of that
transitive set is inventoried in section 5, and the Windows driver binary it
embeds in section 8. Client CLI builds pass `with_external_windivert`, so
the WinDivert driver is **excluded** from every client archive (section 8).

OpenRung is **not affiliated with or endorsed by** sing-box or SagerNet; the
sing-box name is used only descriptively.

---

## 2. Weak copyleft (MPL-2.0) — notice + source pointer only

We redistribute these **unmodified**, so the only obligation is to inform
recipients of the MPL-2.0 terms and where to obtain the source.

### Xray-core (VLESS + REALITY + Vision)

- **Component:** `github.com/XTLS/Xray-core` — the `xray` binary, plus
  `geoip.dat` / `geosite.dat`, bundled into the `openrung-relay` image.
  The OpenRung Volunteer desktop app (`desktop-volunteer/`) bundles the
  `xray` binary **only** — no geo data files — so section 4 does not apply
  to that channel.
- **Version:** v26.3.27 (pinned in `deploy/relay/Dockerfile`
  (`ARG XRAY_VERSION`) and in
  `.github/workflows/desktop-volunteer-release.yml`; SHA-256 verified
  against the release `.dgst` file at build/fetch).
- **License (code):** Mozilla Public License 2.0 (**MPL-2.0**).
- **Source for the exact version:**
  https://github.com/XTLS/Xray-core/releases/tag/v26.3.27
- **License text:** https://github.com/XTLS/Xray-core/blob/main/LICENSE
- Note: `geoip.dat` and `geosite.dat` are **data files with their own licenses**
  — see section 4. They are *not* covered by MPL-2.0.

### yamux

- **Component:** `github.com/hashicorp/yamux` v0.1.2 (reverse-tunnel and
  Reality-over-WSS stream multiplexing; statically linked into the relay,
  relayhub, relay-local sidecar, client CLI, and desktop binaries).
- **License:** MPL-2.0.
- **Source:** https://github.com/hashicorp/yamux/tree/v0.1.2

### ca-certificates (Alpine package)

- **License:** MPL-2.0. Corresponding source is published by Alpine via
  `aports`. Installed in both Docker images.

> Full MPL-2.0 text: https://www.mozilla.org/MPL/2.0/

---

## 3. Strong copyleft in the base image (GPL-2.0) — written source offer

The `alpine:3.21` base of the `openrung-relay` and `openrung-relayhub` images
includes GPL-2.0-only userland. These are aggregated with — and do not
relicense — OpenRung's own binaries, but conveying the images still requires a
source offer for the GPL components themselves.

- **Components:** `busybox` (GPL-2.0-only), `apk-tools` (GPL-2.0-only),
  `alpine-baselayout` (GPL-2.0-only).
- **Written offer (GPL-2.0 §3):** These are **unmodified** Alpine packages. The
  complete corresponding source is available from the Alpine `aports` tree for
  the pinned `alpine:3.21` release:
  https://gitlab.alpinelinux.org/alpine/aports — and from the Alpine package
  repositories (`apk fetch --source <pkg>`). OpenRung will, for at least three
  (3) years, on request, provide or point to the corresponding source for the
  exact package versions shipped in a given image. Contact: **<add contact /
  repo issues URL>**.
- **License text:** GPL-2.0 — https://www.gnu.org/licenses/old-licenses/gpl-2.0.txt

---

## 4. Bundled data files (license differs from the code that ships them)

Both files are extracted from the Xray-core release zip and bundled, unmodified,
into the `openrung-relay` image. They are **not** bundled in the OpenRung
Volunteer desktop app (which ships the `xray` binary only), so these notices
apply to the Docker image channel only.

### geoip.dat — CC-BY-SA-4.0 + MaxMind GeoLite2

- **Data license:** Creative Commons Attribution-ShareAlike 4.0
  (**CC-BY-SA-4.0**) — https://creativecommons.org/licenses/by-sa/4.0/
- **Attribution (CC-BY-SA-4.0):** geoip data sourced from the Loyalsoldier geoip
  project (https://github.com/Loyalsoldier/geoip), licensed CC-BY-SA-4.0.
  Distributed unmodified (no adaptation), so only attribution applies.
- **Required MaxMind GeoLite2 notice (verbatim):**

  > This product includes GeoLite2 data created by MaxMind, available from
  > https://www.maxmind.com

### geosite.dat — MIT

- **License:** MIT.
- **Attribution:** generated from `github.com/v2fly/domain-list-community`,
  Copyright (c) 2018-2019 V2Ray. The MIT notice (see Appendix A) must be retained.

---

## 5. Permissive components (attribution required on distribution)

### BSD-2-Clause

- `github.com/gorilla/websocket` v1.5.3 — Copyright (c) 2013 The
  Gorilla WebSocket Authors (statically linked into the relay-local WSS
  sidecar, the client CLI, and the desktop application)
- `github.com/godbus/dbus/v5` v5.2.2 — Copyright (c) 2013, Georg Reinke,
  Google (client CLI Linux builds, via sing-box's `resolved` service; also in
  the desktop app — section 7.2)

### BSD-3-Clause

Statically linked into the Go server binaries and the client CLI (union
across the binaries; versions per the root `go.mod`):

- `github.com/atotto/clipboard` v0.1.4 — Copyright (c) 2013 Ato Araki
  (client TUI only, via `bubbles/textinput`)
- `golang.org/x/crypto` v0.54.0 — Copyright (c) The Go Authors
- `golang.org/x/net` v0.57.0 — Copyright (c) The Go Authors
- `golang.org/x/sync` v0.22.0 — Copyright (c) The Go Authors
- `golang.org/x/sys` v0.47.0 — Copyright (c) The Go Authors
- `golang.org/x/text` v0.40.0 — Copyright (c) The Go Authors
- Go standard library / runtime — Copyright (c) The Go Authors
  (source: https://github.com/golang/go)

Client CLI only (the bundled sing-box engine's transitive set):

- `golang.org/x/exp` and `golang.org/x/mod` v0.37.0 — Copyright (c) The Go
  Authors
- `github.com/metacubex/utls` v1.8.7 — Copyright (c) 2009 The Go Authors
  (uTLS fork; the Reality/uTLS client handshake)
- `github.com/miekg/dns` v1.1.72 — Copyright (c) 2009 The Go Authors;
  extensions Copyright (c) 2011 Miek Gieben
- `github.com/fsnotify/fsnotify` v1.9.0 — Copyright © 2012 The Go Authors,
  © fsnotify Authors
- `github.com/ajg/form` v1.5.1 — Copyright (c) 2014 Alvaro J. Genial
- `google.golang.org/protobuf` v1.36.11 — Copyright (c) 2018 The Go Authors
- `go4.org/netipx` — Copyright (c) 2020 The Inet.af AUTHORS
- `github.com/database64128/netx-go` v0.1.1 — Copyright 2009 The Go Authors

### Apache-2.0

Client CLI only (the bundled sing-box engine's transitive set). Full text:
https://www.apache.org/licenses/LICENSE-2.0 — none of these ship a NOTICE
file:

- `github.com/caddyserver/certmagic` v0.25.3 and `github.com/mholt/acmez/v3`
  v3.1.6 — Copyright Matthew Holt
- `google.golang.org/grpc` v1.82.1 and
  `google.golang.org/genproto/googleapis/rpc` — Copyright gRPC authors /
  Google LLC
- `github.com/google/btree` v1.1.3 — Copyright Google Inc.
- `github.com/sagernet/netlink` and `github.com/sagernet/nftables`
  v0.3.0-mod.4 — Copyright the vishvananda/netlink and Google nftables
  authors and SagerNet
- `github.com/vishvananda/netns` v0.0.5 — Copyright the netns authors

### ISC

- `github.com/coder/websocket` v1.8.14 — Copyright (c) 2025 Coder (client
  CLI only, via sing-box's API service)

### Public domain (CC0 / Unlicense)

- `github.com/zeebo/blake3` v0.2.4 — CC0-1.0 (client CLI only)
- `github.com/logrusorgru/aurora` v2.0.3 — Unlicense (client CLI only)

### MIT

- `github.com/quic-go/quic-go` v0.60.0 — Copyright (c) 2016 the quic-go authors
  & Google, Inc. (statically linked into the server binaries and the client
  CLI via `internal/punch`, the QUIC layer over the first-party `punchcore`
  module; `punchcore` itself is stdlib-only and quic-go remains a root-module
  dependency)
- `github.com/jackc/pgx/v5` v5.9.2 — Copyright (c) 2013-2021 Jack Christensen
- `github.com/jackc/pgpassfile` v1.0.0 — Copyright (c) 2019 Jack Christensen
- `github.com/jackc/pgservicefile` — Copyright (c) 2020 Jack Christensen
- `github.com/jackc/puddle/v2` v2.2.2 — Copyright (c) 2018 Jack Christensen
- `musl` libc (Alpine) — Copyright (c) the musl contributors
- `alpine-keys` (Alpine) — MIT

Statically linked into the client CLI only (the interactive TUI stack, union
across `GOOS={darwin,linux,windows}`; versions per the root `go.mod`):

- `github.com/charmbracelet/bubbletea` v1.3.10, `…/bubbles` v1.0.0,
  `…/lipgloss` v1.1.0, `…/colorprofile`, and `…/x/{ansi,cellbuf,term}` —
  Copyright (c) 2020-2025 Charmbracelet, Inc
- `github.com/aymanbagabas/go-osc52/v2` v2.0.1 — Copyright (c) 2022 Ayman
  Bagabas
- `github.com/clipperhouse/displaywidth` v0.9.0, `…/stringish` v0.1.1, and
  `…/uax29/v2` v2.5.0 — Copyright (c) 2020-2025 Matt Sherman
- `github.com/erikgeiser/coninput` — Copyright (c) 2021 Erik G. (Windows
  console input; Windows builds only)
- `github.com/lucasb-eyer/go-colorful` v1.3.0 — Copyright (c) 2013 Lucas Beyer
- `github.com/mattn/go-isatty` v0.0.23 and `github.com/mattn/go-runewidth`
  v0.0.27 — Copyright (c) Yasuhiro Matsumoto
- `github.com/mattn/go-localereader` v0.0.1 — Copyright (c) Yasuhiro
  Matsumoto (MIT per its README; upstream ships no LICENSE file)
- `github.com/muesli/ansi`, `github.com/muesli/cancelreader` v0.2.2, and
  `github.com/muesli/termenv` v0.16.0 — Copyright (c) 2019-2022 Christian
  Muehlhaeuser (cancelreader: and Erik Geiser)
- `github.com/rivo/uniseg` v0.4.7 — Copyright (c) 2019 Oliver Kuederle
- `github.com/xo/terminfo` — Copyright (c) 2016 Anmol Sethi

Client CLI only (the bundled sing-box engine's transitive set; versions per
the root `go.mod`):

- `github.com/andybalholm/brotli` v1.1.0 — Copyright (c) 2009-2016 the Brotli
  Authors
- `github.com/klauspost/compress` v1.19.1 — Copyright (c) 2012 The Go
  Authors; (c) 2019+ Klaus Post — and `github.com/klauspost/cpuid/v2` v2.3.0
  — Copyright (c) 2015 Klaus Post
- `github.com/sagernet/bbolt` — Copyright (c) 2013 Ben Johnson
- `github.com/sagernet/cors` v1.2.1 — Copyright (c) 2014 Olivier Poitrey
- `github.com/sagernet/smux` — Copyright (c) 2016-2017 xtaci
- `github.com/sagernet/ws` — Copyright (c) 2017-2021 Sergey Kamardin — with
  `github.com/gobwas/httphead` v0.1.0 and `…/pool` v0.2.1 — Copyright (c)
  2017-2019 Sergey Kamardin
- `github.com/gofrs/uuid/v5` v5.5.1 — Copyright (C) 2013-2018 Maxim Bublis
- `github.com/go-chi/chi/v5` v5.2.5 — Copyright (c) 2015-present Peter
  Kieltyka, Google Inc. — and `github.com/go-chi/render` v1.0.3
- `github.com/go-ole/go-ole` v1.3.0 — Copyright © 2013-2017 Yasuhiro
  Matsumoto (Windows builds only)
- `github.com/cretz/bine` v0.2.0 — Copyright (c) 2018 Chad Retz
- `github.com/database64128/tfo-go/v2` v2.3.2 — Copyright (c) 2021
  database64128
- `github.com/florianl/go-nfqueue/v2` v2.1.0 — Copyright (C) 2018-2020
  Florian Lehner (Linux builds only)
- `github.com/jsimonetti/rtnetlink` v1.4.1 — Copyright (C) 2016 Jeroen
  Simonetti — with `github.com/mdlayher/netlink` v1.11.2 and
  `…/socket` v0.6.0 — Copyright (C) Matt Layher (Linux builds only)
- `github.com/libdns/libdns` v1.1.1 — Copyright (c) 2020 Matthew Holt — and
  `github.com/caddyserver/zerossl` v0.1.5 — Copyright (c) 2024 Matthew Holt
- `go.uber.org/zap` v1.27.1, `…/zap/exp` v0.3.0, and `go.uber.org/multierr`
  v1.11.0 — Copyright (c) 2016-2024 Uber Technologies, Inc.
- `lukechampine.com/blake3` v1.3.0 — Copyright (c) 2020 Luke Champine

> Mobile-app dependencies (UI toolkit, MapLibre, libbox, and their NOTICE
> files) are inventoried in the mobile app's own repository, alongside the
> app's in-app license screen.

---

## 6. Components that are NOT distributed (no obligation)

Listed so they are deliberately **excluded** from the shipped notices: test
dependencies (`testify`, `objx`, `go-spew`, `go-difflib`, `yaml.v3`,
`check.v1`), build tools (the `golang` build image, Vite/TypeScript/vitest and
the rest of `desktop/frontend`'s `devDependencies`), and all GitHub Actions.

---

## 7. Desktop app (Wails GUI)

The desktop release packages bundle three layers. The in-app "Open-source
licenses" screen renders this section (via
`desktop/frontend/src/licenses/notices.ts`) so recipients get the notices
offline.

### 7.1 Bundled sing-box engine binary — GPL-3.0-or-later

See section 1. The binary embeds, among others: gVisor (Apache-2.0), quic-go
(MIT), wireguard-go (MIT), utls (BSD-3-Clause), `sagernet/*`. Capture the
transitive set from the exact sing-box version pinned in
`.github/workflows/desktop-release.yml`.

The upstream official Windows build of this binary embeds two Windows driver
binaries — `wintun.dll` and `WinDivert64.sys` (verified by byte content for
the pinned 1.14.0-alpha.35) — so distributing the Windows desktop package
carries their notices too; see section 8.

### 7.2 Application binary (Go) — statically linked

Union of `GOOS={darwin,windows,linux} go list -deps -tags desktop,production`
(2026-07-11, versions per `desktop/go.mod`):

**MPL-2.0**

- `github.com/hashicorp/yamux` v0.1.2 — Copyright (c) HashiCorp, Inc.;
  source and license: https://github.com/hashicorp/yamux/tree/v0.1.2

**MIT**

- `github.com/wailsapp/wails/v2` — Copyright (c) 2018-Present Lea Anthony
- `github.com/wailsapp/go-webview2` — Copyright (c) 2020 John Chadwick;
  portions Copyright (c) 2017 Serge Zaitsev
- `github.com/wailsapp/mimetype` — Copyright (c) 2018-2020 Gabriel Vasile
- `github.com/leaanthony/go-ansi-parser`, `…/slicer`, `…/u` — Copyright (c)
  Lea Anthony
- `github.com/quic-go/quic-go` — Copyright (c) 2016 the quic-go authors &
  Google, Inc. (via `internal/punch`, the QUIC layer over the first-party
  `punchcore` module)
- `github.com/rivo/uniseg` — Copyright (c) 2019 Oliver Kuederle
- `github.com/bep/debounce` — Copyright (c) 2016 Bjørn Erik Pedersen
- `github.com/go-ole/go-ole` — Copyright (c) 2013-2017 Yasuhiro Matsumoto
- `github.com/samber/lo` — Copyright (c) 2022-2025 Samuel Berthe
- `git.sr.ht/~jackmordaunt/go-toast/v2` — dual UNLICENSE / MIT

**BSD-2-Clause**

- `github.com/gorilla/websocket` — Copyright (c) 2013 The Gorilla WebSocket
  Authors
- `github.com/pkg/errors` — Copyright (c) 2015, Dave Cheney
- `github.com/pkg/browser` — Copyright (c) 2014, Dave Cheney
- `github.com/godbus/dbus/v5` — Copyright (c) 2013, Georg Reinke, Google

**BSD-3-Clause**

- `github.com/google/uuid` — Copyright (c) 2009, 2014 Google Inc.
- `golang.org/x/{crypto,net,sys,text}` + Go standard library / runtime —
  Copyright (c) The Go Authors

**Apache-2.0**

- `github.com/tkrajina/go-reflector`

### 7.3 Embedded web frontend (Vite bundle)

Runtime `dependencies` of `desktop/frontend/package.json`:

- `react`, `react-dom` (+ `scheduler`) — MIT — Copyright (c) Meta Platforms,
  Inc. and affiliates
- `maplibre-gl` — BSD-3-Clause — Copyright (c) 2023, MapLibre contributors;
  Copyright (c) 2020, Mapbox. Its `LICENSE.txt` additionally covers bundled
  portions: glfx.js (MIT, Copyright (C) 2011 by Evan Wallace) and d3-color
  (BSD-3-Clause, Copyright 2010-2016 Mike Bostock)
- `topojson-client` — ISC — Copyright 2012-2019 Michael Bostock
- `world-atlas` — ISC — Copyright 2013-2019 Michael Bostock; data derived from
  Natural Earth (public domain)

---

## 8. Embedded Windows driver binaries (sing-box engine)

Windows builds of the sing-box engine — the `openrung-client.exe` static link
and the desktop package's bundled `sing-box.exe` — carry prebuilt Windows
driver binaries inside the executable. They are embedded byte-for-byte
unmodified and extracted/loaded at runtime by sing-box's own code.

### wintun.dll — Wintun Prebuilt Binaries License (+ MIT wrapper)

- **Component:** `wintun.dll` v0.14.1 — Copyright (C) WireGuard LLC —
  embedded by `github.com/sagernet/sing-tun` (`internal/wintun`) into
  `openrung-client.exe` Windows builds, and present inside the desktop
  package's `sing-box.exe`. The Go wrapper code around it is MIT
  (Copyright (C) 2017-2021 WireGuard LLC).
- **License (the DLL binary):** the Wintun **Prebuilt Binaries License** —
  reproduced in full in Appendix B, as its distribution terms require
  recipients to receive them. The DLL is distributed unmodified, only as part
  of software that uses it solely through the `wintun.h` API (§3(d)), and is
  never reverse engineered or altered.
- **Trademark/endorsement (§3(e)):** OpenRung is not affiliated with and does
  not claim endorsement by WireGuard LLC, the WireGuard project, or the
  Wintun project; the names are used only descriptively.
- **Upstream:** https://www.wintun.net (source of the driver itself, GPL-2.0,
  at https://git.zx2c4.com/wintun/)
- Note: the OpenRung terminal client refuses TUN mode on Windows, so this
  driver is never extracted or loaded by `openrung-client.exe`; the bytes
  nevertheless ship inside the Windows binary and these notices apply.

### WinDivert — excluded from client CLI archives; desktop channel only

- sing-box's Windows bridge/TLS-spoof backends embed
  `WinDivert64.sys` v2.2.2 (https://github.com/basil00/WinDivert,
  redistributed by sing-box under WinDivert's **LGPL-3.0** option) unless
  built with `with_external_windivert`.
- **Client CLI:** every `openrung-client` build passes
  `with_external_windivert` (Makefile, go-checks, client-release), so **no
  OpenRung client archive contains WinDivert**, and the release workflow
  fails if the driver's bytes reappear in the Windows binary. A feature that
  someday needs it must ship `WinDivert64.sys` next to the executable and
  extend these notices with the LGPL-3.0 text.
- **Desktop packages (Windows):** the bundled upstream `sing-box.exe` embeds
  the driver. LGPL-3.0 notice for that channel: WinDivert is Copyright (c)
  Basil, licensed LGPL-3.0 (text: https://www.gnu.org/licenses/lgpl-3.0.txt;
  it incorporates GPL-3.0, bundled in this repository as `LICENSE`); complete
  corresponding source: https://github.com/basil00/WinDivert (v2.2.2). It is
  embedded unmodified and used only through its public driver interface.

---

## Corresponding source (GPL-3.0 §6 / §3)

The complete corresponding source for any distributed OpenRung binary — the
OpenRung source, the pinned sing-box revision, and the build scripts — is
available from the OpenRung public repository: **<add public repo URL>**.

The mobile app is built against a specific sing-box commit; its repository
records that commit SHA per release so the corresponding source is
reproducible. OpenRung will provide the corresponding source for at least
three (3) years on request.

---

## Appendix A — Standard short-form license texts

### The MIT License (MIT)

```
Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

### BSD 3-Clause License

```
Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:

1. Redistributions of source code must retain the above copyright notice, this
   list of conditions and the following disclaimer.
2. Redistributions in binary form must reproduce the above copyright notice,
   this list of conditions and the following disclaimer in the documentation
   and/or other materials provided with the distribution.
3. Neither the name of the copyright holder nor the names of its contributors
   may be used to endorse or promote products derived from this software
   without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS" AND
ANY EXPRESS OR IMPLIED WARRANTIES ARE DISCLAIMED. IN NO EVENT SHALL THE
COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, ... (full
disclaimer as in the standard BSD-3-Clause text).
```

Full texts for MPL-2.0, GPL-2.0, LGPL-3.0, Apache-2.0, and CC-BY-SA-4.0 are referenced by
URL above; GPL-3.0 is bundled as `LICENSE` in this repository.

## Appendix B — Wintun Prebuilt Binaries License (verbatim)

Applies to the `wintun.dll` binary embedded in Windows builds (section 8).
Source: https://git.zx2c4.com/wintun/plain/prebuilt-binaries-license.txt

```
Prebuilt Binaries License
-------------------------

1. DEFINITIONS. "Software" means the precise contents of the "wintun.dll"
   files that are included in the .zip file that contains this document as
   downloaded from wintun.net/builds.

2. LICENSE GRANT. WireGuard LLC grants to you a non-exclusive and
   non-transferable right to use Software for lawful purposes under certain
   obligations and limited rights as set forth in this agreement.

3. RESTRICTIONS. Software is owned and copyrighted by WireGuard LLC. It is
   licensed, not sold. Title to Software and all associated intellectual
   property rights are retained by WireGuard. You must not:
   a. reverse engineer, decompile, disassemble, extract from, or otherwise
      modify the Software;
   b. modify or create derivative work based upon Software in whole or in
      parts, except insofar as only the API interfaces of the "wintun.h" file
      distributed alongside the Software (the "Permitted API") are used;
   c. remove any proprietary notices, labels, or copyrights from the Software;
   d. resell, redistribute, lease, rent, transfer, sublicense, or otherwise
      transfer rights of the Software without the prior written consent of
      WireGuard LLC, except insofar as the Software is distributed alongside
      other software that uses the Software only via the Permitted API;
   e. use the name of WireGuard LLC, the WireGuard project, the Wintun
      project, or the names of its contributors to endorse or promote products
      derived from the Software without specific prior written consent.

4. LIMITED WARRANTY. THE SOFTWARE IS PROVIDED "AS IS" AND WITHOUT WARRANTY OF
   ANY KIND. WIREGUARD LLC HEREBY EXCLUDES AND DISCLAIMS ALL IMPLIED OR
   STATUTORY WARRANTIES, INCLUDING ANY WARRANTIES OF MERCHANTABILITY, FITNESS
   FOR A PARTICULAR PURPOSE, QUALITY, NON-INFRINGEMENT, TITLE, RESULTS,
   EFFORTS, OR QUIET ENJOYMENT. THERE IS NO WARRANTY THAT THE PRODUCT WILL BE
   ERROR-FREE OR WILL FUNCTION WITHOUT INTERRUPTION. YOU ASSUME THE ENTIRE
   RISK FOR THE RESULTS OBTAINED USING THE PRODUCT. TO THE EXTENT THAT
   WIREGUARD LLC MAY NOT DISCLAIM ANY WARRANTY AS A MATTER OF APPLICABLE LAW,
   THE SCOPE AND DURATION OF SUCH WARRANTY WILL BE THE MINIMUM PERMITTED UNDER
   SUCH LAW. ALL EXPRESS OR IMPLIED CONDITIONS, REPRESENTATIONS AND
   WARRANTIES, INCLUDING ANY IMPLIED WARRANTY OF MERCHANTABILITY, FITNESS FOR
   A PARTICULAR PURPOSE OR NON-INFRINGEMENT ARE DISCLAIMED, EXCEPT TO THE
   EXTENT THAT THESE DISCLAIMERS ARE HELD TO BE LEGALLY INVALID.

5. LIMITATION OF LIABILITY. To the extent not prohibited by law, in no event
   WireGuard LLC or any third-party-developer will be liable for any lost
   revenue, profit or data or for special, indirect, consequential, incidental
   or punitive damages, however caused regardless of the theory of liability,
   arising out of or related to the use of or inability to use Software, even
   if WireGuard LLC has been advised of the possibility of such damages.
   Solely you are responsible for determining the appropriateness of using
   Software and accept full responsibility for all risks associated with its
   exercise of rights under this agreement, including but not limited to the
   risks and costs of program errors, compliance with applicable laws, damage
   to or loss of data, programs or equipment, and unavailability or
   interruption of operations. The foregoing limitations will apply even if
   the above stated warranty fails of its essential purpose. You acknowledge,
   that it is in the nature of software that software is complex and not
   completely free of errors. In no event shall WireGuard LLC or any
   third-party-developer be liable to you under any theory for any damages
   suffered by you or any user of Software or for any special, incidental,
   indirect, consequential or similar damages (including without limitation
   damages for loss of business profits, business interruption, loss of
   business information or any other pecuniary loss) arising out of the use or
   inability to use Software, even if WireGuard LLC has been advised of the
   possibility of such damages and regardless of the legal or quitable theory
   (contract, tort, or otherwise) upon which the claim is based.

6. TERMINATION. This agreement is affected until terminated. You may
   terminate this agreement at any time. This agreement will terminate
   immediately without notice from WireGuard LLC if you fail to comply with
   the terms and conditions of this agreement. Upon termination, you must
   delete Software and all copies of Software and cease all forms of
   distribution of Software.

7. SEVERABILITY. If any provision of this agreement is held to be
   unenforceable, this agreement will remain in effect with the provision
   omitted, unless omission would frustrate the intent of the parties, in
   which case this agreement will immediately terminate.

8. RESERVATION OF RIGHTS. All rights not expressly granted in this agreement
   are reserved by WireGuard LLC. For example, WireGuard LLC reserves the
   right at any time to cease development of Software, to alter distribution
   details, features, specifications, capabilities, functions, licensing
   terms, release dates, APIs, ABIs, general availability, or other
   characteristics of the Software.
```

