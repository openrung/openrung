#!/usr/bin/env bash
#
# Build the Linux app, packaged as a tar.gz. The app statically links the
# sing-box engine (internal/singboxruntime) and runs it by re-invoking its own
# binary, so the package is one executable plus its notices — no sing-box to
# install, bundle, or find on PATH.
#
# Prereqs (Debian/Ubuntu): Go, Node >=22, the Wails CLI, and
#   sudo apt-get install -y build-essential libgtk-3-dev libwebkit2gtk-4.1-dev
#
# Build against webkit2gtk 4.1 (not the removed-on-modern-distros 4.0) by passing
# the Wails tag through to this script:
#   ./package-linux.sh -tags webkit2_41
set -euo pipefail
cd "$(dirname "$0")/.."

export PATH="${PATH}:$(go env GOPATH)/bin"
node scripts/versioned-wails-build.mjs "$@"

BIN="build/bin/OpenRung"
[[ -x "${BIN}" ]] || { echo "error: ${BIN} not found after build" >&2; exit 1; }

# Refuses to package a build that lost a sing-box build tag or the app-version
# stamp; also yields the engine version line for the notices below.
SB_VER="$(node scripts/verify-bundled-engine.mjs "${BIN}")"
echo "==> verified bundled engine: ${SB_VER}"

ARCH="$(uname -m)"
STAGE="build/OpenRung"
rm -rf "${STAGE}"; mkdir -p "${STAGE}"
cp "${BIN}" "${STAGE}/OpenRung"
chmod +x "${STAGE}/OpenRung"

# The full repository notices and the GPL text must travel with the package
# (the pointer .txt below is not a substitute for them).
cp ../THIRD_PARTY_NOTICES.md "${STAGE}/THIRD_PARTY_NOTICES.md"
cp ../LICENSE "${STAGE}/LICENSE"

cat > "${STAGE}/THIRD_PARTY_NOTICES.txt" <<EOF
This application statically links ${SB_VER}, licensed GPL-3.0-or-later
(text: LICENSE). Third-party notices are in THIRD_PARTY_NOTICES.md alongside
this file. OpenRung is free software (GPL-3.0-or-later):
https://github.com/openrung/openrung
EOF

OUT="build/bin/OpenRung-linux-${ARCH}.tar.gz"
tar -czf "${OUT}" -C build OpenRung
echo "==> done: ${OUT}"
du -sh "${OUT}"
