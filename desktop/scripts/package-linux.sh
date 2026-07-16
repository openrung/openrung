#!/usr/bin/env bash
#
# Build the Linux app with sing-box bundled next to the binary, packaged as a
# tar.gz. Run this ON Linux (Wails cannot cross-compile from macOS/Windows).
#
# Prereqs (Debian/Ubuntu): Go, Node >=22, the Wails CLI, and
#   sudo apt-get install -y build-essential libgtk-3-dev libwebkit2gtk-4.1-dev
#
# Build against webkit2gtk 4.1 (not the removed-on-modern-distros 4.0) by passing
# the Wails tag through to this script:
#   ./package-linux.sh -tags webkit2_41
#
# Provide a Linux sing-box binary via SING_BOX=/path/to/sing-box (matching the
# target arch), or have sing-box on PATH.
set -euo pipefail
cd "$(dirname "$0")/.."

SINGBOX="${SING_BOX:-$(command -v sing-box || true)}"
if [[ -z "${SINGBOX}" || ! -x "${SINGBOX}" ]]; then
  echo "error: no sing-box. Set SING_BOX=/path/to/linux-sing-box or install it on PATH." >&2
  exit 1
fi

export PATH="${PATH}:$(go env GOPATH)/bin"
node scripts/versioned-wails-build.mjs "$@"

BIN="build/bin/OpenRung"
[[ -x "${BIN}" ]] || { echo "error: ${BIN} not found after build" >&2; exit 1; }

ARCH="$(uname -m)"
STAGE="build/OpenRung"
rm -rf "${STAGE}"; mkdir -p "${STAGE}"
cp "${BIN}" "${STAGE}/OpenRung"
cp "${SINGBOX}" "${STAGE}/sing-box"          # resolver finds it next to the binary
chmod +x "${STAGE}/OpenRung" "${STAGE}/sing-box"

SB_VER="$("${SINGBOX}" version 2>/dev/null | head -1 || echo 'unknown version')"
cat > "${STAGE}/THIRD_PARTY_NOTICES.txt" <<EOF
This application bundles sing-box (${SB_VER}), licensed under GPL-3.0.
Source: https://github.com/SagerNet/sing-box
OpenRung is free software (GPL-3.0-or-later): https://github.com/openrung/openrung
EOF

OUT="build/bin/OpenRung-linux-${ARCH}.tar.gz"
tar -czf "${OUT}" -C build OpenRung
echo "==> done: ${OUT}"
du -sh "${OUT}"
