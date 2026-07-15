#!/usr/bin/env bash
#
# Provision, convert, and update OpenRung Foundation-operated relays.
#
#   deploy/relay/foundation-up.sh create [name]        # new Lightsail host, then install credentials
#   deploy/relay/foundation-up.sh convert <host>...    # install credentials on an existing host
#   deploy/relay/foundation-up.sh update  <host>...    # pull the image and recreate, reusing credentials
#
# This wraps — and never replaces — lightsail-up.sh. That helper deliberately
# refuses registration tokens because Lightsail retains user-data, so a Foundation
# relay has always needed a second, manual step: install a root-owned mode-0600 env
# file over SSH and recreate the container with --env-file. This script automates
# exactly that step, preserving the invariant rather than weakening it: the token
# reaches the host only over SSH, after boot, and never through user-data.
#
# Overridable via env: OPENRUNG_IMAGE, OPENRUNG_BROKER_URL, OPENRUNG_ENV_FILE,
# OPENRUNG_SSH_KEY, OPENRUNG_SSH_USER. `create` also forwards OPENRUNG_REGION,
# OPENRUNG_BUNDLE, and the rest of lightsail-up.sh's own knobs.
#
# The token is read from OPENRUNG_FOUNDATION_TOKEN_CMD (preferred) or
# OPENRUNG_FOUNDATION_TOKEN. It is never accepted as a command-line argument:
# argv is world-readable via /proc and is retained in shell history.
set -euo pipefail

IMAGE="${OPENRUNG_IMAGE:-ghcr.io/openrung/openrung-relay:main}"
BROKER_URL="${OPENRUNG_BROKER_URL:-https://d2r7mdpyevvs1m.cloudfront.net}"
ENV_FILE="${OPENRUNG_ENV_FILE:-/etc/openrung/relay.env}"
SSH_KEY="${OPENRUNG_SSH_KEY:-$HOME/.ssh/id_ed25519_openrung}"
SSH_USER="${OPENRUNG_SSH_USER:-ubuntu}"

CONTAINER="openrung-relay"
LEGACY_CONTAINER="openrung-volunteer"      # pre-rename fleets still run this name
LEGACY_ENV_FILE="/etc/openrung/volunteer.env"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SSH_OPTS=(-o StrictHostKeyChecking=accept-new -o ConnectTimeout=10 -o BatchMode=yes -i "$SSH_KEY")

die() { echo "error: $*" >&2; exit 1; }
log() { echo "  $*"; }

usage() {
  sed -n '3,8p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
  exit 2
}

ssh_run() { local host="$1"; shift; ssh "${SSH_OPTS[@]}" "${SSH_USER}@${host}" "$@"; }

# The relay binary refuses to send the Foundation bearer over cleartext, so a
# non-HTTPS broker URL would fail at runtime as a confusing crash loop. Fail here
# instead, with the reason.
require_https_broker() {
  case "$BROKER_URL" in
    https://*) ;;
    *) die "OPENRUNG_BROKER_URL must be https for a Foundation relay (got '${BROKER_URL}'); the relay refuses to send the token over cleartext" ;;
  esac
}

# Print the Foundation token on stdout. Never echoed, never passed as an argument.
read_token() {
  if [ -n "${OPENRUNG_FOUNDATION_TOKEN_CMD:-}" ]; then
    eval "$OPENRUNG_FOUNDATION_TOKEN_CMD"
  elif [ -n "${OPENRUNG_FOUNDATION_TOKEN:-}" ]; then
    printf '%s\n' "$OPENRUNG_FOUNDATION_TOKEN"
  else
    die "no token source: set OPENRUNG_FOUNDATION_TOKEN_CMD (e.g. 'pass show openrung/foundation-token' or an 'aws secretsmanager get-secret-value ...' invocation) or OPENRUNG_FOUNDATION_TOKEN"
  fi
}

