#!/usr/bin/env bash
#
# Build the macOS .app. The app statically links the sing-box engine
# (internal/singboxruntime) and runs it by re-invoking its own binary, so
# nothing has to be bundled next to it and nothing is looked up on PATH — the
# .app runs on a Mac that has never installed sing-box. The GPL-3.0 notices
# for that linked engine still travel with the package, and the bundle is
# ad-hoc re-signed because adding them changes it after the build.
#
# Usage:
#   scripts/package-macos.sh                 # native arch
#   scripts/package-macos.sh -platform darwin/universal
set -euo pipefail
cd "$(dirname "$0")/.."

export PATH="${PATH}:$(go env GOPATH)/bin"
# sing-box's Apple networking transport uses APIs introduced through macOS 12.
# Keep the compiler target aligned with the bundle metadata.
export MACOSX_DEPLOYMENT_TARGET=12.0

node scripts/versioned-wails-build.mjs "$@"

APP="build/bin/OpenRung.app"
RES="${APP}/Contents/Resources"
[[ -d "${APP}" ]] || { echo "error: ${APP} not found after build" >&2; exit 1; }

# Refuses to package a build that lost a sing-box build tag or the app-version
# stamp; also yields the engine version line for the notices below.
SB_VER="$(node scripts/verify-bundled-engine.mjs "${APP}/Contents/MacOS/OpenRung")"
echo "==> verified bundled engine: ${SB_VER}"

# The full repository notices and the GPL text must travel with the package
# (the pointer .txt below is not a substitute for them).
cp ../THIRD_PARTY_NOTICES.md "${RES}/THIRD_PARTY_NOTICES.md"
cp ../LICENSE "${RES}/LICENSE"

# GPL-3.0 corresponding-source notice for the statically linked engine.
cat > "${RES}/THIRD_PARTY_NOTICES.txt" <<EOF
This application statically links ${SB_VER}, licensed GPL-3.0-or-later
(text: LICENSE in this folder). Third-party notices are in
THIRD_PARTY_NOTICES.md alongside this file.

OpenRung is free software (GPL-3.0-or-later).
Source: https://github.com/openrung/openrung
EOF

echo "==> ad-hoc re-signing the bundle (covers the added notices files)"
codesign --force --deep --sign - "${APP}"
codesign --verify --deep --strict "${APP}" && echo "    signature OK"

echo "==> done"
du -sh "${APP}"
echo "    ship it: ditto -c -k --keepParent ${APP} OpenRung.zip"
