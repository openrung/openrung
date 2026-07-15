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
# reaches the host only over SSH, after boot, and never through user-data. `create`
# even strips the token variables from lightsail-up.sh's environment, so the
# provisioning step structurally cannot see the credential, let alone embed it.
#
# The token is read from OPENRUNG_FOUNDATION_TOKEN_CMD (preferred; run via
# `bash -c`, first output line is the token) or OPENRUNG_FOUNDATION_TOKEN. It is
# never accepted as a command-line argument: argv is world-readable via /proc and
# is retained in shell history.
#
# The broker URL defaults to the broker's own TLS origin, NOT a CDN front. A
# front decrypts the Foundation bearer at every edge POP; the direct origin
# (deploy/broker/origin-tls.md) means only the broker box ever sees it, and the
# broker rate-limits each relay by its real IP instead of by shared edge IPs.
#
# Overridable via env: OPENRUNG_IMAGE, OPENRUNG_BROKER_URL, OPENRUNG_ENV_FILE,
# OPENRUNG_SSH_KEY, OPENRUNG_SSH_USER, OPENRUNG_REGION (host-key pinning lookups).
# `create` also forwards OPENRUNG_BUNDLE and the rest of lightsail-up.sh's knobs.
set -euo pipefail

# A credential transits this script's variables: keep xtrace off even under
# `bash -x`, so the token can never appear in trace output.
{ set +x; } 2>/dev/null

IMAGE="${OPENRUNG_IMAGE:-ghcr.io/openrung/openrung-relay:main}"
BROKER_URL="${OPENRUNG_BROKER_URL:-https://broker-origin.openrung.org}"
ENV_FILE="${OPENRUNG_ENV_FILE:-/etc/openrung/relay.env}"
SSH_KEY="${OPENRUNG_SSH_KEY:-$HOME/.ssh/id_ed25519_openrung}"
SSH_USER="${OPENRUNG_SSH_USER:-ubuntu}"
REGION="${OPENRUNG_REGION:-ap-northeast-1}"

CONTAINER="openrung-relay"
LEGACY_CONTAINER="openrung-volunteer"      # pre-rename fleets still run this name
LEGACY_ENV_FILE="/etc/openrung/volunteer.env"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# accept-new never accepts a CHANGED key; pin_host_key (below) upgrades first
# contact from trust-on-first-use to verified whenever the Lightsail API can
# vouch for the host key.
SSH_OPTS=(-o StrictHostKeyChecking=accept-new -o ConnectTimeout=10 -o BatchMode=yes -i "$SSH_KEY")

TOKEN=""   # set once per run by the commands that need it; never leaves this process except over SSH stdin

die()  { echo "error: $*" >&2; exit 1; }
log()  { echo "  $*"; }
warn() { echo "  warning: $*" >&2; }

usage() {
  cat >&2 <<'EOF'
usage:
  deploy/relay/foundation-up.sh create [name]        # new Lightsail host, then install credentials
  deploy/relay/foundation-up.sh convert <host>...    # install credentials on an existing host
  deploy/relay/foundation-up.sh update  <host>...    # pull the image and recreate, reusing credentials

The Foundation token is read from OPENRUNG_FOUNDATION_TOKEN_CMD (preferred) or
OPENRUNG_FOUNDATION_TOKEN — never from argv. `update` needs no token.
EOF
  exit 2
}

ssh_run() { local host="$1"; shift; ssh "${SSH_OPTS[@]}" "${SSH_USER}@${host}" "$@"; }

# --- input validation -------------------------------------------------------
#
# Everything read back from a host (container names, env-file paths, env values,
# log lines) is attacker-controlled if that host is compromised. Nothing remote
# is interpolated into another command or echoed to the terminal until it has
# passed one of these allowlists; display-only strings are stripped of control
# characters so a hostile value cannot inject terminal escapes.

NAME_RE='^[A-Za-z0-9][A-Za-z0-9._-]*$'      # instance names, labels, container names
HOST_RE='^[A-Za-z0-9][A-Za-z0-9.:-]*$'      # IPv4, IPv6, DNS names

