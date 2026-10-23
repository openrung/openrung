#!/usr/bin/env bash
#
# Provision, convert, and update OpenRung Foundation-operated relays.
#
#   deploy/relay/foundation-up.sh create [name]        # new Lightsail host, then install credentials
#   deploy/relay/foundation-up.sh convert <host>...    # install credentials on an existing host
#   deploy/relay/foundation-up.sh update  <host>...    # pull the image and recreate, reusing credentials
#   deploy/relay/foundation-up.sh rollback <host>...   # restore an exact interrupted roll
#   deploy/relay/foundation-up.sh audit   <host>...    # read-only: relay version, egress limit, access address
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
# OPENRUNG_SSH_KEY, OPENRUNG_SSH_USER, OPENRUNG_REGION (host-key pinning lookups),
# OPENRUNG_ALLOW_TOFU=1 (explicitly accept an unverifiable first-contact host key),
# OPENRUNG_NEW_DEST_RATE (the host new-destination limit `convert` and `update`
# load from the relay image: new destinations per second, default 10, or off),
# OPENRUNG_FOUNDATION_CONTACT (the operator contact `convert` and `create` write
# as OPENRUNG_OPERATOR_CONTACT, sent privately at registration; default empty,
# which keeps whatever contact the host already has),
# OPENRUNG_ACCESS_HOST (an IPv6 address, distinct from the relay's primary
# address and its public host, that `convert` and `create` configure on the host
# through the openrung-access-address unit and write into the env file, so the
# relay advertises it privately at registration; `off` removes both; default
# empty, which keeps whatever the host already has).
# `create` also forwards OPENRUNG_BUNDLE and the rest of lightsail-up.sh's knobs.
set -euo pipefail

# A credential transits this script's variables: keep xtrace off even under
# `bash -x`, so the token can never appear in trace output.
{ set +x; } 2>/dev/null

# Keep caller-supplied token sources in this shell, but remove their export
# attribute before the first command substitution or child process. This lets
# convert inspect every host before invoking a secret-manager command without
# leaking a direct token (or a token-bearing command) to ssh, aws, dirname, or
# any other child.
export -n OPENRUNG_FOUNDATION_TOKEN OPENRUNG_FOUNDATION_TOKEN_CMD 2>/dev/null || true

# TOKEN is a generic name the caller may already have exported; recreate it as
# an unexported shell variable for the same reason.
unset TOKEN
TOKEN=""   # set once per run; never leaves this process except over SSH stdin

IMAGE="${OPENRUNG_IMAGE:-ghcr.io/openrung/openrung-relay:main}"
BROKER_URL="${OPENRUNG_BROKER_URL:-https://broker-origin.openrung.org}"
ENV_FILE="${OPENRUNG_ENV_FILE:-/etc/openrung/relay.env}"
SSH_KEY="${OPENRUNG_SSH_KEY:-$HOME/.ssh/id_ed25519_openrung}"
SSH_USER="${OPENRUNG_SSH_USER:-ubuntu}"
REGION="${OPENRUNG_REGION:-ap-northeast-1}"
NEW_DEST_RATE="${OPENRUNG_NEW_DEST_RATE:-10}"
FOUNDATION_CONTACT="${OPENRUNG_FOUNDATION_CONTACT:-}"
ACCESS_HOST="${OPENRUNG_ACCESS_HOST:-}"

CONTAINER="openrung-relay"
OLD_CONTAINER="${CONTAINER}-old"
NEW_CONTAINER="${CONTAINER}-new"
LEGACY_CONTAINER="openrung-volunteer"
LEGACY_OLD_CONTAINER="${LEGACY_CONTAINER}-old"
LEGACY_ENV_FILE="/etc/openrung/volunteer.env"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# accept-new never accepts a CHANGED key, and pin_host_key below guarantees a
# key is already known (API-pinned, operator-added, or explicitly TOFU'd via
# OPENRUNG_ALLOW_TOFU=1) before any mode connects — no key is ever learned
# implicitly. UserKnownHostsFile pins ssh to the same file pin_host_key writes,
# so the check and the connection cannot diverge on a per-host ssh_config.
SSH_OPTS=(-o StrictHostKeyChecking=accept-new -o "UserKnownHostsFile=$HOME/.ssh/known_hosts" -o ConnectTimeout=10 -o BatchMode=yes -i "$SSH_KEY")

die()  { echo "error: $*" >&2; exit 1; }
log()  { echo "  $*"; }
warn() { echo "  warning: $*" >&2; }

usage() {
  cat >&2 <<'EOF'
usage:
  deploy/relay/foundation-up.sh create [name]        # new Lightsail host, then install credentials
  deploy/relay/foundation-up.sh convert <host>...    # install credentials on an existing host
  deploy/relay/foundation-up.sh update  <host>...    # pull the image and recreate, reusing credentials
  deploy/relay/foundation-up.sh rollback <host>...   # restore an exact interrupted roll
  deploy/relay/foundation-up.sh audit   <host>...    # read-only: relay version, egress limit, access address

The Foundation token is read from OPENRUNG_FOUNDATION_TOKEN_CMD (preferred) or
OPENRUNG_FOUNDATION_TOKEN — never from argv. `update` and `rollback` need no token.

OPENRUNG_ACCESS_HOST=<IPv6 address> on `convert`/`create` adds that address to
the host interface (deprecated, so the relay's own traffic keeps the primary
address) through the openrung-access-address unit and has the relay advertise
it privately at registration; OPENRUNG_ACCESS_HOST=off removes it. Unset keeps
whatever the host has.

Automation accepts one steady state only: a running `openrung-relay`, with no
legacy container/env, `-old` backup, `-new` candidate, or staged `.tmp` file.
Unsupported states are inspected but never changed; the script prints manual
migration or recovery instructions instead.
EOF
  exit 2
}

# Remote stderr reaches the operator's terminal for diagnostics, but a
# compromised host must not be able to write terminal escapes through it —
# strip everything but printable characters, newlines, and tabs.
scrub_stream() { tr -cd '[:print:]\n\t'; }
scrub()        { tr -cd '[:print:]'; }

ssh_run() {
  local host="$1"; shift
  ssh "${SSH_OPTS[@]}" "${SSH_USER}@${host}" "$@" 2> >(scrub_stream >&2)
}

# --- input validation -------------------------------------------------------
#
# Everything read back from a host (container names, env-file paths, env values,
# log lines, stderr) is attacker-controlled if that host is compromised. Nothing
# remote is interpolated into another command until it has passed one of these
# allowlists, and everything remote that reaches the terminal — captured stdout
# and passed-through stderr alike — is stripped of control characters first.

NAME_RE='^[A-Za-z0-9][A-Za-z0-9._-]*$'      # instance names, labels, container names
HOST_RE='^[A-Za-z0-9][A-Za-z0-9.:-]*$'      # IPv4, IPv6, DNS names
CONTACT_RE='^[A-Za-z0-9._%+-]+@[A-Za-z0-9-][A-Za-z0-9.-]*\.[A-Za-z0-9-]+$'   # operator contact (email)
ACCESS_HOST_RE='^[0-9A-Fa-f]{0,4}(:[0-9A-Fa-f]{0,4}){2,7}$'   # IPv6 literal shape; the relay validates it fully
DEV_RE='^[A-Za-z0-9._-]{1,15}$'             # network interface names