# Echo whichever relay container name is present on the host, preferring the
# canonical one. Empty when the host runs neither.
detect_container() {
  ssh_run "$1" "for c in ${CONTAINER} ${LEGACY_CONTAINER}; do
    if sudo docker inspect \"\$c\" >/dev/null 2>&1; then echo \"\$c\"; exit 0; fi
  done"
}

# Echo whichever env file exists, preferring the canonical path. Empty when neither.
detect_env_file() {
  ssh_run "$1" "for f in ${ENV_FILE} ${LEGACY_ENV_FILE}; do
    if sudo test -f \"\$f\"; then echo \"\$f\"; exit 0; fi
  done"
}

# Read one non-secret OPENRUNG_* value out of the running container.
container_env_value() {
  local host="$1" container="$2" key="$3"
  ssh_run "$host" "sudo docker inspect ${container} --format '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null" \
    | sed -n "s/^${key}=//p" | head -1
}

# Write the root-owned mode-0600 env file. The token arrives on stdin so it never
# appears in argv on either side of the connection.
#
# target is the path to write. Callers pass an already-present env file when the
# host has one, so a convert overwrites in place rather than leaving a second,
# stale copy of the token on disk under the other name.
install_env_file() {
  local host="$1" public_host="$2" label="$3" token="$4" target="$5"
  {
    printf 'OPENRUNG_BROKER_URL=%s\n' "$BROKER_URL"
    printf 'OPENRUNG_PUBLIC_HOST=%s\n' "$public_host"
    printf 'OPENRUNG_LISTEN_HOST=0.0.0.0\n'
    printf 'OPENRUNG_LABEL=%s\n' "$label"
    printf 'OPENRUNG_FOUNDATION_TOKEN=%s\n' "$token"
  } | ssh "${SSH_OPTS[@]}" "${SSH_USER}@${host}" \
      "sudo install -d -m 700 -o root -g root /etc/openrung \
       && sudo sh -c 'umask 077 && cat > ${target}' \
       && sudo chown root:root ${target} && sudo chmod 600 ${target}"
}

# Recreate the relay container against env_file. Keeps the previous container,
# stopped, as <name>-old so a bad image can be rolled back in place.
#
# Container hardening mirrors lightsail-up.sh exactly: --cap-drop ALL with only
# NET_BIND_SERVICE re-added (the binary binds 443 through a cap_net_bind_service
# file capability), read-only rootfs, and deliberately NO --security-opt
# no-new-privileges, which would disable that file capability and break the bind.
recreate_container() {
  local host="$1" env_file="$2" live="$3"
  ssh_run "$host" "set -e
    sudo docker pull ${IMAGE} >/dev/null
    sudo docker rm -f ${CONTAINER}-old ${LEGACY_CONTAINER}-old >/dev/null 2>&1 || true
    if [ -n '${live}' ]; then
      sudo docker stop '${live}' >/dev/null
      sudo docker rename '${live}' ${CONTAINER}-old
    fi
    sudo docker run -d --name ${CONTAINER} --restart unless-stopped \
      --network host --cap-drop ALL --cap-add NET_BIND_SERVICE --read-only --tmpfs /tmp \
      --env-file ${env_file} \
      ${IMAGE} >/dev/null"
}

# A relay that registers as anything other than Foundation exits during startup
# (the binary rejects a broker's node_class attestation that does not match what
# it claimed). So a container still running, having logged a registration, is
# proof the broker attested Foundation — no separate broker query needed.
verify_foundation() {
  local host="$1" i=0
  while [ "$i" -lt 15 ]; do
    if ssh_run "$host" "sudo docker logs ${CONTAINER} 2>&1 | grep -q 'registered relay'"; then
      ssh_run "$host" "sudo docker ps --format '{{.Names}}' | grep -qx ${CONTAINER}" \
        || die "${host}: container exited after registering; check: docker logs ${CONTAINER}"
      log "registered and running: $(ssh_run "$host" "sudo docker logs ${CONTAINER} 2>&1 | grep 'registered relay' | tail -1")"
      return 0
    fi
    if ! ssh_run "$host" "sudo docker ps --format '{{.Names}}' | grep -qx ${CONTAINER}"; then
      die "${host}: container is not running; check: docker logs ${CONTAINER}"
    fi
    i=$((i + 1)); sleep 2
  done
  die "${host}: no registration within 30s; check: docker logs ${CONTAINER}"
}