scrub() { tr -cd '[:print:]'; }

assert_matches() { # value regex what
  local val="$1" re="$2" what="$3"
  [[ "$val" =~ $re ]] || die "unexpected ${what}: '$(printf '%s' "$val" | scrub | head -c 100)'"
}

# Exactly one of the two known values (or empty): remote detection output must
# never become a free-form string in a later privileged command.
assert_one_of() { # value what allowed...
  local val="$1" what="$2"; shift 2
  [ -z "$val" ] && return 0
  local allowed
  for allowed in "$@"; do [ "$val" = "$allowed" ] && return 0; done
  die "host returned an unexpected ${what}: '$(printf '%s' "$val" | scrub | head -c 100)'"
}

# The relay binary refuses to send the Foundation bearer over cleartext, so a
# non-HTTPS broker URL would fail at runtime as a confusing crash loop. Fail here
# instead, with the reason. The URL is also written into the env file, so reject
# whitespace and control characters (an embedded newline would inject extra
# variables into the file).
require_https_broker() {
  case "$BROKER_URL" in
    https://*) ;;
    *) die "OPENRUNG_BROKER_URL must be https for a Foundation relay (got '$(printf '%s' "$BROKER_URL" | scrub)'); the relay refuses to send the token over cleartext" ;;
  esac
  assert_matches "$BROKER_URL" '^[[:graph:]]+$' "OPENRUNG_BROKER_URL"
}

# Operator-set values that get interpolated into privileged remote commands.
validate_config() {
  assert_matches "$ENV_FILE" '^/[A-Za-z0-9/._-]+$' "OPENRUNG_ENV_FILE"
  assert_matches "$IMAGE" '^[A-Za-z0-9][A-Za-z0-9:/@._-]*$' "OPENRUNG_IMAGE"
}

# --- credentials ------------------------------------------------------------

# Print the Foundation token on stdout. Never echoed, never in argv. The token
# command runs under `bash -c` in a child shell — not `eval` — so it cannot
# read or clobber this script's own state, and only its first output line is
# taken (matching `pass show`, whose later lines are metadata).
read_token() {
  { set +x; } 2>/dev/null
  local raw
  if [ -n "${OPENRUNG_FOUNDATION_TOKEN_CMD:-}" ]; then
    raw="$(bash -c "$OPENRUNG_FOUNDATION_TOKEN_CMD")" || die "OPENRUNG_FOUNDATION_TOKEN_CMD failed"
  elif [ -n "${OPENRUNG_FOUNDATION_TOKEN:-}" ]; then
    raw="$OPENRUNG_FOUNDATION_TOKEN"
  else
    die "no token source: set OPENRUNG_FOUNDATION_TOKEN_CMD (e.g. 'pass show openrung/foundation-token' or an 'aws secretsmanager get-secret-value ...' invocation) or OPENRUNG_FOUNDATION_TOKEN"
  fi
  raw="${raw%%$'\n'*}"
  [ -n "$raw" ] || die "token source produced an empty token"
  # The token is written into a KEY=VALUE env file: whitespace or control
  # characters would corrupt it (or inject extra variables), so refuse them.
  [[ "$raw" =~ ^[[:graph:]]+$ ]] || die "token contains whitespace or control characters; refusing to write it to an env file"
  printf '%s' "$raw"
}

# --- SSH host identity ------------------------------------------------------