assert_matches() { # value regex what
  local val="$1" re="$2" what="$3"
  [[ "$val" =~ $re ]] || die "unexpected ${what}: '$(printf '%s' "$val" | scrub | head -c 100)'"
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
  assert_matches "$NEW_DEST_RATE" '^([1-9][0-9]*|off)$' "OPENRUNG_NEW_DEST_RATE"
  if [ -n "$FOUNDATION_CONTACT" ]; then
    assert_matches "$FOUNDATION_CONTACT" "$CONTACT_RE" "OPENRUNG_FOUNDATION_CONTACT"
    [ "${#FOUNDATION_CONTACT}" -le 128 ] || die "OPENRUNG_FOUNDATION_CONTACT must be at most 128 characters"
  fi
  if [ -n "$ACCESS_HOST" ] && [ "$ACCESS_HOST" != off ]; then
    assert_matches "$ACCESS_HOST" "$ACCESS_HOST_RE" "OPENRUNG_ACCESS_HOST (want a bare IPv6 address, or off)"
  fi
  [ "$ENV_FILE" != "$LEGACY_ENV_FILE" ] \
    || die "OPENRUNG_ENV_FILE may not use the legacy path ${LEGACY_ENV_FILE}; migrate it to a canonical path first"
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

# Capture the token for this run, then drop the source variables so children
# (ssh, aws, ssh-keygen) never inherit the credential — the same rationale as
# create's env -u for lightsail-up.sh.
load_token() {
  TOKEN="$(read_token)" || return
  unset OPENRUNG_FOUNDATION_TOKEN OPENRUNG_FOUNDATION_TOKEN_CMD
}

# --- SSH host identity ------------------------------------------------------

# Verify first contact out-of-band: Lightsail publishes each instance's SSH host
# keys over the (TLS, SigV4-authenticated) AWS API, so pin them into known_hosts
# before the first connection. When the key can be neither found locally nor
# pinned, EVERY mode refuses to connect — a tokenless mode that silently
# accepted a first-seen key would launder it into "known" for a later
# token-carrying convert. OPENRUNG_ALLOW_TOFU=1 is the explicit, per-run opt-in
# (e.g. for a non-Lightsail host whose key was checked another way).
pin_host_key() { # host [instance-name] [attempts]
  local host="$1" name="${2:-}" attempts="${3:-1}" keys="" i=0
  ssh-keygen -F "$host" -f "$HOME/.ssh/known_hosts" >/dev/null 2>&1 && return 0

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
    if [ "${OPENRUNG_ALLOW_TOFU:-0}" = 1 ]; then
      warn "${host}: no Lightsail-published host key; first contact is trust-on-first-use (OPENRUNG_ALLOW_TOFU=1)"
      return 0
    fi
    die "${host}: cannot verify the SSH host key out-of-band (no Lightsail-published key: non-Lightsail host, or not in ${REGION}; IPv6/DNS targets are looked up by IPv4 address only). No mode contacts an unverified host implicitly. Verify the key via your provider's console, add it to ~/.ssh/known_hosts, and re-run ('convert ${host}' if the instance is already provisioned) — or set OPENRUNG_ALLOW_TOFU=1 to accept the first-seen key explicitly."
  fi

  mkdir -p "$HOME/.ssh" \
    || die "${host}: could not create $HOME/.ssh; host key was not pinned"
  if [ ! -f "$HOME/.ssh/known_hosts" ]; then
    touch "$HOME/.ssh/known_hosts" \
      || die "${host}: could not create $HOME/.ssh/known_hosts; host key was not pinned"
    chmod 600 "$HOME/.ssh/known_hosts" \
      || die "${host}: could not protect $HOME/.ssh/known_hosts; host key was not pinned"
  fi
  local algo key added=0
  while read -r algo key; do
    [ -n "$algo" ] && [ -n "$key" ] || continue
    assert_matches "$algo" '^[A-Za-z0-9@.-]+$' "host-key algorithm"
    assert_matches "$key" '^[A-Za-z0-9+/=]+$' "host-key material"
    printf '%s %s %s\n' "$host" "$algo" "$key" >> "$HOME/.ssh/known_hosts" \
      || die "${host}: could not write $HOME/.ssh/known_hosts; host key was not pinned"
    added=$((added + 1))
  done <<< "$keys"
  [ "$added" -gt 0 ] \
    || die "${host}: Lightsail returned no valid SSH host keys; host key was not pinned"
  log "pinned ${host}'s SSH host key from the Lightsail API"
}

# --- host inspection --------------------------------------------------------

# One read-only snapshot replaces the old preference-based detection, which
# could hide a legacy container behind a canonical one. The eleven fields are:
#   live live_running old old_running new legacy legacy_old env legacy_env tmp unexpected
# Every field is an allowlisted bit before it influences a decision.
inspect_host_state() {
  local host="$1" state
  state="$(ssh_run "$host" "
    live=0; live_running=0; old=0; old_running=0; new=0
    legacy=0; legacy_old=0; env=0; legacy_env=0; tmp=0; unexpected=0
    names=\"\$(sudo docker ps -a --format '{{.Names}}')\" || exit 1
    for c in \$names; do
      case \"\$c\" in
        ${CONTAINER}) live=1 ;;
        ${OLD_CONTAINER}) old=1 ;;
        ${NEW_CONTAINER}) new=1 ;;
        ${LEGACY_CONTAINER}) legacy=1 ;;
        ${LEGACY_OLD_CONTAINER}) legacy_old=1 ;;
        ${CONTAINER}-*|${CONTAINER}_*|${CONTAINER}.*|${LEGACY_CONTAINER}-*|${LEGACY_CONTAINER}_*|${LEGACY_CONTAINER}.*) unexpected=1 ;;
      esac
    done
    if [ \"\$live\" = 1 ]; then
      [ \"\$(sudo docker inspect -f '{{.State.Running}}' ${CONTAINER} 2>/dev/null)\" = true ] && live_running=1
    fi
    if [ \"\$old\" = 1 ]; then
      [ \"\$(sudo docker inspect -f '{{.State.Running}}' ${OLD_CONTAINER} 2>/dev/null)\" = true ] && old_running=1
    fi
    sudo test -f ${ENV_FILE} && env=1
    if sudo test -e ${LEGACY_ENV_FILE} || sudo test -L ${LEGACY_ENV_FILE}; then legacy_env=1; fi
    if sudo test -e ${ENV_FILE}.tmp || sudo test -L ${ENV_FILE}.tmp; then tmp=1; fi
    printf '%s %s %s %s %s %s %s %s %s %s %s\\n' \
      \"\$live\" \"\$live_running\" \"\$old\" \"\$old_running\" \"\$new\" \
      \"\$legacy\" \"\$legacy_old\" \"\$env\" \"\$legacy_env\" \"\$tmp\" \"\$unexpected\"
  ")" || die "${host}: could not inspect relay state; no remote changes were made"
  [[ "$state" =~ ^[01](\ [01]){10}$ ]] \
    || die "${host}: host returned an invalid state snapshot: '$(printf '%s' "$state" | scrub | head -c 100)'"
  printf '%s' "$state"
}

print_ssh_command() {
  local host="$1" arg
  printf '  connect:' >&2
  for arg in ssh "${SSH_OPTS[@]}" "${SSH_USER}@${host}"; do
    printf ' %q' "$arg" >&2
  done
  printf '\n' >&2
}

print_manual_state_help() { # host reason
  local host="$1" reason="$2"
  echo "error: ${host}: ${reason}" >&2
  echo "  no relay container, env-file, image, backup, or candidate changes were made on this host" >&2
  print_ssh_command "$host"
  cat >&2 <<EOF
  inspect:
    sudo docker ps -a --filter name=openrung-relay --filter name=openrung-volunteer
    sudo ls -l ${ENV_FILE} ${ENV_FILE}.tmp ${LEGACY_ENV_FILE} 2>/dev/null

  automation requires exactly one running ${CONTAINER}, plus no ${OLD_CONTAINER},
  ${NEW_CONTAINER}, ${LEGACY_CONTAINER}, ${LEGACY_OLD_CONTAINER}, legacy env, .tmp,
  or unrecognized ${CONTAINER}-* / ${LEGACY_CONTAINER}-* container.

  manual legacy migration (only after identifying the healthy container/config):
    sudo docker stop ${LEGACY_CONTAINER}
    sudo docker rename ${LEGACY_CONTAINER} ${CONTAINER}
    sudo docker start ${CONTAINER}
    sudo install -m 0600 ${LEGACY_ENV_FILE} ${ENV_FILE}   # only if the legacy file is authoritative
    # remove ${LEGACY_ENV_FILE} only after the canonical relay is verified

  interrupted canonical rolls (${OLD_CONTAINER} with optional ${NEW_CONTAINER},
  and no live/legacy artifacts) may be restored with:
    deploy/relay/foundation-up.sh rollback ${host}
EOF
}

parse_state() { # state; populates STATE_* globals for the caller
  read -r STATE_LIVE STATE_LIVE_RUNNING STATE_OLD STATE_OLD_RUNNING STATE_NEW \
    STATE_LEGACY STATE_LEGACY_OLD STATE_ENV STATE_LEGACY_ENV STATE_TMP \
    STATE_UNEXPECTED <<< "$1"
}

# convert/update deliberately accept one entry state only. Any backup, candidate,
# legacy artifact, stopped live container, or staged tmp is an operator decision,
# not something automation guesses how to reconcile.
require_clean_state() { # host mode state
  local host="$1" mode="$2" state="$3"
  parse_state "$state"

  if [ "$STATE_LEGACY" = 1 ] || [ "$STATE_LEGACY_OLD" = 1 ] || [ "$STATE_LEGACY_ENV" = 1 ]; then
    print_manual_state_help "$host" "legacy relay artifacts detected; manual migration required"
    return 1
  fi
  if [ "$STATE_UNEXPECTED" = 1 ]; then
    print_manual_state_help "$host" "unrecognized OpenRung relay container detected; manual migration required"
    return 1
  fi
  if [ "$STATE_OLD" = 1 ] || [ "$STATE_NEW" = 1 ] || [ "$STATE_TMP" = 1 ]; then
    print_manual_state_help "$host" "backup, candidate, or staged env detected; state is not a clean automation entry point"
    return 1
  fi
  if [ "$STATE_LIVE" != 1 ]; then
    print_manual_state_help "$host" "canonical container ${CONTAINER} is absent"
    return 1
  fi
  if [ "$STATE_LIVE_RUNNING" != 1 ]; then
    print_manual_state_help "$host" "canonical container ${CONTAINER} is not running"
    return 1
  fi
  if [ "$STATE_OLD_RUNNING" != 0 ]; then
    print_manual_state_help "$host" "inconsistent backup state returned by host"
    return 1
  fi
  if [ "$mode" = update ] && [ "$STATE_ENV" != 1 ]; then
    print_manual_state_help "$host" "canonical env file ${ENV_FILE} is absent; run convert after reconciling the host"
    return 1
  fi
  return 0
}

# rollback supports only the two exact interruption states this script creates:
# stopped old alone (candidate failed to start), or stopped old + candidate
# (candidate failed verification/commit). Everything else is ambiguous.
require_rollback_state() { # host state
  local host="$1" state="$2"
  parse_state "$state"
  if [ "$STATE_LEGACY" = 1 ] || [ "$STATE_LEGACY_OLD" = 1 ] || [ "$STATE_LEGACY_ENV" = 1 ] || [ "$STATE_TMP" = 1 ]; then
    print_manual_state_help "$host" "rollback refused because legacy or staged artifacts are present"
    return 1
  fi
  if [ "$STATE_UNEXPECTED" = 1 ]; then
    print_manual_state_help "$host" "rollback refused because an unrecognized OpenRung relay container is present"
    return 1
  fi
  if [ "$STATE_LIVE" != 0 ] || [ "$STATE_LIVE_RUNNING" != 0 ] \
     || [ "$STATE_OLD" != 1 ] || [ "$STATE_OLD_RUNNING" != 0 ]; then
    print_manual_state_help "$host" "rollback requires no live container and exactly one stopped ${OLD_CONTAINER}"
    return 1
  fi
  return 0
}

preflight_host() { # host mode
  local host="$1" mode="$2" state
  state="$(inspect_host_state "$host")" || return
  if [ "$mode" = rollback ]; then
    require_rollback_state "$host" "$state" || return
  else
    require_clean_state "$host" "$mode" "$state" || return
    if [ "$mode" = update ]; then require_env_has_token "$host" "$ENV_FILE" || return; fi
  fi
  return 0
}

preflight_fleet() { # mode host...
  local mode="$1" host; shift
  for host in "$@"; do
    assert_matches "$host" "$HOST_RE" "host" || return
    pin_host_key "$host" || return
    preflight_host "$host" "$mode" || return
  done
  return 0
}

# Confirm the env file carries a usable Foundation credential (presence only —
# the value is never read back): at least one token line whose value starts
# with a non-whitespace character, and no empty assignment anywhere. Both
# checks matter: docker's --env-file strips a trailing CR (so 'TOKEN=\r' is
# empty in the container despite looking non-empty to grep), and on duplicate
# keys the LAST occurrence wins (so 'TOKEN=x' followed by 'TOKEN=' is empty
# too). An empty token silently demotes the relay to volunteer class, which
# verify_foundation could not tell apart from success.
require_env_has_token() {
  local host="$1" file="$2"
  ssh_run "$host" "sudo grep -q '^OPENRUNG_FOUNDATION_TOKEN=[^[:space:]]' ${file} \
      && ! sudo grep -q '^OPENRUNG_FOUNDATION_TOKEN=[[:space:]]*\$' ${file}" \
    || die "${host}: ${file} has no usable OPENRUNG_FOUNDATION_TOKEN (missing, empty, or an empty duplicate); not a Foundation relay — run 'convert' first"
}

# --- mutation ---------------------------------------------------------------

# Keys this script owns and (re)writes on convert. Everything else already
# configured on the host — stable identity (OPENRUNG_CLIENT_ID, Reality keys,
# short id), capacity limits, camouflage — is preserved verbatim, so a convert
# or re-convert never rotates a pinned identity or drops tuning. (Bootstrap
# relays carry no pinned identity and regenerate one per restart; that is fleet
# status quo, unchanged by this script.) Dropped deliberately:
#   VOLUNTEER_TOKEN / NODE_CLASS — the foundation token forces the class (a
#     conflicting node_class is a startup error) and a spare credential in the
#     file is pure liability;
#   XRAY_PATH / CONFIG_OUT — image/entrypoint internals that docker inspect
#     reports from the image's own ENV; pinning them in the host env file would
#     permanently override future images that relocate those paths.
# OPERATOR_CONTACT is managed only when OPENRUNG_FOUNDATION_CONTACT is set, and
# ACCESS_HOST only when OPENRUNG_ACCESS_HOST is; otherwise the host's existing
# values are preserved like any other setting.
PRESERVE_DROP_KEYS='BROKER_URL|FOUNDATION_TOKEN|VOLUNTEER_TOKEN|NODE_CLASS|XRAY_PATH|CONFIG_OUT'
preserve_drop() {
  local keys="$PRESERVE_DROP_KEYS"
  [ -z "$FOUNDATION_CONTACT" ] || keys+='|OPERATOR_CONTACT'
  [ -z "$ACCESS_HOST" ] || keys+='|ACCESS_HOST'
  printf '/^OPENRUNG_(%s)=/d' "$keys"
}

# Stage the preserved settings into ENV_FILE.tmp on the host. The only accepted
# sources are the canonical env file, when present, or the canonical running
# container. Legacy files are rejected during preflight and are never merged or
# removed automatically. Only OPENRUNG_* lines are kept. The filter runs on the
# host and the result stays there: no secret and no preserved value ever transits
# the connection or a command line. Fails when no OPENRUNG_PUBLIC_HOST survives,
# since direct mode cannot run without one. Deliberately no
# OPENRUNG_LISTEN_HOST default is added: the binary's own default (::,
# dual-stack) is correct, and pinning 0.0.0.0 here would silently cut IPv6 on
# hosts that relied on the default.
stage_preserved_env() { # host container
  local host="$1" container="$2"
  ssh_run "$host" "sudo sh -c \"set -e; umask 077 \
    && { test -d ${ENV_FILE%/*} || install -d -m 700 -o root -g root ${ENV_FILE%/*}; } \
    && { if test -f ${ENV_FILE}; then cat ${ENV_FILE}; \
         else docker inspect ${container} --format '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null || true; fi; } \
       | sed -n '/^OPENRUNG_[A-Za-z0-9_]*=/p' | sed -E '$(preserve_drop)' > ${ENV_FILE}.tmp \
    && grep -q '^OPENRUNG_PUBLIC_HOST=..*' ${ENV_FILE}.tmp \
    || { rm -f ${ENV_FILE}.tmp; exit 1; }\"" \
    || die "${host}: could not stage the relay's existing settings (is OPENRUNG_PUBLIC_HOST set in its env file or container?)"
}

# Append the managed keys to the staged file and move it into place. The token
# arrives on stdin so it never appears in argv on either side of the connection.
# tmp + rename keeps the canonical path complete-or-previous; the EXIT trap
# removes the token-bearing tmp on orderly failures (a hard connection drop can
# still strand it briefly — the next stage_preserved_env truncates it).
install_env_file() { # host   (managed values from $TOKEN / $BROKER_URL; preserved settings already staged)
  { set +x; } 2>/dev/null
  local host="$1"
  {
    printf 'OPENRUNG_BROKER_URL=%s\n' "$BROKER_URL"
    printf 'OPENRUNG_FOUNDATION_TOKEN=%s\n' "$TOKEN"
    if [ -n "$FOUNDATION_CONTACT" ]; then printf 'OPENRUNG_OPERATOR_CONTACT=%s\n' "$FOUNDATION_CONTACT"; fi
    if [ -n "$ACCESS_HOST" ] && [ "$ACCESS_HOST" != off ]; then printf 'OPENRUNG_ACCESS_HOST=%s\n' "$ACCESS_HOST"; fi
  } | ssh_run "$host" \
      "sudo sh -c 'trap \"rm -f ${ENV_FILE}.tmp\" EXIT; set -e; umask 077; cat >> ${ENV_FILE}.tmp; mv -f ${ENV_FILE}.tmp ${ENV_FILE}'"
}

print_recovery_help() {
  local host="$1" reason="$2"
  echo "error: ${host}: ${reason}" >&2
  echo "  the previous relay remains as stopped ${OLD_CONTAINER}; automation will refuse further convert/update runs until this exact state is resolved" >&2
  print_ssh_command "$host"
  echo "  restore with: deploy/relay/foundation-up.sh rollback ${host}" >&2
}

print_interrupted_state_help() {
  local host="$1" reason="$2"
  echo "error: ${host}: ${reason}" >&2
  echo "  the commit guard made no further container changes; an earlier roll step may already have created ${OLD_CONTAINER} or ${NEW_CONTAINER}" >&2
  print_ssh_command "$host"
  echo "  inspect: sudo docker ps -a --filter name=openrung-relay" >&2
  echo "  use 'deploy/relay/foundation-up.sh rollback ${host}' only if inspection shows no live container and one stopped ${OLD_CONTAINER}, with at most ${NEW_CONTAINER}" >&2
}

# Pull only after an exact clean-state guard. This is the first remote mutation;
# all fleet targets have already passed the read-only preflight before it runs.
prepare_image() {
  local host="$1" rc=0
  ssh_run "$host" "set -e
    names=\"\$(sudo docker ps -a --format '{{.Names}}')\" || exit 42
    live_seen=0
    for c in \$names; do
      case \"\$c\" in
        ${CONTAINER}) live_seen=1 ;;
        ${OLD_CONTAINER}|${NEW_CONTAINER}|${LEGACY_CONTAINER}|${LEGACY_OLD_CONTAINER}|${CONTAINER}-*|${CONTAINER}_*|${CONTAINER}.*|${LEGACY_CONTAINER}-*|${LEGACY_CONTAINER}_*|${LEGACY_CONTAINER}.*) exit 42 ;;
      esac
    done
    [ \"\$live_seen\" = 1 ] || exit 42
    [ \"\$(sudo docker inspect -f '{{.State.Running}}' ${CONTAINER} 2>/dev/null)\" = true ] || exit 42
    ! sudo test -e ${LEGACY_ENV_FILE} && ! sudo test -L ${LEGACY_ENV_FILE} || exit 42
    ! sudo test -e ${ENV_FILE}.tmp && ! sudo test -L ${ENV_FILE}.tmp || exit 42
    sudo docker pull ${IMAGE} >/dev/null" || rc=$?
  if [ "$rc" = 42 ]; then
    print_manual_state_help "$host" "relay state changed after preflight; refusing before image pull"
    return 1
  elif [ "$rc" != 0 ]; then
    die "${host}: image pull failed; the live container and env file were not touched"
  fi
}

# After preparation, recheck that no competing state appeared, then move the
# running live relay to the one exact backup name. Nothing is deleted here.
begin_roll() {
  local host="$1" rc=0
  ssh_run "$host" "set -e
    names=\"\$(sudo docker ps -a --format '{{.Names}}')\" || exit 42
    live_seen=0
    for c in \$names; do
      case \"\$c\" in
        ${CONTAINER}) live_seen=1 ;;
        ${OLD_CONTAINER}|${NEW_CONTAINER}|${LEGACY_CONTAINER}|${LEGACY_OLD_CONTAINER}|${CONTAINER}-*|${CONTAINER}_*|${CONTAINER}.*|${LEGACY_CONTAINER}-*|${LEGACY_CONTAINER}_*|${LEGACY_CONTAINER}.*) exit 42 ;;
      esac
    done
    [ \"\$live_seen\" = 1 ] || exit 42
    [ \"\$(sudo docker inspect -f '{{.State.Running}}' ${CONTAINER} 2>/dev/null)\" = true ] || exit 42
    ! sudo test -e ${LEGACY_ENV_FILE} && ! sudo test -L ${LEGACY_ENV_FILE} || exit 42
    ! sudo test -e ${ENV_FILE}.tmp && ! sudo test -L ${ENV_FILE}.tmp || exit 42
    sudo test -f ${ENV_FILE} || exit 42
    sudo grep -q '^OPENRUNG_FOUNDATION_TOKEN=[^[:space:]]' ${ENV_FILE} || exit 42
    ! sudo grep -q '^OPENRUNG_FOUNDATION_TOKEN=[[:space:]]*\$' ${ENV_FILE} || exit 42
    sudo docker stop ${CONTAINER} >/dev/null
    sudo docker rename ${CONTAINER} ${OLD_CONTAINER}" || rc=$?
  if [ "$rc" = 42 ]; then
    echo "error: ${host}: relay state changed after preparation; no container was stopped or removed" >&2
    print_ssh_command "$host"
    return 1
  elif [ "$rc" != 0 ]; then
    echo "error: ${host}: could not move the live relay to ${OLD_CONTAINER}; inspect whether ${CONTAINER} needs to be restarted" >&2
    print_ssh_command "$host"
    return 1
  fi
}

# Container hardening mirrors lightsail-up.sh exactly. The uncommitted image
# always runs under NEW_CONTAINER; it never occupies the live name until after
# verification succeeds.
run_candidate() { # host env_file
  local host="$1" env_file="$2"
  ssh_run "$host" "sudo docker run -d --name ${NEW_CONTAINER} --restart unless-stopped \
      --network host --cap-drop ALL --cap-add NET_BIND_SERVICE --read-only --tmpfs /tmp \
      --env-file ${env_file} \
      ${IMAGE} >/dev/null" \
    || { print_recovery_help "$host" "candidate container failed to start"; return 1; }
}

# A relay that presents the Foundation token forces node_class=foundation, and it
# exits during startup if the broker attests any other class (cmd/relay refuses a
# mismatched attestation; the broker 403s a foundation claim without the token).
# So a container that logged a registration AND is running cleanly (no restarts —
# a post-registration crash loop keeps the old log line while the relay flaps) IS
# the proof the broker attested Foundation. This holds only because convert
# always writes a validated non-empty token and update asserts one is present.
# An ssh failure during polling is treated as transient — never as evidence the
# container died — so a network blip cannot trigger a false recovery diagnosis.
verify_foundation() { # host container
  local host="$1" container="$2" i=0 line state ps_out
  while [ "$i" -lt 30 ]; do
    if ssh_run "$host" "sudo docker logs ${container} 2>&1 | grep -q 'registered relay'"; then
      if state="$(ssh_run "$host" "sudo docker inspect -f '{{.State.Running}} {{.RestartCount}}' ${container} 2>/dev/null")"; then
        if [ "$state" = "true 0" ]; then
          line="$(ssh_run "$host" "sudo docker logs ${container} 2>&1 | grep 'registered relay' | tail -1" | scrub | head -c 300)" || line=""
          log "registered and running: ${line}"
          return 0
        fi
        print_recovery_help "$host" "candidate registered but is not running cleanly (state: $(printf '%s' "$state" | scrub | head -c 40))"
        return 1
      fi
    elif ps_out="$(ssh_run "$host" "sudo docker ps --format '{{.Names}}'" 2>/dev/null)"; then
      if ! printf '%s\n' "$ps_out" | grep -qx "${container}"; then
        print_recovery_help "$host" "candidate container is not running"
        return 1
      fi
    fi
    i=$((i + 1)); sleep 2
  done
  print_recovery_help "$host" "candidate did not register within 60s"
  return 1
}

# Commit only the verified candidate: rename it to the live name, then remove
# the stopped old container last. A cleanup failure leaves a visible live+old
# state that every later automation run refuses to guess about.
#
# ROLL_PROMOTED tells a caller whose roll failed whether the candidate went
# live: 0 when it certainly did not (the roll failed before the commit, or the
# commit guard refused), 1 when it did (committed, or only the old container's
# removal failed), and "unknown" when the commit was interrupted. Whatever
# the candidate depends on must be kept unless it is 0.
ROLL_PROMOTED=0
commit_candidate() {
  local host="$1" rc=0
  ROLL_PROMOTED=unknown
  ssh_run "$host" "set -e
    names=\"\$(sudo docker ps -a --format '{{.Names}}')\" || exit 42
    old_seen=0; new_seen=0
    for c in \$names; do
      case \"\$c\" in
        ${OLD_CONTAINER}) old_seen=1 ;;
        ${NEW_CONTAINER}) new_seen=1 ;;
        ${CONTAINER}|${LEGACY_CONTAINER}|${LEGACY_OLD_CONTAINER}) exit 42 ;;
        ${CONTAINER}-*|${CONTAINER}_*|${CONTAINER}.*|${LEGACY_CONTAINER}-*|${LEGACY_CONTAINER}_*|${LEGACY_CONTAINER}.*) exit 42 ;;
      esac
    done
    [ \"\$old_seen\" = 1 ] && [ \"\$new_seen\" = 1 ] || exit 42
    [ \"\$(sudo docker inspect -f '{{.State.Running}} {{.RestartCount}}' ${NEW_CONTAINER} 2>/dev/null)\" = 'true 0' ] || exit 42
    sudo docker logs ${NEW_CONTAINER} 2>&1 | grep -q 'registered relay' || exit 42
    [ \"\$(sudo docker inspect -f '{{.State.Running}}' ${OLD_CONTAINER} 2>/dev/null)\" = false ] || exit 42
    ! sudo test -e ${LEGACY_ENV_FILE} && ! sudo test -L ${LEGACY_ENV_FILE} || exit 42
    ! sudo test -e ${ENV_FILE}.tmp && ! sudo test -L ${ENV_FILE}.tmp || exit 42
    sudo docker rename ${NEW_CONTAINER} ${CONTAINER}
    sudo docker rm -f ${OLD_CONTAINER} >/dev/null || exit 43" || rc=$?
  case "$rc" in
    0|43) ROLL_PROMOTED=1 ;;
    42)   ROLL_PROMOTED=0 ;;
  esac
  if [ "$rc" = 42 ]; then
    print_interrupted_state_help "$host" "candidate state changed after verification; refusing to promote or delete the old relay"
    return 1
  elif [ "$rc" = 43 ]; then
    echo "error: ${host}: candidate is live and verified, but ${OLD_CONTAINER} could not be removed; future automation will refuse until you inspect and remove that stale backup" >&2
    print_ssh_command "$host"
    return 1
  elif [ "$rc" != 0 ]; then
    echo "error: ${host}: candidate commit was interrupted; inspect ${CONTAINER}, ${NEW_CONTAINER}, and ${OLD_CONTAINER} before taking action" >&2
    print_ssh_command "$host"
    return 1
  fi
  log "committed verified candidate; removed ${OLD_CONTAINER}"
}

