# Third-Party Notices

OpenRung is distributed with, links against, or bundles the third-party
components listed below. This file reproduces the copyright notices and license
information those components require us to carry when we distribute them.

It is the single source of truth for attribution. Every distribution channel
must surface it:

- **`openrung-volunteer` / `openrung-relayhub` Docker images** — the file is
  copied to `/usr/local/share/openrung/THIRD_PARTY_NOTICES.md` in each image.
- **Server binaries on the host** — ship this file alongside the binary.
- **Desktop app (Wails GUI)** — the in-app "Open-source licenses" screen
  renders these notices (`desktop/frontend/src/licenses/notices.ts` mirrors
  section 7 of this file plus `LICENSE`; a frontend test pins the bundled GPL
  text to `LICENSE`).

The OpenRung mobile app is developed and distributed from its own repository
and must carry its own third-party notices (in-app "Open Source Licenses"
screen), including the sing-box / Libbox transitive set for the exact engine
commit each release is built against.

> Maintenance: regenerate the Go sections from tooling so the transitive set
> stays accurate as dependencies drift:
> `go-licenses report ./cmd/volunteer ./cmd/relayhub` for the server side;
> for the desktop app, the union of
> `GOOS={darwin,windows,linux} go list -deps -tags desktop,production .`
> run inside `desktop/`, plus the runtime `dependencies` of
> `desktop/frontend/package.json`.

---

## 1. Strong copyleft (GPL) — controls the project license

### sing-box (libbox) — GPL-3.0-or-later

- **Component:** `github.com/SagerNet/sing-box` (the `libbox` mobile library,
  statically linked into the OpenRung mobile app — developed in its own
  repository — and, since the desktop GUI, the sing-box engine **binary
  bundled into desktop release packages** and run as a supervised subprocess;
  the pinned version lives in `.github/workflows/desktop-release.yml`)
- **License:** GNU General Public License v3.0 or later (**GPL-3.0-or-later**),
  with an additional permitted term.
- **Upstream:** https://github.com/SagerNet/sing-box
- **License text:** https://github.com/SagerNet/sing-box/blob/main/LICENSE
  (the full GPL-3.0 text is also bundled in this repository as `LICENSE`).
- **Additional term (GPL-3.0 §7, must be preserved):**
  *"In addition, no derivative work may use the name or imply association with
  this application without prior consent."*

sing-box is **statically linked** into the OpenRung mobile app. Under
GPL-3.0 §5, the resulting combined work — including OpenRung's own first-party
code in that app — is licensed to recipients under **GPL-3.0-or-later**.
OpenRung as a whole is licensed under GPL-3.0-or-later (see `LICENSE`).

The mobile app's repository must carry the full sing-box notices, including
the libbox transitive set (`gvisor`, `quic-go`, `wireguard-go`, `utls`,
`sagernet/*`, …) captured from the exact sing-box commit each release is
built against.

OpenRung is **not affiliated with or endorsed by** sing-box or SagerNet; the
sing-box name is used only descriptively.

---

## 2. Weak copyleft (MPL-2.0) — notice + source pointer only

We redistribute these **unmodified**, so the only obligation is to inform
recipients of the MPL-2.0 terms and where to obtain the source.

### Xray-core (VLESS + REALITY + Vision)

- **Component:** `github.com/XTLS/Xray-core` — the `xray` binary, plus
  `geoip.dat` / `geosite.dat`, bundled into the `openrung-volunteer` image.
- **Version:** v26.3.27 (pinned in `deploy/volunteer/Dockerfile`,
  `ARG XRAY_VERSION`, SHA-256 verified at build).
- **License (code):** Mozilla Public License 2.0 (**MPL-2.0**).
- **Source for the exact version:**
  https://github.com/XTLS/Xray-core/releases/tag/v26.3.27
- **License text:** https://github.com/XTLS/Xray-core/blob/main/LICENSE
- Note: `geoip.dat` and `geosite.dat` are **data files with their own licenses**
  — see section 4. They are *not* covered by MPL-2.0.

