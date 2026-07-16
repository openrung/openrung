#!/usr/bin/env bash
#
# Build the macOS .app with sing-box bundled INSIDE it, so it runs on a Mac that
# has never installed sing-box. sing-box (GPL-3.0) is copied into
# Contents/Resources/sing-box; the app's resolveSingBoxPath() finds it there
# before consulting PATH. The whole bundle is then ad-hoc re-signed so macOS
# accepts the added binary.
#
# Usage:
#   scripts/package-macos.sh                 # native arch
#   SING_BOX=/path/to/sing-box scripts/package-macos.sh
#   scripts/package-macos.sh -platform darwin/universal   # (needs a universal sing-box)
set -euo pipefail
cd "$(dirname "$0")/.."

# Locate sing-box: SING_BOX override, else PATH.
SINGBOX="${SING_BOX:-$(command -v sing-box || true)}"
if [[ -z "${SINGBOX}" || ! -x "${SINGBOX}" ]]; then
  echo "error: sing-box not found. Install it (brew install sing-box) or set SING_BOX=/path/to/sing-box" >&2
  exit 1
fi

export PATH="${PATH}:$(go env GOPATH)/bin"

node scripts/versioned-wails-build.mjs "$@"

APP="build/bin/OpenRung.app"
RES="${APP}/Contents/Resources"
[[ -d "${APP}" ]] || { echo "error: ${APP} not found after build" >&2; exit 1; }

echo "==> bundling sing-box from ${SINGBOX}"
cp "${SINGBOX}" "${RES}/sing-box"
chmod +x "${RES}/sing-box"

# Warn if the bundled sing-box arch won't match the app (Intel vs Apple Silicon).
APP_ARCH="$(lipo -archs "${APP}/Contents/MacOS/OpenRung" 2>/dev/null || echo unknown)"
SB_ARCH="$(lipo -archs "${RES}/sing-box" 2>/dev/null || echo unknown)"
echo "    app arch: ${APP_ARCH}   sing-box arch: ${SB_ARCH}"
if [[ "${APP_ARCH}" != "unknown" && "${SB_ARCH}" != *"${APP_ARCH%% *}"* ]]; then
  echo "    WARNING: sing-box arch (${SB_ARCH}) may not match the app (${APP_ARCH}) — the friend's Mac needs a matching arch." >&2
fi

# GPL-3.0 corresponding-source notice for the bundled binary.
SB_VER="$("${SINGBOX}" version 2>/dev/null | head -1 || echo 'unknown version')"
cat > "${RES}/THIRD_PARTY_NOTICES.txt" <<EOF
This application bundles sing-box (${SB_VER}), licensed under GPL-3.0.
Source: https://github.com/SagerNet/sing-box

OpenRung is free software (GPL-3.0-or-later).
Source: https://github.com/openrung/openrung
EOF

echo "==> ad-hoc re-signing the bundle (covers the added sing-box)"
codesign --force --deep --sign - "${APP}"
codesign --verify --deep --strict "${APP}" && echo "    signature OK"

echo "==> done"
du -sh "${APP}"
echo "    bundled sing-box: ${RES}/sing-box"
echo "    ship it: ditto -c -k --keepParent ${APP} OpenRung.zip"