# Upgrade first contact from trust-on-first-use to verified: Lightsail publishes
# each instance's SSH host keys over the (TLS, SigV4-authenticated) AWS API, so
# pin them into known_hosts before the first connection that will carry the
# token. Best-effort — a non-Lightsail host or an instance outside
# OPENRUNG_REGION falls back to accept-new, loudly.
pin_host_key() { # host [instance-name] [attempts]
  local host="$1" name="${2:-}" attempts="${3:-1}" keys="" i=0
  ssh-keygen -F "$host" >/dev/null 2>&1 && return 0

  if [ -z "$name" ]; then
    name="$(aws lightsail get-instances --region "$REGION" \
      --query "instances[?publicIpAddress=='${host}'].name | [0]" --output text 2>/dev/null)" || name=""
    [ "$name" = "None" ] && name=""
  fi
  if [ -n "$name" ] && [[ "$name" =~ $NAME_RE ]]; then
    while [ "$i" -lt "$attempts" ]; do
      keys="$(aws lightsail get-instance-access-details --instance-name "$name" --region "$REGION" \
        --query 'accessDetails.hostKeys[].[algorithm,publicKey]' --output text 2>/dev/null)" || keys=""
      [ -n "$keys" ] && [ "$keys" != "None" ] && break
      keys=""
      i=$((i + 1)); [ "$i" -lt "$attempts" ] && sleep 10
    done
  fi
  if [ -z "$keys" ]; then
    warn "${host}: no Lightsail-published host key (non-Lightsail host, or not in ${REGION}); first contact is trust-on-first-use"
    return 0
  fi

  mkdir -p "$HOME/.ssh"
  [ -f "$HOME/.ssh/known_hosts" ] || { touch "$HOME/.ssh/known_hosts"; chmod 600 "$HOME/.ssh/known_hosts"; }
  local algo key
  while read -r algo key; do
    [ -n "$algo" ] && [ -n "$key" ] || continue
    assert_matches "$algo" '^[A-Za-z0-9@.-]+$' "host-key algorithm"
    assert_matches "$key" '^[A-Za-z0-9+/=]+$' "host-key material"
    printf '%s %s %s\n' "$host" "$algo" "$key" >> "$HOME/.ssh/known_hosts"
  done <<< "$keys"
  log "pinned ${host}'s SSH host key from the Lightsail API"
}

# --- host inspection --------------------------------------------------------

# Echo whichever relay container name is present on the host, preferring the
# canonical one. Empty when the host runs neither.
detect_container() {
  local found
  found="$(ssh_run "$1" "for c in ${CONTAINER} ${LEGACY_CONTAINER}; do
    if sudo docker inspect \"\$c\" >/dev/null 2>&1; then echo \"\$c\"; exit 0; fi
  done")"
  assert_one_of "$found" "container name" "$CONTAINER" "$LEGACY_CONTAINER"
  printf '%s' "$found"
}

# Echo whichever env file exists, preferring the canonical path. Empty when neither.
detect_env_file() {
  local found
  found="$(ssh_run "$1" "for f in ${ENV_FILE} ${LEGACY_ENV_FILE}; do
    if sudo test -f \"\$f\"; then echo \"\$f\"; exit 0; fi
  done")"
  assert_one_of "$found" "env-file path" "$ENV_FILE" "$LEGACY_ENV_FILE"
  printf '%s' "$found"
}

# Read one non-secret OPENRUNG_* value out of the running container. The filter
# runs on the host, so secret-bearing env lines never transit the connection.
container_env_value() {
  local host="$1" container="$2" key="$3"
  ssh_run "$host" "sudo docker inspect ${container} --format '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null \
    | sed -n 's/^${key}=//p' | head -1"
}

# Confirm the env file carries the Foundation credential (presence only — the
# value is never read back). Without this, `update` would happily roll and
# "verify" a host that is not a Foundation relay at all.
require_env_has_token() {
  local host="$1" file="$2"
  ssh_run "$host" "sudo grep -q '^OPENRUNG_FOUNDATION_TOKEN=' ${file}" \
    || die "${host}: ${file} has no OPENRUNG_FOUNDATION_TOKEN; not a Foundation relay — run 'convert' first"
}

# --- mutation ---------------------------------------------------------------

