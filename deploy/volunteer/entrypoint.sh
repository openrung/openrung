#!/bin/sh
# Translate OPENRUNG_* environment variables into volunteer CLI flags, then exec
# the binary so it receives SIGTERM directly for a clean shutdown.
set -eu

if [ "${OPENRUNG_TUNNEL:-}" = "true" ] || [ "${OPENRUNG_TUNNEL:-}" = "1" ]; then
  # --- CGNAT reverse-tunnel mode ---
  # The relay hub supplies the public endpoint and registers the relay with the
  # broker, so neither OPENRUNG_PUBLIC_HOST nor OPENRUNG_BROKER_URL is needed
  # here. Only the hub address is required. In this mode the volunteer makes no
  # inbound connections, so it needs no public port and no NET_BIND_SERVICE.
  : "${OPENRUNG_HUB_ADDR:?OPENRUNG_HUB_ADDR is required in tunnel mode (e.g. hub.example.com:9443)}"
  set -- /usr/local/bin/volunteer \
    -tunnel \
    -hub "${OPENRUNG_HUB_ADDR}" \
    -xray "${OPENRUNG_XRAY_PATH:-/usr/local/bin/xray}" \
    -config-out "${OPENRUNG_CONFIG_OUT:-/tmp/openrung-xray-config.json}"
  # Bool flags: Go's flag package requires the = form.
  if [ -n "${OPENRUNG_HUB_TLS:-}" ]; then set -- "$@" "-hub-tls=${OPENRUNG_HUB_TLS}"; fi
  if [ -n "${OPENRUNG_HUB_INSECURE:-}" ]; then set -- "$@" "-hub-insecure=${OPENRUNG_HUB_INSECURE}"; fi
  echo "openrung-volunteer: tunnel mode hub=${OPENRUNG_HUB_ADDR}" >&2
else
  # --- Direct-exit mode (volunteer exposes a public port) ---
  # A container cannot reliably auto-detect the host's public address, so the
  # public host must always be provided explicitly.
  : "${OPENRUNG_BROKER_URL:?OPENRUNG_BROKER_URL is required (e.g. https://broker.example.com)}"
  : "${OPENRUNG_PUBLIC_HOST:?OPENRUNG_PUBLIC_HOST is required — a container cannot auto-detect the host public IP; set it to this server's public IP or DNS name}"
  set -- /usr/local/bin/volunteer \
    -broker "${OPENRUNG_BROKER_URL}" \
    -public-host "${OPENRUNG_PUBLIC_HOST}" \
    -public-port "${OPENRUNG_PUBLIC_PORT:-443}" \
    -listen-host "${OPENRUNG_LISTEN_HOST:-::}" \
    -listen-port "${OPENRUNG_LISTEN_PORT:-443}" \
    -xray "${OPENRUNG_XRAY_PATH:-/usr/local/bin/xray}" \
    -config-out "${OPENRUNG_CONFIG_OUT:-/tmp/openrung-xray-config.json}"
  if [ -n "${OPENRUNG_HEARTBEAT_INTERVAL:-}" ]; then set -- "$@" -heartbeat-interval "${OPENRUNG_HEARTBEAT_INTERVAL}"; fi
  # Bool flag: Go's flag package requires the = form (-connection-log=false).
  if [ -n "${OPENRUNG_CONNECTION_LOG:-}" ]; then set -- "$@" "-connection-log=${OPENRUNG_CONNECTION_LOG}"; fi
  echo "openrung-volunteer: broker=${OPENRUNG_BROKER_URL} public=${OPENRUNG_PUBLIC_HOST}:${OPENRUNG_PUBLIC_PORT:-443}" >&2
fi

# --- Optional flags shared by both modes (appended only when the env var is set) ---
if [ -n "${OPENRUNG_SERVER_NAME:-}" ]; then set -- "$@" -server-name "${OPENRUNG_SERVER_NAME}"; fi
if [ -n "${OPENRUNG_REALITY_DEST:-}" ]; then set -- "$@" -reality-dest "${OPENRUNG_REALITY_DEST}"; fi
if [ -n "${OPENRUNG_CLIENT_ID:-}" ]; then set -- "$@" -client-id "${OPENRUNG_CLIENT_ID}"; fi
if [ -n "${OPENRUNG_REALITY_PRIVATE_KEY:-}" ]; then set -- "$@" -reality-private-key "${OPENRUNG_REALITY_PRIVATE_KEY}"; fi
if [ -n "${OPENRUNG_REALITY_PUBLIC_KEY:-}" ]; then set -- "$@" -reality-public-key "${OPENRUNG_REALITY_PUBLIC_KEY}"; fi
if [ -n "${OPENRUNG_SHORT_ID:-}" ]; then set -- "$@" -short-id "${OPENRUNG_SHORT_ID}"; fi
if [ -n "${OPENRUNG_MAX_SESSIONS:-}" ]; then set -- "$@" -max-sessions "${OPENRUNG_MAX_SESSIONS}"; fi
if [ -n "${OPENRUNG_MAX_MBPS:-}" ]; then set -- "$@" -max-mbps "${OPENRUNG_MAX_MBPS}"; fi

# The registration token (OPENRUNG_VOLUNTEER_TOKEN) and label (OPENRUNG_LABEL)
# are read natively from the environment by the binary, so they need no flag
# mapping.

exec "$@"
