#!/usr/bin/env bash
#
# Build the macOS .app with xray bundled INSIDE it, so it runs on a Mac that
# has never installed Xray-core. xray (MPL-2.0) is copied into
# Contents/Resources/xray; the app's resolveXrayPath() finds it there before
# consulting PATH. The whole bundle is then ad-hoc re-signed so macOS accepts
# the added binary.
#
# Usage:
#   scripts/package-macos.sh                 # native arch
#   XRAY=/path/to/xray scripts/package-macos.sh
#   scripts/package-macos.sh -platform darwin/universal   # (needs a universal xray)
set -euo pipefail
cd "$(dirname "$0")/.."

# Locate xray: XRAY override, else PATH.
XRAYBIN="${XRAY:-$(command -v xray || true)}"
if [[ -z "${XRAYBIN}" || ! -x "${XRAYBIN}" ]]; then
  echo "error: xray not found. Install Xray-core or set XRAY=/path/to/xray" >&2
  exit 1
fi

export PATH="${PATH}:$(go env GOPATH)/bin"

echo "==> wails build ${*:-}"
wails build "$@"

APP="build/bin/OpenRungVolunteer.app"
RES="${APP}/Contents/Resources"
[[ -d "${APP}" ]] || { echo "error: ${APP} not found after build" >&2; exit 1; }

echo "==> bundling xray from ${XRAYBIN}"
cp "${XRAYBIN}" "${RES}/xray"
chmod +x "${RES}/xray"

# Warn if the bundled xray arch won't match the app (Intel vs Apple Silicon).
APP_ARCH="$(lipo -archs "${APP}/Contents/MacOS/OpenRungVolunteer" 2>/dev/null || echo unknown)"
XR_ARCH="$(lipo -archs "${RES}/xray" 2>/dev/null || echo unknown)"
echo "    app arch: ${APP_ARCH}   xray arch: ${XR_ARCH}"
if [[ "${APP_ARCH}" != "unknown" && "${XR_ARCH}" != *"${APP_ARCH%% *}"* ]]; then
  echo "    WARNING: xray arch (${XR_ARCH}) may not match the app (${APP_ARCH}) — the volunteer's Mac needs a matching arch." >&2
fi

# License notices for the app (GPL-3.0-or-later) and the bundled Xray-core
# binary (MPL-2.0, unmodified aggregation — run as a separate process).
XR_VER="$("${XRAYBIN}" version 2>/dev/null | head -1 || echo 'unknown version')"
cat > "${RES}/THIRD_PARTY_NOTICES.txt" <<EOF
This application bundles Xray-core (${XR_VER}), licensed under MPL-2.0.
It is included unmodified and runs as a separate process.
Source: https://github.com/XTLS/Xray-core
License text: https://www.mozilla.org/MPL/2.0/

OpenRung Volunteer is free software (GPL-3.0-or-later).
Source: https://github.com/openrung/openrung
EOF

# Ship the full corresponding-source license texts, not just URLs: GPL-3.0-or-later
# §4/§6 require a copy of the License to accompany every conveyed binary, and
# MPL-2.0 §3.1 requires Xray-core's own license. XRAY_LICENSE (Xray's LICENSE from
# the release zip) is set by CI; skipped silently for a local PATH-xray build.
cp ../LICENSE "${RES}/LICENSE.txt"
cp ../THIRD_PARTY_NOTICES.md "${RES}/THIRD_PARTY_NOTICES.md"
if [[ -n "${XRAY_LICENSE:-}" && -f "${XRAY_LICENSE}" ]]; then
  cp "${XRAY_LICENSE}" "${RES}/XRAY-LICENSE.txt"
fi

echo "==> ad-hoc re-signing the bundle (covers the added xray)"
codesign --force --deep --sign - "${APP}"
codesign --verify --deep --strict "${APP}" && echo "    signature OK"

echo "==> done"
du -sh "${APP}"
echo "    bundled xray: ${RES}/xray"
echo "    ship it: ditto -c -k --keepParent ${APP} OpenRungVolunteer.zip"
