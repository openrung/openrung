#!/usr/bin/env bash
#
# Build the Linux app with xray bundled next to the binary, packaged as a
# tar.gz. Run this ON Linux (Wails cannot cross-compile from macOS/Windows).
#
# Prereqs (Debian/Ubuntu): Go, Node >=22, the Wails CLI, and
#   sudo apt-get install -y build-essential libgtk-3-dev libwebkit2gtk-4.1-dev
#
# Build against webkit2gtk 4.1 (not the removed-on-modern-distros 4.0) by passing
# the Wails tag through to this script:
#   ./package-linux.sh -tags webkit2_41
#
# Provide a Linux xray binary via XRAY=/path/to/xray (matching the target
# arch), or have xray on PATH.
set -euo pipefail
cd "$(dirname "$0")/.."

XRAYBIN="${XRAY:-$(command -v xray || true)}"
if [[ -z "${XRAYBIN}" || ! -x "${XRAYBIN}" ]]; then
  echo "error: no xray. Set XRAY=/path/to/linux-xray or install it on PATH." >&2
  exit 1
fi

export PATH="${PATH}:$(go env GOPATH)/bin"
echo "==> wails build ${*:-}"
wails build "$@"

BIN="build/bin/OpenRungVolunteer"
[[ -x "${BIN}" ]] || { echo "error: ${BIN} not found after build" >&2; exit 1; }

ARCH="$(uname -m)"
STAGE="build/OpenRungVolunteer"
rm -rf "${STAGE}"; mkdir -p "${STAGE}"
cp "${BIN}" "${STAGE}/OpenRungVolunteer"
cp "${XRAYBIN}" "${STAGE}/xray"              # resolver finds it next to the binary
chmod +x "${STAGE}/OpenRungVolunteer" "${STAGE}/xray"

XR_VER="$("${XRAYBIN}" version 2>/dev/null | head -1 || echo 'unknown version')"
cat > "${STAGE}/THIRD_PARTY_NOTICES.txt" <<EOF
This application bundles Xray-core (${XR_VER}), licensed under MPL-2.0.
It is included unmodified and runs as a separate process.
Source: https://github.com/XTLS/Xray-core
License text: https://www.mozilla.org/MPL/2.0/
OpenRung Volunteer is free software (GPL-3.0-or-later): https://github.com/openrung/openrung
EOF

# Full corresponding-source license texts (GPL-3.0-or-later §4/§6 require the
# License to accompany every conveyed binary; MPL-2.0 §3.1 requires Xray's).
# XRAY_LICENSE (Xray's LICENSE from the release zip) is set by CI; skipped for a
# local PATH-xray build.
cp ../LICENSE "${STAGE}/LICENSE.txt"
cp ../THIRD_PARTY_NOTICES.md "${STAGE}/THIRD_PARTY_NOTICES.md"
if [[ -n "${XRAY_LICENSE:-}" && -f "${XRAY_LICENSE}" ]]; then
  cp "${XRAY_LICENSE}" "${STAGE}/XRAY-LICENSE.txt"
fi

OUT="build/bin/OpenRungVolunteer-linux-${ARCH}.tar.gz"
tar -czf "${OUT}" -C build OpenRungVolunteer
echo "==> done: ${OUT}"
du -sh "${OUT}"
