#!/usr/bin/env bash
#
# Provision, convert, and update OpenRung Foundation-operated relays.
#
#   deploy/relay/foundation-up.sh create [name]           # new Lightsail host, then install credentials
#   deploy/relay/foundation-up.sh convert <host>...       # install credentials on an existing host
#   deploy/relay/foundation-up.sh update  <host>...       # pull the image and recreate, reusing credentials
#   deploy/relay/foundation-up.sh trust <region> <name>   # pin a Lightsail host key from the AWS API
#
# This wraps — and never replaces — lightsail-up.sh. That helper deliberately
# refuses registration tokens because Lightsail retains user-data, so a Foundation
# relay has always needed a second, manual step: install a root-owned mode-0600 env
# file over SSH and recreate the container with --env-file. This script automates
# exactly that step, preserving the invariant rather than weakening it: the token
# reaches the host only over SSH, after boot, and never through user-data.
#
# Overridable via env: OPENRUNG_IMAGE, OPENRUNG_BROKER_URL, OPENRUNG_ENV_FILE,
# OPENRUNG_SSH_KEY, OPENRUNG_SSH_USER, OPENRUNG_VERIFY_BROKER_URL. `create` also
# forwards OPENRUNG_REGION, OPENRUNG_BUNDLE, and lightsail-up.sh's other knobs.
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
# Read-only endpoint used to confirm the broker's node_class attestation. The
# plaintext origin is fine here: this reads a signed public directory and carries
# no credential.
VERIFY_BROKER_URL="${OPENRUNG_VERIFY_BROKER_URL:-http://54.238.185.205:8080}"

CONTAINER="openrung-relay"
LEGACY_CONTAINER="openrung-volunteer"      # pre-rename fleets still run this name
LEGACY_ENV_FILE="/etc/openrung/volunteer.env"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# StrictHostKeyChecking=yes, never accept-new: this connection carries the
# fleet-wide Foundation token, and trust-on-first-use would hand that credential
# to whichever host key answers first. An unknown host is a hard failure; use the
# `trust` subcommand to pin the key from the authenticated AWS API first.
SSH_OPTS=(-o StrictHostKeyChecking=yes -o ConnectTimeout=10 -o BatchMode=yes -i "$SSH_KEY")

# Env keys this script owns. Everything else already on the host is preserved.
CONTROLLED_KEYS='OPENRUNG_BROKER_URL|OPENRUNG_FOUNDATION_TOKEN'
# Dropped on convert: the token alone forces Foundation class, and a stale
# node-class or a volunteer bearer alongside it is a startup error.
DROPPED_KEYS='OPENRUNG_NODE_CLASS|OPENRUNG_VOLUNTEER_TOKEN'

die() { echo "error: $*" >&2; exit 1; }
log() { echo "  $*"; }
warn() { echo "  warning: $*" >&2; }

