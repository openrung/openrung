# Third-Party Notices

OpenRung is distributed with, links against, or bundles the third-party
components listed below. This file reproduces the copyright notices and license
information those components require us to carry when we distribute them.

It is the single source of truth for attribution. Every distribution channel
must surface it:

- **Android app / iOS app** — render these notices in an in-app
  "Open Source Licenses" screen.
- **`openrung-volunteer` / `openrung-relayhub` Docker images** — the file is
  copied to `/usr/local/share/openrung/THIRD_PARTY_NOTICES.md` in each image.
- **Server binaries on the host** — ship this file alongside the binary.

> Maintenance: regenerate the Go and Android sections from tooling so the
> transitive set stays accurate as dependencies drift:
> `go-licenses report ./cmd/volunteer ./cmd/relayhub` for the server side, and an
> Android license plugin (e.g. AboutLibraries) for the app. The sing-box /
> Libbox transitive set (below) should be captured from the **exact sing-box
> commit** the release was built against — see "Corresponding source," below.

---

## 1. Strong copyleft (GPL) — controls the mobile clients

### sing-box (libbox) — GPL-3.0-or-later

- **Component:** `github.com/SagerNet/sing-box` (the `libbox` mobile library:
  `android/app/libs/libbox.aar`, `ios/ThirdParty/Libbox.xcframework`)
- **License:** GNU General Public License v3.0 or later (**GPL-3.0-or-later**),
  with an additional permitted term.
- **Upstream:** https://github.com/SagerNet/sing-box
- **License text:** https://github.com/SagerNet/sing-box/blob/main/LICENSE
  (the full GPL-3.0 text is also bundled in this repository as `LICENSE`).
- **Additional term (GPL-3.0 §7, must be preserved):**
  *"In addition, no derivative work may use the name or imply association with
  this application without prior consent."*

sing-box is **statically linked** into the OpenRung Android APK and iOS app.
Under GPL-3.0 §5, the resulting combined work — including OpenRung's own
first-party code in those apps — is licensed to recipients under
**GPL-3.0-or-later**. OpenRung as a whole is licensed under GPL-3.0-or-later
(see `LICENSE`).

OpenRung is **not affiliated with or endorsed by** sing-box or SagerNet; the
sing-box name is used only descriptively.

#### sing-box transitive components (compiled into the apps)

The `libbox` build statically links additional libraries that are therefore
distributed inside the apps. This list must be completed from the exact build
(`go-licenses` against the sing-box module); the notable ones include:

- `gvisor.dev/gvisor` — Apache-2.0 (ships a NOTICE file that must be reproduced)
- `github.com/quic-go/quic-go` — MIT
- `golang.zx2c4.com/wireguard` (wireguard-go) — MIT
- `github.com/refraction-networking/utls` — BSD-3-Clause
- `github.com/sagernet/sing`, `sing-quic`, `sing-shadowsocks*`, and related
  `sagernet/*` modules — GPL-3.0 / mixed (reinforces the GPL-3.0 result above)

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

Statically linked into the Go server binaries (and, via libbox, into the apps):

- `golang.org/x/crypto` v0.17.0 — Copyright (c) The Go Authors
- `golang.org/x/sync` v0.1.0 — Copyright (c) The Go Authors
- `golang.org/x/text` v0.14.0 — Copyright (c) The Go Authors
- Go standard library / runtime — Copyright (c) The Go Authors
  (source: https://github.com/golang/go)

### MIT

- `github.com/jackc/pgx/v5` v5.6.0 — Copyright (c) 2013-2021 Jack Christensen
- `github.com/jackc/pgpassfile` v1.0.0 — Copyright (c) 2019 Jack Christensen
- `github.com/jackc/pgservicefile` — Copyright (c) 2020 Jack Christensen
- `github.com/jackc/puddle/v2` v2.2.1 — Copyright (c) 2018 Jack Christensen
- `musl` libc (Alpine) — Copyright (c) the musl contributors
- `alpine-keys` (Alpine) — MIT

### BSD-2-Clause

- `org.maplibre.gl:android-sdk` 11.8.0 (MapLibre Native) — Copyright (c)
  MapLibre contributors (2021), MapTiler.com (2018-2021), Mapbox (2014-2020).
  The SDK also aggregates further third-party notices in its `LICENSE.md`
  (https://github.com/maplibre/maplibre-native/blob/main/LICENSE.md), which
  should be reproduced in the Android in-app notices.

### Apache-2.0 (reproduce each component's NOTICE file, not just the license)

Bundled in the Android APK:

- `androidx.compose:*` (compose-bom 2024.12.01): foundation,
  material-icons-extended, material3, runtime, ui, ui-tooling-preview
- `androidx.activity:activity-compose` 1.9.3
- `androidx.appcompat:appcompat` 1.7.0
- `androidx.core:core-ktx` 1.15.0
- `androidx.lifecycle:lifecycle-runtime-compose` 2.8.7
- `org.jetbrains.kotlin:kotlin-stdlib` (Apache-2.0; also bundles Boost-1.0 and
  fdlibm/SUN-licensed math portions that must be acknowledged)
- `org.jetbrains.kotlinx:kotlinx-coroutines-android` 1.9.0
- `org.jetbrains.kotlinx:kotlinx-serialization-json` 1.7.3

> Full Apache-2.0 text: https://www.apache.org/licenses/LICENSE-2.0

---

## 6. Components that are NOT distributed (no obligation)

Listed so they are deliberately **excluded** from the shipped notices: test
dependencies (`testify`, `objx`, `go-spew`, `go-difflib`, `yaml.v3`,
`check.v1`), build tools (`sagernet/gomobile`/`gobind`, Gradle, the `golang`
build image), and all GitHub Actions. The MapLibre **demo tiles and glyph
fonts** at `demotiles.maplibre.org` are fetched at runtime and not bundled, so
no redistribution obligation attaches (but that demo endpoint is not intended
for production traffic — move to a self-hosted/licensed source before scaling).

---

## Corresponding source (GPL-3.0 §6 / §3)

The complete corresponding source for any distributed OpenRung binary — the
OpenRung source, the pinned sing-box revision, and the build scripts — is
available from the OpenRung public repository: **<add public repo URL>**.

The mobile apps are built against a specific sing-box commit. Record that commit
SHA per release (see `android/ThirdParty/README.md` and `ios/ThirdParty/README.md`)
so the corresponding source is reproducible. OpenRung will provide the
corresponding source for at least three (3) years on request.

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

### BSD 2-Clause License

```
Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:

1. Redistributions of source code must retain the above copyright notice, this
   list of conditions and the following disclaimer.
2. Redistributions in binary form must reproduce the above copyright notice,
   this list of conditions and the following disclaimer in the documentation
   and/or other materials provided with the distribution.

THIS SOFTWARE IS PROVIDED ... "AS IS" ... (full standard BSD-2-Clause disclaimer).
```

Full texts for MPL-2.0, Apache-2.0, GPL-2.0, and CC-BY-SA-4.0 are referenced by
URL above; GPL-3.0 is bundled as `LICENSE` in this repository.