cmd_convert() {
  [ "$#" -ge 1 ] || usage
  require_https_broker
  local token; token="$(read_token)"
  [ -n "$token" ] || die "token source produced an empty token"

  for host in "$@"; do
    echo "==> convert ${host}"
    local live public_host label target
    live="$(detect_container "$host")"
    [ -n "$live" ] || die "${host}: no relay container found; provision it first (lightsail-up.sh or 'create')"

    # Reuse the identity the bootstrap helper already baked in, so a convert does
    # not silently relabel or re-home an existing relay.
    public_host="$(container_env_value "$host" "$live" OPENRUNG_PUBLIC_HOST)"
    label="$(container_env_value "$host" "$live" OPENRUNG_LABEL)"
    [ -n "$public_host" ] || die "${host}: could not read OPENRUNG_PUBLIC_HOST from ${live}"
    [ -n "$label" ] || die "${host}: could not read OPENRUNG_LABEL from ${live}"

    # Overwrite an existing env file in place; only fall back to the canonical
    # path when the host has none, so we never leave two token files behind.
    target="$(detect_env_file "$host")"
    [ -n "$target" ] || target="$ENV_FILE"

    log "identity: label=${label} public_host=${public_host}"
    install_env_file "$host" "$public_host" "$label" "$token" "$target"
    log "wrote ${target} (root:root 0600)"
    recreate_container "$host" "$target" "$live"
    verify_foundation "$host"
  done
}

cmd_update() {
  [ "$#" -ge 1 ] || usage
  # Fail-fast across the fleet: a broken image stops the roll at the first host
  # instead of taking every relay down.
  for host in "$@"; do
    echo "==> update ${host}"
    local live env_file
    env_file="$(detect_env_file "$host")"
    [ -n "$env_file" ] || die "${host}: no env file (${ENV_FILE} or ${LEGACY_ENV_FILE}); run 'convert' first"
    live="$(detect_container "$host")"
    log "reusing ${env_file}; image ${IMAGE}"
    recreate_container "$host" "$env_file" "$live"
    verify_foundation "$host"
  done
}

cmd_create() {
  require_https_broker
  read_token >/dev/null   # fail before provisioning, not after

  local out ip name
  out="$("${HERE}/lightsail-up.sh" "$@")"
  echo "$out"
  # lightsail-up.sh's machine-readable handoff line.
  name="$(printf '%s\n' "$out" | sed -n 's/^OPENRUNG_RELAY name=\([^ ]*\) ip=.*$/\1/p' | head -1)"
  ip="$(printf '%s\n' "$out" | sed -n 's/^OPENRUNG_RELAY .*ip=\(.*\)$/\1/p' | head -1)"
  [ -n "$ip" ] && [ -n "$name" ] || die "could not parse the 'OPENRUNG_RELAY name=... ip=...' line from lightsail-up.sh"

  echo "==> waiting for ${name} (${ip}) to finish bootstrapping"
  local i=0
  until ssh_run "$ip" "sudo docker inspect ${CONTAINER} >/dev/null 2>&1" 2>/dev/null; do
    i=$((i + 1))
    [ "$i" -lt 60 ] || die "${ip}: relay container did not appear within 5 minutes; check /var/log/openrung-init.log"
    sleep 5
  done
  cmd_convert "$ip"
}

[ "$#" -ge 1 ] || usage
subcommand="$1"; shift
case "$subcommand" in
  create)  cmd_create "$@" ;;
  convert) cmd_convert "$@" ;;
  update)  cmd_update "$@" ;;
  *)       usage ;;
esac