usage() {
  sed -n '3,9p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
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

# Fail before the token is transmitted if the host key is not already trusted.
require_known_host() {
  local host="$1"
  ssh-keygen -F "$host" >/dev/null 2>&1 \
    || die "${host}: host key is not in known_hosts. This connection carries the Foundation token, so it will not trust an unverified key. Pin it from the AWS API first:
    ${0##*/} trust <region> <instance-name>
  or verify the key out-of-band and add it to known_hosts yourself."
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

# True when the env file holds a non-empty Foundation token.
env_file_has_token() {
  ssh_run "$1" "sudo grep -qE '^OPENRUNG_FOUNDATION_TOKEN=.+' '$2'"
}

# Pin a Lightsail host key using AWS as the out-of-band channel. AWS witnesses the
# host keys from the instance's own console output at first boot, so a fingerprint
# from the authenticated API is trustworthy in a way trust-on-first-use is not.
cmd_trust() {
  [ "$#" -eq 2 ] || usage
  local region="$1" name="$2" ip expected scanned fp line tmp
  ip="$(aws lightsail get-instance --instance-name "$name" --region "$region" \
        --query 'instance.publicIpAddress' --output text 2>/dev/null)" \
    || die "could not look up ${name} in ${region}"
  [ -n "$ip" ] && [ "$ip" != "None" ] || die "${name} in ${region} has no public IP"

  expected="$(aws lightsail get-instance-access-details --instance-name "$name" --region "$region" \
              --protocol ssh --query 'accessDetails.hostKeys[].fingerprintSHA256' --output text)" \
    || die "could not fetch host keys for ${name} from the Lightsail API"
  [ -n "$expected" ] || die "Lightsail returned no host keys for ${name}"

  echo "==> trust ${name} (${ip})"
  scanned="$(ssh-keyscan -T 15 "$ip" 2>/dev/null)" || true
  [ -n "$scanned" ] || die "${ip}: ssh-keyscan returned nothing (is the host up and 22 reachable?)"

  tmp="$(mktemp)"; trap 'rm -f "$tmp"' RETURN
  local matched=0
  # Drop any existing entries first, then pin every key that AWS witnessed. Pinning
  # all of them (not just the first) means SSH cannot negotiate a host key
  # algorithm we left unpinned and fail against StrictHostKeyChecking=yes.
  ssh-keygen -R "$ip" >/dev/null 2>&1 || true
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    printf '%s\n' "$line" > "$tmp"
    fp="$(ssh-keygen -lf "$tmp" 2>/dev/null | awk '{print $2}')" || continue
    # Compare against the AWS-witnessed set (tab-separated).
    if printf '%s' "$expected" | tr '\t' '\n' | grep -qxF "$fp"; then
      printf '%s\n' "$line" >> "${HOME}/.ssh/known_hosts"
      log "pinned $(printf '%s' "$line" | awk '{print $2}') ${fp}"
      matched=$((matched + 1))
    fi
  done <<< "$scanned"

  [ "$matched" -gt 0 ] \
    || die "${ip}: NO presented host key matched the AWS-witnessed fingerprints. Do not connect — this may be a machine-in-the-middle. Expected one of:
$(printf '%s' "$expected" | tr '\t' '\n' | sed 's/^/    /')"
  log "known_hosts updated; ${ip} is now safe to send credentials to"
}

# Merge: keep every key already configured on the host, drop the ones a Foundation
# token makes invalid, and set the ones this script owns. Written atomically via a
# temp file so a failure cannot leave a half-written credential file behind.
#
# The token arrives on stdin so it never appears in argv on either side.
install_env_file() {
  local host="$1" token="$2" target="$3" seed="$4"
  {
    printf '%s\n' "$seed" | grep -vE "^(${CONTROLLED_KEYS}|${DROPPED_KEYS})=" || true
    printf 'OPENRUNG_BROKER_URL=%s\n' "$BROKER_URL"
    printf 'OPENRUNG_FOUNDATION_TOKEN=%s\n' "$token"
  } | ssh "${SSH_OPTS[@]}" "${SSH_USER}@${host}" \
      "sudo install -d -m 700 -o root -g root /etc/openrung \
       && sudo sh -c 'umask 077 && cat > ${target}.tmp' \
       && sudo chown root:root ${target}.tmp && sudo chmod 600 ${target}.tmp \
       && sudo mv -f ${target}.tmp ${target}"
}

# Foundation requires direct mode; auto and tunnel would route the bearer through
# the hub. Fail loudly rather than silently stripping settings the operator chose.
assert_no_conflicting_mode() {
  local host="$1" seed="$2" bad
  bad="$(printf '%s\n' "$seed" | grep -E '^(OPENRUNG_MODE|OPENRUNG_TUNNEL|OPENRUNG_HUB_ADDR)=' || true)"
  [ -z "$bad" ] || die "${host}: existing config conflicts with a Foundation relay (which forces direct mode):
$(printf '%s' "$bad" | sed 's/^/    /')
  A Foundation relay must not use the hub path. Remove these before converting."
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

# Ask the broker what class it actually attested for this relay.
#   foundation | volunteer | <other> : the attested class
#   unknown                          : relay absent from the response
#
# Both the API list and the mirror cap at 20 relays, so a large fleet can push a
# relay out of the response. That is reported as unknown rather than guessed at.
broker_attested_class() {
  local public_host="$1"
  curl -fsS --max-time 10 "${VERIFY_BROKER_URL}/api/v1/relays.mirror" 2>/dev/null \
    | python3 -c "
import json,sys
try:
    d = json.load(sys.stdin)
except Exception:
    print('unknown'); sys.exit()
for r in d.get('relays', []):
    if r.get('public_host') == '$public_host':
        print(r.get('node_class') or 'unknown'); sys.exit()
print('unknown')
" 2>/dev/null || echo unknown
}

# Confirm the relay came back as Foundation.
#
# Two independent signals, because neither alone is sufficient:
#   1. The relay's own guard. With a token present the binary claims Foundation,
#      and it exits if the broker attests anything else. So token-present plus a
#      running, registered container rules out a silent volunteer downgrade. The
#      token check is enforced before recreate; the generic "registered relay" log
#      means nothing without it.
#   2. The broker's attestation, read back from the signed directory. Authoritative
#      when the relay appears; a mismatch is a hard failure.
verify_foundation() {
  local host="$1" public_host="$2" i=0 class
  while [ "$i" -lt 15 ]; do
    if ssh_run "$host" "sudo docker logs ${CONTAINER} 2>&1 | grep -q 'registered relay'"; then
      ssh_run "$host" "sudo docker ps --format '{{.Names}}' | grep -qx ${CONTAINER}" \
        || die "${host}: container exited after registering; check: docker logs ${CONTAINER}"

      class="$(broker_attested_class "$public_host")"
      case "$class" in
        foundation) log "broker attested node_class=foundation for ${public_host}"; return 0 ;;
        unknown)    warn "${host}: broker did not list ${public_host} (the directory caps at 20 relays), so its attestation could not be read back. The relay is running and registered with a Foundation token, which its own startup guard requires the broker to have attested — but this run did not confirm it independently."
                    return 0 ;;
        *)          die "${host}: broker attested node_class='${class}', not 'foundation'. The relay is NOT Foundation-operated. Roll back: docker rm -f ${CONTAINER} && docker rename ${CONTAINER}-old ${CONTAINER} && docker start ${CONTAINER}" ;;
      esac
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
    require_known_host "$host"

    local live existing target seed public_host
    live="$(detect_container "$host")"
    [ -n "$live" ] || die "${host}: no relay container found; provision it first (lightsail-up.sh or 'create')"

    # Overwrite an existing env file in place; only fall back to the canonical
    # path when the host has none, so we never leave two token files behind.
    existing="$(detect_env_file "$host")"
    target="${existing:-$ENV_FILE}"

    # Seed from the env file when there is one, else from the running container.
    # Either way every setting already in force is carried over — stable identity
    # (client id, Reality keys, short id), capacity limits, and camouflage would
    # otherwise be silently regenerated, breaking clients on cached descriptors.
    if [ -n "$existing" ]; then
      seed="$(ssh_run "$host" "sudo cat '${existing}'")"
    else
      seed="$(ssh_run "$host" "sudo docker inspect ${live} --format '{{range .Config.Env}}{{println .}}{{end}}'" | grep -E '^OPENRUNG_' || true)"
    fi
    [ -n "$seed" ] || die "${host}: could not read any existing OPENRUNG_* configuration to preserve"
    assert_no_conflicting_mode "$host" "$seed"

    public_host="$(printf '%s\n' "$seed" | sed -n 's/^OPENRUNG_PUBLIC_HOST=//p' | head -1)"
    [ -n "$public_host" ] || die "${host}: could not determine OPENRUNG_PUBLIC_HOST"

    log "preserving $(printf '%s\n' "$seed" | grep -cE '^OPENRUNG_' || true) existing setting(s); public_host=${public_host}"
    install_env_file "$host" "$token" "$target" "$seed"
    log "wrote ${target} (root:root 0600, atomic)"
    recreate_container "$host" "$target" "$live"
    verify_foundation "$host" "$public_host"
  done
}

cmd_update() {
  [ "$#" -ge 1 ] || usage
  # Fail-fast across the fleet: a broken image stops the roll at the first host
  # instead of taking every relay down.
  for host in "$@"; do
    echo "==> update ${host}"
    require_known_host "$host"

    local live env_file public_host
    env_file="$(detect_env_file "$host")"
    [ -n "$env_file" ] || die "${host}: no env file (${ENV_FILE} or ${LEGACY_ENV_FILE}); run 'convert' first"

    # An env file alone proves nothing. Without a token the relay legitimately
    # registers as volunteer and logs the same line a Foundation relay does, so
    # skipping this check would let the roll certify a volunteer as Foundation.
    env_file_has_token "$host" "$env_file" \
      || die "${host}: ${env_file} has no OPENRUNG_FOUNDATION_TOKEN. This host would come back volunteer-class, not Foundation. Run 'convert' instead."

    public_host="$(ssh_run "$host" "sudo sed -n 's/^OPENRUNG_PUBLIC_HOST=//p' '${env_file}' | head -1")"
    [ -n "$public_host" ] || die "${host}: could not read OPENRUNG_PUBLIC_HOST from ${env_file}"

    live="$(detect_container "$host")"
    log "reusing ${env_file}; image ${IMAGE}"
    recreate_container "$host" "$env_file" "$live"
    verify_foundation "$host" "$public_host"
  done
}

cmd_create() {
  require_https_broker
  read_token >/dev/null   # fail before provisioning, not after

  local out ip name region="${OPENRUNG_REGION:-ap-northeast-1}"
  # lightsail-up.sh rejects credential variables outright (user-data persists), so
  # they must be stripped from its environment or it exits 2 before provisioning.
  # The token is read above and delivered later, over SSH.
  out="$(env -u OPENRUNG_FOUNDATION_TOKEN -u OPENRUNG_FOUNDATION_TOKEN_CMD \
             -u OPENRUNG_VOLUNTEER_TOKEN -u OPENRUNG_NODE_CLASS \
             "${HERE}/lightsail-up.sh" "$@")"
  echo "$out"
  # lightsail-up.sh's machine-readable handoff line.
  name="$(printf '%s\n' "$out" | sed -n 's/^OPENRUNG_RELAY name=\([^ ]*\) ip=.*$/\1/p' | head -1)"
  ip="$(printf '%s\n' "$out" | sed -n 's/^OPENRUNG_RELAY .*ip=\(.*\)$/\1/p' | head -1)"
  [ -n "$ip" ] && [ -n "$name" ] || die "could not parse the 'OPENRUNG_RELAY name=... ip=...' line from lightsail-up.sh"

  echo "==> waiting for ${name} (${ip}) to finish bootstrapping"
  local i=0
  until ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10 -o BatchMode=yes -i "$SSH_KEY" \
          "${SSH_USER}@${ip}" "sudo docker inspect ${CONTAINER} >/dev/null 2>&1" 2>/dev/null; do
    i=$((i + 1))
    [ "$i" -lt 60 ] || die "${ip}: relay container did not appear within 5 minutes; check /var/log/openrung-init.log"
    sleep 5
  done

  # Pin the key from the AWS API before convert sends the token. The poll above
  # deliberately does not trust the key for anything but an existence check.
  ssh-keygen -R "$ip" >/dev/null 2>&1 || true
  cmd_trust "$region" "$name"
  cmd_convert "$ip"
}

[ "$#" -ge 1 ] || usage
subcommand="$1"; shift
case "$subcommand" in
  create)  cmd_create "$@" ;;
  convert) cmd_convert "$@" ;;
  update)  cmd_update "$@" ;;
  trust)   cmd_trust "$@" ;;
  *)       usage ;;
esac