### yamux

- **Component:** `github.com/hashicorp/yamux` v0.1.2 (reverse-tunnel transport;
  statically linked into the volunteer, relayhub, and broker binaries).
- **License:** MPL-2.0.
- **Source:** https://github.com/hashicorp/yamux/tree/v0.1.2

### ca-certificates (Alpine package)

- **License:** MPL-2.0. Corresponding source is published by Alpine via
  `aports`. Installed in both Docker images.

> Full MPL-2.0 text: https://www.mozilla.org/MPL/2.0/

---

## 3. Strong copyleft in the base image (GPL-2.0) — written source offer

The `alpine:3.21` base of the `openrung-volunteer` and `openrung-relayhub`
images includes GPL-2.0-only userland. These are aggregated with — and do not
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
into the `openrung-volunteer` image.

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

### BSD-3-Clause

Statically linked into the Go server binaries:

- `golang.org/x/crypto` v0.17.0 — Copyright (c) The Go Authors
- `golang.org/x/sync` v0.1.0 — Copyright (c) The Go Authors
- `golang.org/x/text` v0.14.0 — Copyright (c) The Go Authors
- Go standard library / runtime — Copyright (c) The Go Authors
  (source: https://github.com/golang/go)

### MIT

- `github.com/quic-go/quic-go` v0.60.0 — Copyright (c) 2016 the quic-go authors
  & Google, Inc. (statically linked into the server binaries via `internal/punch`)
- `github.com/jackc/pgx/v5` v5.6.0 — Copyright (c) 2013-2021 Jack Christensen
- `github.com/jackc/pgpassfile` v1.0.0 — Copyright (c) 2019 Jack Christensen
- `github.com/jackc/pgservicefile` — Copyright (c) 2020 Jack Christensen
- `github.com/jackc/puddle/v2` v2.2.1 — Copyright (c) 2018 Jack Christensen
- `musl` libc (Alpine) — Copyright (c) the musl contributors
- `alpine-keys` (Alpine) — MIT

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

### 7.2 Application binary (Go) — statically linked

Union of `GOOS={darwin,windows,linux} go list -deps -tags desktop,production`
(2026-07-11, versions per `desktop/go.mod`):

**MIT**

- `github.com/wailsapp/wails/v2` — Copyright (c) 2018-Present Lea Anthony
- `github.com/wailsapp/go-webview2` — Copyright (c) 2020 John Chadwick;
  portions Copyright (c) 2017 Serge Zaitsev
- `github.com/wailsapp/mimetype` — Copyright (c) 2018-2020 Gabriel Vasile
- `github.com/leaanthony/go-ansi-parser`, `…/slicer`, `…/u` — Copyright (c)
  Lea Anthony
- `github.com/quic-go/quic-go` — Copyright (c) 2016 the quic-go authors &
  Google, Inc. (via `internal/punch`)
- `github.com/rivo/uniseg` — Copyright (c) 2019 Oliver Kuederle
- `github.com/bep/debounce` — Copyright (c) 2016 Bjørn Erik Pedersen
- `github.com/go-ole/go-ole` — Copyright (c) 2013-2017 Yasuhiro Matsumoto
- `github.com/samber/lo` — Copyright (c) 2022-2025 Samuel Berthe
- `git.sr.ht/~jackmordaunt/go-toast/v2` — dual UNLICENSE / MIT

**BSD-2-Clause**

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
- `maplibre-gl` — BSD-3-Clause — Copyright (c) MapLibre contributors; portions
  Copyright (c) 2016-2020 Mapbox, Inc. (its published bundle vendors its own
  permissive-licensed dependencies)
- `topojson-client` — ISC — Copyright 2012-2019 Michael Bostock
- `world-atlas` — ISC — Copyright 2013-2019 Michael Bostock; data derived from
  Natural Earth (public domain)

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

Full texts for MPL-2.0, GPL-2.0, and CC-BY-SA-4.0 are referenced by
URL above; GPL-3.0 is bundled as `LICENSE` in this repository.
