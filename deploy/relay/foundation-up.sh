#!/usr/bin/env bash
#
# Provision, convert, and update OpenRung Foundation-operated relays.
#
#   deploy/relay/foundation-up.sh create [name]        # new Lightsail host, then install credentials
#   deploy/relay/foundation-up.sh convert <host>...    # install credentials on an existing host
#   deploy/relay/foundation-up.sh update  <host>...    # pull the image and recreate, reusing credentials
#   deploy/relay/foundation-up.sh rollback <host>...   # restore an exact interrupted roll
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
# OPENRUNG_ALLOW_TOFU=1 (explicitly accept an unverifiable first-contact host key).
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

The Foundation token is read from OPENRUNG_FOUNDATION_TOKEN_CMD (preferred) or
OPENRUNG_FOUNDATION_TOKEN — never from argv. `update` and `rollback` need no token.

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
PRESERVE_DROP='/^OPENRUNG_(BROKER_URL|FOUNDATION_TOKEN|VOLUNTEER_TOKEN|NODE_CLASS|XRAY_PATH|CONFIG_OUT)=/d'

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
       | sed -n '/^OPENRUNG_[A-Za-z0-9_]*=/p' | sed -E '${PRESERVE_DROP}' > ${ENV_FILE}.tmp \
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
commit_candidate() {
  local host="$1" rc=0
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
  roll_host "$host" "$ENV_FILE" || return
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

main() {
  [ "$#" -ge 1 ] || usage
  local subcommand="$1"; shift
  case "$subcommand" in
    create)   cmd_create "$@" ;;
    convert)  cmd_convert "$@" ;;
    update)   cmd_update "$@" ;;
    rollback) cmd_rollback "$@" ;;
    *)        usage ;;
  esac
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  main "$@"
fi
