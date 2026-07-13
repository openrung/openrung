#!/bin/sh
# Translate OPENRUNG_* environment variables into volunteer CLI flags, then exec
# the binary so it receives SIGTERM directly for a clean shutdown.
set -eu

# Resolve the connection mode the same way the binary's normalizeMode does:
# lowercase and trim OPENRUNG_MODE, then, when it is unset, fall back to the
# legacy OPENRUNG_TUNNEL boolean, then to auto when a hub is configured, else
# direct. The resolved value is passed back as -mode so this wrapper and the
# binary can never disagree about which mode is active.
norm() { printf '%s' "$1" | tr 'A-Z' 'a-z' | sed 's/^[[:space:]]*//; s/[[:space:]]*$//'; }
mode="$(norm "${OPENRUNG_MODE:-}")"
if [ -n "${OPENRUNG_FOUNDATION_TOKEN:-}" ]; then
  # A foundation token forces direct mode, mirroring the binary's
  # applyFoundationTokenPosture. This must win over OPENRUNG_MODE/OPENRUNG_TUNNEL/
  # a stale hub: resolving tunnel or auto here would take a branch that omits the
  # broker URL and public host that direct mode needs, and the binary would then
  # flip to direct with wrong defaults (e.g. the localhost broker). Forcing it
  # here makes the direct branch require and pass OPENRUNG_BROKER_URL and
  # OPENRUNG_PUBLIC_HOST.
  mode=direct
elif [ -z "$mode" ]; then
  # Match the binary's boolEnv for the legacy OPENRUNG_TUNNEL boolean: lowercase,
  # trim, and accept 1/true/yes/on. A narrower test here would reject a tunnel
  # config the binary accepts, since the resolved -mode below is authoritative.
  case "$(norm "${OPENRUNG_TUNNEL:-}")" in
    1 | true | yes | on)
      mode=tunnel
      ;;
    *)
      if [ -n "${OPENRUNG_HUB_ADDR:-}" ]; then
        mode=auto
      else
        mode=direct
      fi
      ;;
  esac
fi

case "$mode" in
  tunnel | direct | auto) ;;
  *)
    echo "openrung-volunteer: OPENRUNG_MODE must be auto, direct, or tunnel (got '${OPENRUNG_MODE:-}')" >&2
    exit 2
    ;;
esac

set -- /usr/local/bin/volunteer \
  -mode "$mode" \
  -xray "${OPENRUNG_XRAY_PATH:-/usr/local/bin/xray}" \
  -config-out "${OPENRUNG_CONFIG_OUT:-/tmp/openrung-xray-config.json}"

# Hub flags: tunnel dials the hub, and auto probes it (and may fall back to it),
# so both require a hub address. Direct never contacts a hub.
case "$mode" in
  tunnel | auto)
    : "${OPENRUNG_HUB_ADDR:?OPENRUNG_HUB_ADDR is required in ${mode} mode (e.g. hub.example.com:9443)}"
    set -- "$@" -hub "${OPENRUNG_HUB_ADDR}"
    # Bool flags: Go's flag package requires the = form.
    if [ -n "${OPENRUNG_HUB_TLS:-}" ]; then set -- "$@" "-hub-tls=${OPENRUNG_HUB_TLS}"; fi
    if [ -n "${OPENRUNG_HUB_INSECURE:-}" ]; then set -- "$@" "-hub-insecure=${OPENRUNG_HUB_INSECURE}"; fi
    ;;
esac

# Broker flags: direct and auto register with the broker themselves (auto does so
# when the reachability probe finds the relay directly reachable). Tunnel relays
# are registered by the hub and never call the broker.
case "$mode" in
  direct | auto)
    : "${OPENRUNG_BROKER_URL:?OPENRUNG_BROKER_URL is required in ${mode} mode (e.g. https://broker.example.com)}"
    set -- "$@" \
      -broker "${OPENRUNG_BROKER_URL}" \
      -listen-host "${OPENRUNG_LISTEN_HOST:-::}" \
      -listen-port "${OPENRUNG_LISTEN_PORT:-443}"
    if [ -n "${OPENRUNG_HEARTBEAT_INTERVAL:-}" ]; then set -- "$@" -heartbeat-interval "${OPENRUNG_HEARTBEAT_INTERVAL}"; fi
    # Bool flag: Go's flag package requires the = form (-connection-log=false).
    if [ -n "${OPENRUNG_CONNECTION_LOG:-}" ]; then set -- "$@" "-connection-log=${OPENRUNG_CONNECTION_LOG}"; fi
    ;;
esac

# Public endpoint: a container cannot auto-detect its public address, so direct
# mode requires it explicitly. Auto learns it from the reachability probe when
# the relay is directly reachable, so it is optional there; tunnel gets its
# public endpoint from the hub and never uses it.
case "$mode" in
  direct)
    : "${OPENRUNG_PUBLIC_HOST:?OPENRUNG_PUBLIC_HOST is required in direct mode — a container cannot auto-detect the host public IP; set it to the server public IP or DNS name}"
    set -- "$@" \
      -public-host "${OPENRUNG_PUBLIC_HOST}" \
      -public-port "${OPENRUNG_PUBLIC_PORT:-443}"
    ;;
  auto)
    if [ -n "${OPENRUNG_PUBLIC_HOST:-}" ]; then set -- "$@" -public-host "${OPENRUNG_PUBLIC_HOST}"; fi
    if [ -n "${OPENRUNG_PUBLIC_PORT:-}" ]; then set -- "$@" -public-port "${OPENRUNG_PUBLIC_PORT}"; fi
    ;;
esac

echo "openrung-volunteer: mode=${mode}${OPENRUNG_HUB_ADDR:+ hub=${OPENRUNG_HUB_ADDR}}${OPENRUNG_BROKER_URL:+ broker=${OPENRUNG_BROKER_URL}}" >&2

# --- Optional flags shared by all modes (appended only when the env var is set) ---
if [ -n "${OPENRUNG_SERVER_NAME:-}" ]; then set -- "$@" -server-name "${OPENRUNG_SERVER_NAME}"; fi
if [ -n "${OPENRUNG_REALITY_DEST:-}" ]; then set -- "$@" -reality-dest "${OPENRUNG_REALITY_DEST}"; fi
if [ -n "${OPENRUNG_CLIENT_ID:-}" ]; then set -- "$@" -client-id "${OPENRUNG_CLIENT_ID}"; fi
if [ -n "${OPENRUNG_REALITY_PRIVATE_KEY:-}" ]; then set -- "$@" -reality-private-key "${OPENRUNG_REALITY_PRIVATE_KEY}"; fi
if [ -n "${OPENRUNG_REALITY_PUBLIC_KEY:-}" ]; then set -- "$@" -reality-public-key "${OPENRUNG_REALITY_PUBLIC_KEY}"; fi
if [ -n "${OPENRUNG_SHORT_ID:-}" ]; then set -- "$@" -short-id "${OPENRUNG_SHORT_ID}"; fi
if [ -n "${OPENRUNG_MAX_SESSIONS:-}" ]; then set -- "$@" -max-sessions "${OPENRUNG_MAX_SESSIONS}"; fi
if [ -n "${OPENRUNG_MAX_MBPS:-}" ]; then set -- "$@" -max-mbps "${OPENRUNG_MAX_MBPS}"; fi

# The registration tokens (OPENRUNG_VOLUNTEER_TOKEN, OPENRUNG_FOUNDATION_TOKEN),
# label (OPENRUNG_LABEL), and node class (OPENRUNG_NODE_CLASS) are read natively
# from the environment by the binary, so they need no flag mapping.

exec "$@"
