#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Build the desktop app from this checkout and run it against the network lab
# (test/e2e) left running on this machine's network by `make e2e-lab-host`.
# Linux only: the lab attaches to the network of the machine running Docker.
#
#   ./desktop/scripts/run-against-lab.sh             build, then run
#   ./desktop/scripts/run-against-lab.sh --no-build  run the last build
#
# The app is given:
#   OPENRUNG_BROKER_URL  the lab broker, which the lab serves on this machine's
#                        loopback
#   SSL_CERT_FILE        the lab's certificate authority, and no other: the
#                        app's connectivity checks reach the lab's internet
#                        stand-in, and the public broker fronts it would fall
#                        back to fail verification
#   XDG_CONFIG_HOME      desktop/build/lab/config, so its identity, recents and
#                        proxy port stay apart from an installed app's
set -euo pipefail
cd "$(dirname "$0")/.."

container="${LAB_CONTAINER:-openrung-lab}"
broker="http://127.0.0.1:18080" # lab.HostBrokerURL
state="$PWD/build/lab"

build=1
for arg in "$@"; do
  case "$arg" in
    --no-build) build=0 ;;
    *) echo "usage: $0 [--no-build]" >&2; exit 2 ;;
  esac
done

[[ "$(uname -s)" == Linux ]] || { echo "error: the lab attaches only to a Linux machine's network" >&2; exit 1; }
if ! docker exec "$container" lab status >/dev/null 2>&1; then
  echo "error: no lab is running in container $container; start one with \`make e2e-lab-host\`" >&2
  exit 1
fi
if ! ip link show or-lab >/dev/null 2>&1; then
  echo "error: the lab is not attached to this machine's network; start it with \`make e2e-lab-host\` instead of \`make e2e-lab\`" >&2
  exit 1
fi

if (( build )); then
  tags=()
  # Distributions that dropped webkit2gtk 4.0 need the 4.1 build tag.
  if pkg-config --exists webkit2gtk-4.1 2>/dev/null; then tags=(-tags webkit2_41); fi
  export PATH="${PATH}:$(go env GOPATH)/bin"
  node scripts/versioned-wails-build.mjs "${tags[@]}"
fi

mkdir -p "$state/config"
# The lab mints a new certificate authority every time it starts.
docker exec "$container" cat /run/lab/ca.pem > "$state/ca.pem"

echo "==> OpenRung against the lab broker $broker (state in $state)"
exec env OPENRUNG_BROKER_URL="$broker" SSL_CERT_FILE="$state/ca.pem" XDG_CONFIG_HOME="$state/config" \
  build/bin/OpenRung