roll_host() { # host env_file
  local host="$1" env_file="$2"
  ROLL_PROMOTED=0
  begin_roll "$host" || return
  run_candidate "$host" "$env_file" || return
  verify_foundation "$host" "$NEW_CONTAINER" || return
  commit_candidate "$host" || return
}

rollback_host() {
  local host="$1" rc=0
  preflight_host "$host" rollback || return
  ssh_run "$host" "set -e
    names=\"\$(sudo docker ps -a --format '{{.Names}}')\" || exit 42
    old_seen=0; new_seen=0
    for c in \$names; do
      case \"\$c\" in
        ${OLD_CONTAINER}) old_seen=1 ;;
        ${NEW_CONTAINER}) new_seen=1 ;;
        ${CONTAINER}|${LEGACY_CONTAINER}|${LEGACY_OLD_CONTAINER}) exit 42 ;;
        ${CONTAINER}-*|${CONTAINER}_*|${CONTAINER}.*|${LEGACY_CONTAINER}-*|${LEGACY_CONTAINER}_*|${LEGACY_CONTAINER}.*) exit 42 ;;
      esac
    done
    [ \"\$old_seen\" = 1 ] || exit 42
    [ \"\$(sudo docker inspect -f '{{.State.Running}}' ${OLD_CONTAINER} 2>/dev/null)\" = false ] || exit 42
    ! sudo test -e ${LEGACY_ENV_FILE} && ! sudo test -L ${LEGACY_ENV_FILE} || exit 42
    ! sudo test -e ${ENV_FILE}.tmp && ! sudo test -L ${ENV_FILE}.tmp || exit 42
    if [ \"\$new_seen\" = 1 ]; then sudo docker rm -f ${NEW_CONTAINER} >/dev/null; fi
    sudo docker rename ${OLD_CONTAINER} ${CONTAINER}
    sudo docker start ${CONTAINER} >/dev/null" || rc=$?
  if [ "$rc" = 42 ]; then
    print_manual_state_help "$host" "rollback state changed after preflight; refusing without mutation"
    return 1
  elif [ "$rc" != 0 ]; then
    echo "error: ${host}: rollback was interrupted; inspect the canonical container names manually" >&2
    print_ssh_command "$host"
    return 1
  fi
  log "restored ${OLD_CONTAINER} as running ${CONTAINER}"
}

# --- commands ---------------------------------------------------------------

# The host new-destination limit ships in the relay image as
# /usr/local/bin/egress-limit (deploy/relay/egress-limit.sh), so it versions with
# the relay. A capability-less, network-less container only copies it out; the
# host installs it at EGRESS_LIMIT_BIN and runs it with its own nft, which
# keeps userspace and kernel in step, then installs the boot unit the same
# script prints. Runs after a verified roll, so the rules always come from the
# image now serving.
EGRESS_LIMIT_BIN=/usr/local/sbin/openrung-egress-limit
EGRESS_LIMIT_UNIT=/etc/systemd/system/openrung-egress-limit.service

apply_egress_limit() { # host
  local host="$1"
  ssh_run "$host" "set -e
    command -v nft >/dev/null 2>&1 || {
      sudo env DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=300 update </dev/null >/dev/null
      sudo env DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=300 install -y nftables </dev/null >/dev/null
    }
    sudo test -d /etc/openrung || sudo install -d -m 0700 /etc/openrung
    script=\$(sudo docker run --rm --network none --cap-drop ALL --entrypoint cat ${IMAGE} /usr/local/bin/egress-limit)
    case \"\$script\" in '#!/bin/sh'*) ;; *) echo 'the image carries no egress-limit script' >&2; exit 1 ;; esac
    printf '%s\\n' \"\$script\" | sudo tee ${EGRESS_LIMIT_BIN}.new >/dev/null
    sudo chmod 0755 ${EGRESS_LIMIT_BIN}.new
    sudo mv ${EGRESS_LIMIT_BIN}.new ${EGRESS_LIMIT_BIN}
    sudo ${EGRESS_LIMIT_BIN} apply ${NEW_DEST_RATE} >/dev/null
    unit=\$(sudo ${EGRESS_LIMIT_BIN} unit)
    printf '%s\\n' \"\$unit\" | sudo tee ${EGRESS_LIMIT_UNIT} >/dev/null
    sudo systemctl daemon-reload
    sudo systemctl enable openrung-egress-limit.service >/dev/null 2>&1" \
    || { echo "error: ${host}: the relay is live, but the new-destination limit from ${IMAGE} could not be loaded; re-run update to retry" >&2
         print_ssh_command "$host"
         return 1; }
  log "new-destination limit from the relay image: ${NEW_DEST_RATE}$([ "$NEW_DEST_RATE" = off ] || printf '/s')"
}

# The access address (OPENRUNG_ACCESS_HOST) is one extra IPv6 address on the
# interface of the host's default IPv6 route. A host unit beside the egress
# limiter adds it at boot, before docker starts the relay, and removes it when
# stopped. It is added deprecated (preferred_lft 0): the kernel accepts and
# answers connections to it but never picks it as the source of a new one, so
# registration, heartbeats and client egress stay on the primary address. The
# relay checks exactly that before advertising it. nodad skips duplicate
# address detection, which would otherwise hold the address tentative.
ACCESS_UNIT_NAME=openrung-access-address.service
ACCESS_UNIT=/etc/systemd/system/${ACCESS_UNIT_NAME}

access_address_unit() { # address dev
  cat <<UNIT
[Unit]
Description=OpenRung relay access address
After=network-online.target
Wants=network-online.target
Before=docker.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/sbin/ip -6 address replace $1/128 dev $2 preferred_lft 0 nodad
ExecStop=-/sbin/ip -6 address delete $1/128 dev $2

[Install]
WantedBy=multi-user.target
UNIT
}

# A network manager that reconfigures the interface (systemd-networkd restarting
# under unattended upgrades, for one) drops addresses it does not own, and the
# one-shot unit above would stay "active" without the address. Every minute the
# re-assert timer runs a tiny script that re-adds the address the unit names
# (read from its ExecStart line) when it is missing or no longer deprecated,
# and logs a warning only then; while the address is in place it is silent,
# and LogLevelMax keeps systemd's own per-run lines out of the journal too. It
# runs only while the unit is active, so stopping the unit (an `off` or an
# address change) is never undone. None of the three files names the address.
ACCESS_REASSERT_NAME=openrung-access-address-reassert
ACCESS_REASSERT_SCRIPT=/usr/local/sbin/${ACCESS_REASSERT_NAME}
ACCESS_REASSERT_SERVICE=/etc/systemd/system/${ACCESS_REASSERT_NAME}.service
ACCESS_REASSERT_TIMER=/etc/systemd/system/${ACCESS_REASSERT_NAME}.timer

access_reassert_script() {
  cat <<'SCRIPT'
#!/bin/sh
# Re-add the access address that openrung-access-address.service configures
# when the interface has lost it (or it lost its deprecated flag). Silent
# while the address is in place; logs a warning only when it re-adds it.
set -eu
SCRIPT
  printf 'unit=%s
' "$ACCESS_UNIT"
  cat <<'SCRIPT'
set -- $(sed -n "s#^ExecStart=/sbin/ip -6 address replace \([^ ]*\)/128 dev \([^ ]*\) .*#\1 \2#p" "$unit")
if [ "$#" -ne 2 ]; then
  echo "<3>cannot read the access address from $unit"
  exit 1
fi
if ip -6 -o address show to "$1/128" | grep -q deprecated; then
  exit 0
fi
ip -6 address replace "$1/128" dev "$2" preferred_lft 0 nodad
echo "<4>re-added access address $1 on $2"
SCRIPT
}

access_reassert_service() {
  cat <<UNIT
[Unit]
Description=Re-add the OpenRung relay access address if it is missing
After=${ACCESS_UNIT_NAME}

[Service]
Type=oneshot
LogLevelMax=notice
ExecCondition=/bin/systemctl is-active --quiet ${ACCESS_UNIT_NAME}
ExecStart=${ACCESS_REASSERT_SCRIPT}
UNIT
}

access_reassert_timer() {
  cat <<UNIT
[Unit]
Description=Re-assert the OpenRung relay access address every minute

[Timer]
OnBootSec=1min
OnUnitActiveSec=1min
AccuracySec=5s

[Install]
WantedBy=timers.target
UNIT
}

# Install (idempotently) and start the re-assert timer.
install_access_reassert() { # host
  local host="$1"
  access_reassert_script | ssh_run "$host" "sudo tee ${ACCESS_REASSERT_SCRIPT}.new >/dev/null && sudo chmod 0755 ${ACCESS_REASSERT_SCRIPT}.new && sudo mv ${ACCESS_REASSERT_SCRIPT}.new ${ACCESS_REASSERT_SCRIPT}" \
    && access_reassert_service | ssh_run "$host" "sudo tee ${ACCESS_REASSERT_SERVICE} >/dev/null" \
    && access_reassert_timer | ssh_run "$host" "sudo tee ${ACCESS_REASSERT_TIMER} >/dev/null" \
    && ssh_run "$host" "sudo systemctl daemon-reload && sudo systemctl enable --now ${ACCESS_REASSERT_NAME}.timer >/dev/null 2>&1"
}

# An access address change is split around the roll so that a failure at any
# point leaves the previous relay with exactly the addresses it had:
#   prepare_access_address  (before the roll) adds the new address beside any
#                           previous one, so the candidate finds it in place
#                           at its first registration;
#   abort_access_address    (a roll whose candidate never went live) removes
#                           only an address this run added — the previous unit
#                           and address are untouched; a candidate that was
#                           promoted keeps the address (see ROLL_PROMOTED);
#   commit_access_address   (verified roll) swaps the boot unit, restoring the
#                           previous unit if the new one does not come up.
# Unset OPENRUNG_ACCESS_HOST leaves the host as it is; `off` changes nothing
# until the roll is verified, then removes the unit (and with it the address).
ACCESS_DEV=""     # the interface prepare_access_address chose
ACCESS_ADDED=0    # 1 when prepare_access_address added the address itself

# An address the host already carries as a preferred address is refused: that
# is a primary address, not one this script configured.
prepare_access_address() { # host
  local host="$1" dev out rc=0
  ACCESS_DEV=""; ACCESS_ADDED=0
  [ -n "$ACCESS_HOST" ] && [ "$ACCESS_HOST" != off ] || return 0
  dev="$(ssh_run "$host" "ip -6 route show default | sed -n 's/.* dev \([^ ]*\).*/\1/p' | head -1" | scrub)" || dev=""
  [[ "$dev" =~ $DEV_RE ]] \
    || { echo "error: ${host}: no IPv6 default route to carry the access address (got '$(printf '%s' "$dev" | head -c 40)')" >&2; return 1; }
  out="$(ssh_run "$host" "set -e
    current=\$(ip -6 -o address show to ${ACCESS_HOST}/128)
    if [ -n \"\$current\" ] && ! printf '%s' \"\$current\" | grep -q deprecated; then exit 42; fi
    added=0
    if [ -z \"\$current\" ]; then
      sudo ip -6 address replace ${ACCESS_HOST}/128 dev ${dev} preferred_lft 0 nodad
      added=1
    fi
    if ! ip -6 -o address show to ${ACCESS_HOST}/128 | grep -q deprecated; then
      if [ \"\$added\" = 1 ]; then sudo ip -6 address delete ${ACCESS_HOST}/128 dev ${dev} || true; fi
      exit 1
    fi
    echo \"added=\$added\"" | scrub)" || rc=$?
  if [ "$rc" = 42 ]; then
    echo "error: ${host}: ${ACCESS_HOST} is already a preferred address on this host (its primary address?); choose another address for OPENRUNG_ACCESS_HOST" >&2
    return 1
  elif [ "$rc" != 0 ]; then
    echo "error: ${host}: could not add the access address ${ACCESS_HOST} on ${dev}; the live relay was not touched — re-run convert to retry" >&2
    print_ssh_command "$host"
    return 1
  fi
  ACCESS_DEV="$dev"
  [ "$out" = added=1 ] && ACCESS_ADDED=1
  log "access address: ${ACCESS_HOST} on ${dev} ($([ "$ACCESS_ADDED" = 1 ] && echo added || echo already present))"
}

abort_access_address() { # host
  local host="$1"
  [ "$ACCESS_ADDED" = 1 ] || return 0
  ACCESS_ADDED=0
  ssh_run "$host" "sudo ip -6 address delete ${ACCESS_HOST}/128 dev ${ACCESS_DEV}" \
    && log "access address: removed ${ACCESS_HOST}, which this run added; the previous relay keeps its addresses" \
    || warn "${host}: could not remove ${ACCESS_HOST}, which this run added; remove it by hand (ip -6 address delete ${ACCESS_HOST}/128 dev ${ACCESS_DEV})"
  return 0
}

commit_access_address() { # host
  local host="$1" rc=0
  [ -n "$ACCESS_HOST" ] || return 0
  if [ "$ACCESS_HOST" = off ]; then
    ssh_run "$host" "set -e
      if sudo test -f ${ACCESS_REASSERT_TIMER} || sudo test -f ${ACCESS_REASSERT_SERVICE}; then
        sudo systemctl disable --now ${ACCESS_REASSERT_NAME}.timer >/dev/null 2>&1 || true
        sudo systemctl stop ${ACCESS_REASSERT_NAME}.service >/dev/null 2>&1 || true
        sudo rm -f ${ACCESS_REASSERT_TIMER} ${ACCESS_REASSERT_SERVICE} ${ACCESS_REASSERT_SCRIPT}
        sudo systemctl daemon-reload
      fi
      if sudo test -f ${ACCESS_UNIT}; then
        sudo systemctl disable --now ${ACCESS_UNIT_NAME} >/dev/null 2>&1 || true
        sudo rm -f ${ACCESS_UNIT}
        sudo systemctl daemon-reload
      fi" \
      || { echo "error: ${host}: the relay is live without an access address, but the access address unit could not be removed; re-run convert" >&2; print_ssh_command "$host"; return 1; }
    log "access address: removed"
    return 0
  fi
  [[ "$ACCESS_DEV" =~ $DEV_RE ]] \
    || { echo "error: ${host}: internal: the access address was not prepared before the roll" >&2; return 1; }
  # Stopping a previous unit runs its ExecStop, which removes the PREVIOUS
  # address only; it is skipped when that unit already names this address,
  # which the relay is now serving.
  access_address_unit "$ACCESS_HOST" "$ACCESS_DEV" | ssh_run "$host" "
    unit=\$(cat)
    printf '%s\\n' \"\$unit\" | sudo tee ${ACCESS_UNIT}.new >/dev/null || exit 1
    if sudo test -f ${ACCESS_UNIT} && sudo cmp -s ${ACCESS_UNIT}.new ${ACCESS_UNIT}; then
      sudo rm -f ${ACCESS_UNIT}.new
      sudo systemctl enable --now ${ACCESS_UNIT_NAME} >/dev/null 2>&1 || exit 1
      exit 0
    fi
    had_old=0
    # A re-assert run that read the old unit could re-add its address after
    # the stop below removes it: stop the timer and wait out a run in
    # progress (systemctl stop returns once it has exited) before any
    # address is touched. install_access_reassert starts the timer again.
    sudo systemctl stop ${ACCESS_REASSERT_NAME}.timer ${ACCESS_REASSERT_NAME}.service >/dev/null 2>&1 || true
    if sudo test -f ${ACCESS_UNIT}; then
      had_old=1
      sudo cp -p ${ACCESS_UNIT} ${ACCESS_UNIT}.old || exit 1
      if ! sudo grep -q 'address replace ${ACCESS_HOST}/128 ' ${ACCESS_UNIT}; then
        sudo systemctl stop ${ACCESS_UNIT_NAME} || true
      fi
    fi
    if sudo mv ${ACCESS_UNIT}.new ${ACCESS_UNIT} && sudo systemctl daemon-reload \\
       && sudo systemctl enable --now ${ACCESS_UNIT_NAME} >/dev/null 2>&1 \\
       && ip -6 -o address show to ${ACCESS_HOST}/128 | grep -q deprecated; then
      sudo rm -f ${ACCESS_UNIT}.old
      exit 0
    fi
    sudo rm -f ${ACCESS_UNIT}.new
    if [ \"\$had_old\" = 1 ]; then
      sudo mv ${ACCESS_UNIT}.old ${ACCESS_UNIT}
      sudo systemctl daemon-reload
      sudo systemctl enable --now ${ACCESS_UNIT_NAME} >/dev/null 2>&1 || true
      if sudo test -f ${ACCESS_REASSERT_TIMER}; then sudo systemctl start ${ACCESS_REASSERT_NAME}.timer >/dev/null 2>&1 || true; fi
    else
      sudo systemctl disable ${ACCESS_UNIT_NAME} >/dev/null 2>&1 || true
      sudo rm -f ${ACCESS_UNIT}
      sudo systemctl daemon-reload
    fi
    exit 43" || rc=$?
  if [ "$rc" = 43 ]; then
    echo "error: ${host}: the relay is live on ${ACCESS_HOST}, but its boot unit did not start; the previous unit was restored, so a reboot would not keep ${ACCESS_HOST} — re-run convert" >&2
    print_ssh_command "$host"
    return 1
  elif [ "$rc" != 0 ]; then
    echo "error: ${host}: the relay is live on ${ACCESS_HOST}, but its boot unit could not be installed; re-run convert" >&2
    print_ssh_command "$host"
    return 1
  fi
  install_access_reassert "$host" \
    || { echo "error: ${host}: the access address unit is in place, but its re-assert timer could not be installed, so a network reconfiguration could drop ${ACCESS_HOST}; re-run convert" >&2
         print_ssh_command "$host"
         return 1; }
  log "access address: ${ACCESS_HOST} kept by ${ACCESS_UNIT_NAME}, re-asserted every minute"
}

convert_host() {
  local host="$1" ident public_host label
  echo "==> convert ${host}"
  # Recheck immediately before this host's first mutation. Fleet-wide preflight
  # already ensured a later host cannot begin in an unsupported state after an
  # earlier host has changed.
  preflight_host "$host" convert || return
  prepare_image "$host" || return

  # Everything already configured on the host is preserved; only the broker URL
  # and the token are (re)written. The identity readback below is display-only.
  stage_preserved_env "$host" "$CONTAINER" || return
  # On readback failure, remove the staged tmp (it holds the preserved Reality
  # private key and no trap guards it until install_env_file's session).
  ident="$(ssh_run "$host" "sudo sed -n -e 's/^OPENRUNG_PUBLIC_HOST=/public_host=/p' -e 's/^OPENRUNG_LABEL=/label=/p' ${ENV_FILE}.tmp")" \
    || { ssh_run "$host" "sudo rm -f ${ENV_FILE}.tmp" >/dev/null 2>&1 || true
         die "${host}: could not read back the staged identity"; }
  public_host="$(printf '%s\n' "$ident" | sed -n 's/^public_host=//p' | tail -1 | scrub | head -c 100)"
  label="$(printf '%s\n' "$ident" | sed -n 's/^label=//p' | tail -1 | scrub | head -c 100)"

  log "identity: label=${label:--} public_host=${public_host} (preserved)"
  install_env_file "$host" || return
  log "wrote ${ENV_FILE} (root:root 0600; existing settings preserved, broker URL + token updated)"
  prepare_access_address "$host" || return
  if ! roll_host "$host" "$ENV_FILE"; then
    # Only a candidate that never went live leaves the previous relay in
    # charge, so only then is the access change undone. A promoted candidate
    # (only the old container's removal failed) serves the new address, so
    # its unit is still committed; an interrupted commit keeps the address.
    case "$ROLL_PROMOTED" in
      0) abort_access_address "$host" ;;
      1) commit_access_address "$host" || true ;;
      *) [ "$ACCESS_ADDED" = 1 ] && warn "${host}: the roll was interrupted after promotion began; kept ${ACCESS_HOST}, which the live relay may be serving" ;;
    esac
    return 1
  fi
  commit_access_address "$host" || return
  apply_egress_limit "$host" || return
}

cmd_convert() {
  [ "$#" -ge 1 ] || usage
  require_https_broker || return
  validate_config || return
  export -n OPENRUNG_FOUNDATION_TOKEN OPENRUNG_FOUNDATION_TOKEN_CMD 2>/dev/null || true
  local host
  # Inspect the entire fleet before invoking the token source. The source
  # variables were made non-exported at startup, so aws/ssh children cannot see
  # them during this read-only phase.
  preflight_fleet convert "$@" || return
  load_token || return
  for host in "$@"; do
    convert_host "$host" || return
  done
}

cmd_update() {
  [ "$#" -ge 1 ] || usage
  validate_config || return
  unset OPENRUNG_FOUNDATION_TOKEN OPENRUNG_FOUNDATION_TOKEN_CMD
  # No token needed: the credentials already on each host are reused as-is.
  # Every target passes read-only preflight before the first image pull.
  local host
  preflight_fleet update "$@" || return
  for host in "$@"; do
    echo "==> update ${host}"
    preflight_host "$host" update || return
    prepare_image "$host" || return
    log "reusing ${ENV_FILE}; image ${IMAGE}"
    roll_host "$host" "$ENV_FILE" || return
    apply_egress_limit "$host" || return
  done
}

cmd_rollback() {
  [ "$#" -ge 1 ] || usage
  validate_config || return
  unset OPENRUNG_FOUNDATION_TOKEN OPENRUNG_FOUNDATION_TOKEN_CMD
  local host
  preflight_fleet rollback "$@" || return
  for host in "$@"; do
    echo "==> rollback ${host}"
    rollback_host "$host" || return
  done
}

cmd_create() {
  require_https_broker || return
  validate_config || return
  load_token || return   # fail before provisioning, not after

  # lightsail-up.sh refuses to run with token variables set (its own user-data
  # guard), and stripping them here makes the guarantee structural: the
  # provisioning child never holds the credential, so no bug in it could ever
  # embed the token in user-data.
  local out ip name
  out="$(env -u OPENRUNG_FOUNDATION_TOKEN -u OPENRUNG_FOUNDATION_TOKEN_CMD \
             -u OPENRUNG_VOLUNTEER_TOKEN -u OPENRUNG_NODE_CLASS \
             "${HERE}/lightsail-up.sh" "$@" | tee /dev/stderr)" \
    || die "Lightsail provisioning failed; no credential installation was attempted"
  # lightsail-up.sh's machine-readable handoff line.
  name="$(printf '%s\n' "$out" | sed -n 's/^OPENRUNG_RELAY name=\([^ ]*\) ip=.*$/\1/p' | head -1)"
  ip="$(printf '%s\n' "$out" | sed -n 's/^OPENRUNG_RELAY .*ip=\(.*\)$/\1/p' | head -1)"
  [ -n "$ip" ] && [ -n "$name" ] || die "could not parse the 'OPENRUNG_RELAY name=... ip=...' line from lightsail-up.sh"
  assert_matches "$name" "$NAME_RE" "instance name"
  assert_matches "$ip" "$HOST_RE" "instance ip"

  # Pin before the first connection; the token transits this session, so an
  # unverifiable key is fatal (see pin_host_key). Host keys are published
  # shortly after boot; retry while the instance settles.
  pin_host_key "$ip" "$name" 6 || return

  # The wait loop polls quietly, but a persistent ssh failure (wrong key pair,
  # host-key mismatch) must not masquerade as "still bootstrapping" — surface
  # the last ssh error when the wait times out. Direct ssh (not ssh_run) so the
  # error is captured synchronously; it is scrubbed before display.
  echo "==> waiting for ${name} (${ip}) to finish bootstrapping"
  local i=0 waiterr=""
  until waiterr="$(ssh "${SSH_OPTS[@]}" "${SSH_USER}@${ip}" "sudo docker inspect ${CONTAINER} >/dev/null 2>&1" 2>&1)"; do
    i=$((i + 1))
    [ "$i" -lt 60 ] || die "${ip}: relay container did not appear within 5 minutes (last ssh error: $(printf '%s' "$waiterr" | scrub | tail -c 160)); check /var/log/openrung-init.log"
    sleep 5
  done
  convert_host "$ip" || return
}

# Read-only. Per host: the running relay's version, whether the saved host
# ruleset is exactly what that relay's own image renders at OPENRUNG_NEW_DEST_RATE,
# whether the installed egress-limit script is that image's copy, whether the
# host has the nftables it needs, whether the table is loaded, whether the
# boot unit is enabled, and the access address the env file names: none, or
# whether it is present (and deprecated) with its unit enabled. Everything
# printed comes back from the host, so it is scrubbed before display.
cmd_audit() {
  [ "$#" -ge 1 ] || usage
  validate_config || return
  [ "$NEW_DEST_RATE" != off ] \
    || die "audit compares each host against a rate; set OPENRUNG_NEW_DEST_RATE to the rate in use"
  unset OPENRUNG_FOUNDATION_TOKEN OPENRUNG_FOUNDATION_TOKEN_CMD
  local host out
  for host in "$@"; do
    assert_matches "$host" "$HOST_RE" "host"
    pin_host_key "$host" || return
    out="$(ssh_run "$host" "
      image=\$(sudo docker inspect -f '{{.Image}}' ${CONTAINER} 2>/dev/null) || { echo relay=missing; exit 0; }
      version=\$(sudo docker logs ${CONTAINER} 2>&1 | grep -m1 -o 'starting relay version=[^ ]*' | sed 's/.*=//')
      echo \"relay=\${version:-unknown}\"
      expected=\$(mktemp)
      if ! sudo docker run --rm --network none --cap-drop ALL --entrypoint /usr/local/bin/egress-limit \"\$image\" render ${NEW_DEST_RATE} >\"\$expected\" 2>/dev/null; then
        echo rules=image-without-egress-limit
      elif ! sudo test -f /etc/openrung/egress-limit.nft; then
        echo rules=missing
      elif sudo cat /etc/openrung/egress-limit.nft | cmp -s - \"\$expected\"; then
        echo rules=current
      else
        echo rules=stale
      fi
      rm -f \"\$expected\"
      if ! sudo test -f ${EGRESS_LIMIT_BIN}; then
        echo script=missing
      elif sudo docker run --rm --network none --cap-drop ALL --entrypoint cat \"\$image\" /usr/local/bin/egress-limit 2>/dev/null | sudo cmp -s - ${EGRESS_LIMIT_BIN}; then
        echo script=current
      else
        echo script=stale
      fi
      if ! command -v nft >/dev/null 2>&1; then
        echo host_nft=missing loaded=unknown
      elif sudo nft list table inet openrung_egress >/dev/null 2>&1; then
        echo host_nft=yes loaded=yes
      else
        echo host_nft=yes loaded=no
      fi
      echo \"unit=\$(systemctl is-enabled openrung-egress-limit.service 2>/dev/null || echo missing)\"
      access=\$(sudo sed -n 's/^OPENRUNG_ACCESS_HOST=//p' ${ENV_FILE} 2>/dev/null | tail -1)
      if [ -z \"\$access\" ]; then
        echo access=none
      else
        current=\$(ip -6 -o address show to \"\$access/128\" 2>/dev/null)
        if [ -z \"\$current\" ]; then state=missing
        elif printf '%s' \"\$current\" | grep -q deprecated; then state=present
        else state=preferred; fi
        echo \"access=\$access access_state=\$state access_unit=\$(systemctl is-enabled ${ACCESS_UNIT_NAME} 2>/dev/null || echo missing) access_reassert=\$(systemctl is-active ${ACCESS_REASSERT_NAME}.timer 2>/dev/null || echo missing)\"
      fi" 2>/dev/null)" \
      || out="unreachable"
    printf '%s %s\n' "$host" "$(printf '%s' "$out" | scrub_stream | tr '\n' ' ' | head -c 500)"
  done
}

main() {
  [ "$#" -ge 1 ] || usage
  local subcommand="$1"; shift
  case "$subcommand" in
    create)   cmd_create "$@" ;;
    convert)  cmd_convert "$@" ;;
    update)   cmd_update "$@" ;;
    rollback) cmd_rollback "$@" ;;
    audit)    cmd_audit "$@" ;;
    *)        usage ;;
  esac
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  main "$@"
fi