# Write the root-owned mode-0600 env file. The token arrives on stdin so it never
# appears in argv on either side of the connection, and the write is atomic
# (tmp + rename) so a dropped connection cannot leave a truncated credential file.
install_env_file() { # host public_host label   (token from $TOKEN, target is $ENV_FILE)
  { set +x; } 2>/dev/null
  local host="$1" public_host="$2" label="$3"
  {
    printf 'OPENRUNG_BROKER_URL=%s\n' "$BROKER_URL"
    printf 'OPENRUNG_PUBLIC_HOST=%s\n' "$public_host"
    printf 'OPENRUNG_LISTEN_HOST=0.0.0.0\n'
    printf 'OPENRUNG_LABEL=%s\n' "$label"
    printf 'OPENRUNG_FOUNDATION_TOKEN=%s\n' "$TOKEN"
  } | ssh "${SSH_OPTS[@]}" "${SSH_USER}@${host}" \
      "sudo install -d -m 700 -o root -g root /etc/openrung \
       && sudo sh -c 'umask 077 && cat > ${ENV_FILE}.tmp && mv -f ${ENV_FILE}.tmp ${ENV_FILE}'"
}

# A convert writes the canonical path; once the relay has verified, remove the
# legacy volunteer.env so exactly one copy of the token remains on disk. (The
# rollback container does not need the file: its env was baked in at create.)
cleanup_legacy_env() {
  local host="$1"
  [ "$ENV_FILE" = "$LEGACY_ENV_FILE" ] && return 0   # operator pointed the canonical path at the legacy one
  if ssh_run "$host" "sudo test -f ${LEGACY_ENV_FILE}"; then
    ssh_run "$host" "sudo sh -c 'shred -u ${LEGACY_ENV_FILE} 2>/dev/null || rm -f ${LEGACY_ENV_FILE}'"
    log "removed legacy ${LEGACY_ENV_FILE}; credentials now live only at ${ENV_FILE}"
  fi
}

rollback_hint() {
  local host="$1"
  echo "  rollback: ssh ${SSH_USER}@${host} 'sudo docker rm -f ${CONTAINER}; sudo docker rename ${CONTAINER}-old ${CONTAINER} && sudo docker start ${CONTAINER}'" >&2
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
      ${IMAGE} >/dev/null" \
    || { rollback_hint "$host"; die "${host}: container recreate failed"; }
}

# A relay that presents the Foundation token forces node_class=foundation, and it
# exits during startup if the broker attests any other class (cmd/relay refuses a
# mismatched attestation; the broker 403s a foundation claim without the token).
# So a container that is still running and has logged a registration IS the proof
# the broker attested Foundation — no separate broker query needed. This holds
# only because convert always writes the token and update asserts its presence.
verify_foundation() {
  local host="$1" i=0 line
  while [ "$i" -lt 30 ]; do
    if ssh_run "$host" "sudo docker logs ${CONTAINER} 2>&1 | grep -q 'registered relay'"; then
      ssh_run "$host" "sudo docker ps --format '{{.Names}}' | grep -qx ${CONTAINER}" \
        || { rollback_hint "$host"; die "${host}: container exited after registering; check: docker logs ${CONTAINER}"; }
      line="$(ssh_run "$host" "sudo docker logs ${CONTAINER} 2>&1 | grep 'registered relay' | tail -1" | scrub | head -c 300)" || line=""
      log "registered and running: ${line}"
      return 0
    fi
    if ! ssh_run "$host" "sudo docker ps --format '{{.Names}}' | grep -qx ${CONTAINER}"; then
      rollback_hint "$host"
      die "${host}: container is not running; check: docker logs ${CONTAINER}"
    fi
    i=$((i + 1)); sleep 2
  done
  rollback_hint "$host"
  die "${host}: no registration within 60s; check: docker logs ${CONTAINER}"
}

# --- commands ---------------------------------------------------------------

convert_host() {
  local host="$1" live public_host label
  echo "==> convert ${host}"
  live="$(detect_container "$host")"
  [ -n "$live" ] || die "${host}: no relay container found; provision it first (lightsail-up.sh or 'create')"

  # Reuse the identity the bootstrap helper already baked in, so a convert does
  # not silently relabel or re-home an existing relay. Both values came from the
  # host: validate before they are reused or displayed.
  public_host="$(container_env_value "$host" "$live" OPENRUNG_PUBLIC_HOST)"
  label="$(container_env_value "$host" "$live" OPENRUNG_LABEL)"
  [ -n "$public_host" ] || die "${host}: could not read OPENRUNG_PUBLIC_HOST from ${live}"
  [ -n "$label" ] || die "${host}: could not read OPENRUNG_LABEL from ${live}"
  assert_matches "$public_host" "$HOST_RE" "OPENRUNG_PUBLIC_HOST"
  assert_matches "$label" "$NAME_RE" "OPENRUNG_LABEL"

  log "identity: label=${label} public_host=${public_host}"
  install_env_file "$host" "$public_host" "$label"
  log "wrote ${ENV_FILE} (root:root 0600)"
  recreate_container "$host" "$ENV_FILE" "$live"
  verify_foundation "$host"
  cleanup_legacy_env "$host"
}

cmd_convert() {
  [ "$#" -ge 1 ] || usage
  require_https_broker
  validate_config
  TOKEN="$(read_token)"
  local host
  for host in "$@"; do
    assert_matches "$host" "$HOST_RE" "host"
    pin_host_key "$host"
    convert_host "$host"
  done
}

cmd_update() {
  [ "$#" -ge 1 ] || usage
  validate_config
  # No token needed: the credentials already on each host are reused as-is.
  # Fail-fast across the fleet: a broken image stops the roll at the first host
  # instead of taking every relay down.
  local host live env_file
  for host in "$@"; do
    assert_matches "$host" "$HOST_RE" "host"
    echo "==> update ${host}"
    pin_host_key "$host"
    env_file="$(detect_env_file "$host")"
    [ -n "$env_file" ] || die "${host}: no env file (${ENV_FILE} or ${LEGACY_ENV_FILE}); run 'convert' first"
    require_env_has_token "$host" "$env_file"
    live="$(detect_container "$host")"
    log "reusing ${env_file}; image ${IMAGE}"
    recreate_container "$host" "$env_file" "$live"
    verify_foundation "$host"
  done
}

cmd_create() {
  require_https_broker
  validate_config
  TOKEN="$(read_token)"   # fail before provisioning, not after

  # lightsail-up.sh refuses to run with token variables set (its own user-data
  # guard), and stripping them here makes the guarantee structural: the
  # provisioning child never holds the credential, so no bug in it could ever
  # embed the token in user-data.
  local out ip name
  out="$(env -u OPENRUNG_FOUNDATION_TOKEN -u OPENRUNG_FOUNDATION_TOKEN_CMD \
             -u OPENRUNG_VOLUNTEER_TOKEN -u OPENRUNG_NODE_CLASS \
             "${HERE}/lightsail-up.sh" "$@" | tee /dev/stderr)"
  # lightsail-up.sh's machine-readable handoff line.
  name="$(printf '%s\n' "$out" | sed -n 's/^OPENRUNG_RELAY name=\([^ ]*\) ip=.*$/\1/p' | head -1)"
  ip="$(printf '%s\n' "$out" | sed -n 's/^OPENRUNG_RELAY .*ip=\(.*\)$/\1/p' | head -1)"
  [ -n "$ip" ] && [ -n "$name" ] || die "could not parse the 'OPENRUNG_RELAY name=... ip=...' line from lightsail-up.sh"
  assert_matches "$name" "$NAME_RE" "instance name"
  assert_matches "$ip" "$HOST_RE" "instance ip"

  # Pin before the first connection that will carry the token. Host keys are
  # published shortly after boot; retry while the instance settles.
  pin_host_key "$ip" "$name" 6

  echo "==> waiting for ${name} (${ip}) to finish bootstrapping"
  local i=0
  until ssh_run "$ip" "sudo docker inspect ${CONTAINER} >/dev/null 2>&1" 2>/dev/null; do
    i=$((i + 1))
    [ "$i" -lt 60 ] || die "${ip}: relay container did not appear within 5 minutes; check /var/log/openrung-init.log"
    sleep 5
  done
  convert_host "$ip"
}

[ "$#" -ge 1 ] || usage
subcommand="$1"; shift
case "$subcommand" in
  create)  cmd_create "$@" ;;
  convert) cmd_convert "$@" ;;
  update)  cmd_update "$@" ;;
  *)       usage ;;
esac
