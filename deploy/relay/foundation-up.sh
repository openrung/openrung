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
# OPENRUNG_SSH_KEY, OPENRUNG_SSH_USER, OPENRUNG_REGION (host-key pinning lookups),
# OPENRUNG_ALLOW_TOFU=1 (explicitly accept an unverifiable first-contact host key).
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
# accept-new never accepts a CHANGED key, and pin_host_key below guarantees a
# key is already known (API-pinned, operator-added, or explicitly TOFU'd via
# OPENRUNG_ALLOW_TOFU=1) before any mode connects — no key is ever learned
# implicitly. UserKnownHostsFile pins ssh to the same file pin_host_key writes,
# so the check and the connection cannot diverge on a per-host ssh_config.
SSH_OPTS=(-o StrictHostKeyChecking=accept-new -o "UserKnownHostsFile=$HOME/.ssh/known_hosts" -o ConnectTimeout=10 -o BatchMode=yes -i "$SSH_KEY")

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

# Exactly one of the known values (or empty): remote detection output must
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

# Capture the token for this run, then drop the source variables so children
# (ssh, aws, ssh-keygen) never inherit the credential — the same rationale as
# create's env -u for lightsail-up.sh.
load_token() {
  TOKEN="$(read_token)"
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
# canonical one. Empty when the host runs neither. Note: matches stopped
# containers too — callers that need a live one must check state themselves.
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

# Stage the preserved settings into ENV_FILE.tmp on the host. Sources: the
# legacy env file, then the canonical one (both when present — docker's
# last-occurrence-wins semantics make the canonical file take precedence per
# key while legacy-only keys still survive), else the running container's
# OPENRUNG_* environment. Only OPENRUNG_* lines are kept. The filter runs on
# the host and the result stays there: no secret and no preserved value ever
# transits the connection or a command line. Fails when no OPENRUNG_PUBLIC_HOST
# survives, since direct mode cannot run without one. Deliberately no
# OPENRUNG_LISTEN_HOST default is added: the binary's own default (::,
# dual-stack) is correct, and pinning 0.0.0.0 here would silently cut IPv6 on
# hosts that relied on the default.
stage_preserved_env() { # host container
  local host="$1" container="$2"
  ssh_run "$host" "sudo sh -c \"set -e; umask 077 \
    && { test -d ${ENV_FILE%/*} || install -d -m 700 -o root -g root ${ENV_FILE%/*}; } \
    && { if test -f ${ENV_FILE} || test -f ${LEGACY_ENV_FILE}; then \
           if test -f ${LEGACY_ENV_FILE}; then cat ${LEGACY_ENV_FILE}; fi; \
           if test -f ${ENV_FILE}; then cat ${ENV_FILE}; fi; \
         else docker inspect ${container} --format '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null || true; fi; } \
       | sed -n '/^OPENRUNG_[A-Za-z0-9_]*=/p' | sed -E '${PRESERVE_DROP}' > ${ENV_FILE}.tmp \
    && grep -q '^OPENRUNG_PUBLIC_HOST=..*' ${ENV_FILE}.tmp\"" \
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

# A convert writes the canonical path with everything preserved into it; once
# the relay has verified, remove the legacy volunteer.env so exactly one copy
# of the token remains on disk. (The rollback container does not need the file:
# its env was baked in at create.) Post-success hygiene: an ssh failure here
# warns rather than fails the conversion, but never silently — and it is
# distinguished from "no legacy file" by an explicit marker, not an exit code.
cleanup_legacy_env() {
  local host="$1" present
  [ "$ENV_FILE" = "$LEGACY_ENV_FILE" ] && return 0   # operator pointed the canonical path at the legacy one
  present="$(ssh_run "$host" "sudo sh -c 'rm -f ${ENV_FILE}.tmp; if test -f ${LEGACY_ENV_FILE}; then echo yes; else echo no; fi'")" \
    || { warn "${host}: could not check for a legacy env file; re-run convert or remove ${LEGACY_ENV_FILE} manually"; return 0; }
  assert_one_of "$present" "legacy-check result" yes no
  if [ "$present" = yes ]; then
    ssh_run "$host" "sudo sh -c 'shred -u ${LEGACY_ENV_FILE} 2>/dev/null || rm -f ${LEGACY_ENV_FILE}'" \
      || { warn "${host}: could not remove ${LEGACY_ENV_FILE}; remove it manually"; return 0; }
    log "removed legacy ${LEGACY_ENV_FILE}; credentials now live only at ${ENV_FILE}"
  fi
}

rollback_hint() {
  local host="$1"
  echo "  rollback: ssh ${SSH_USER}@${host} 'sudo docker rm -f ${CONTAINER}; sudo docker rename ${CONTAINER}-old ${CONTAINER} && sudo docker start ${CONTAINER}'" >&2
}

# Recreate the relay container against env_file, in three separately-reported
# stages so a failure names exactly what state the host is in. The previous
# container is kept, stopped, as <name>-old for in-place rollback — and the
# rollback hint is only printed once the swap has actually happened, because
# before that point "rm -f openrung-relay + rename -old" would destroy the
# healthy live container. The swap stage also refuses to discard an existing
# -old backup when the "live" container is not actually running: that state
# means a previous roll already failed, and deleting the backup before the new
# image has proven itself would destroy the last healthy relay.
#
# Container hardening mirrors lightsail-up.sh exactly: --cap-drop ALL with only
# NET_BIND_SERVICE re-added (the binary binds 443 through a cap_net_bind_service
# file capability), read-only rootfs, and deliberately NO --security-opt
# no-new-privileges, which would disable that file capability and break the bind.
recreate_container() {
  local host="$1" env_file="$2" live="$3"
  ssh_run "$host" "sudo docker pull ${IMAGE} >/dev/null" \
    || die "${host}: image pull failed; the live container was not touched"
  ssh_run "$host" "set -e
    if [ \"\$(sudo docker inspect -f '{{.State.Running}}' '${live}' 2>/dev/null)\" != true ] \
       && sudo docker inspect ${CONTAINER}-old >/dev/null 2>&1; then
      echo 'refusing to discard the ${CONTAINER}-old backup: ${live} is not running (previous roll failed?)' >&2
      exit 42
    fi
    sudo docker rm -f ${CONTAINER}-old ${LEGACY_CONTAINER}-old >/dev/null 2>&1 || true
    sudo docker stop '${live}' >/dev/null
    sudo docker rename '${live}' ${CONTAINER}-old" \
    || die "${host}: could not swap out '${live}'. If a stopped '${live}' coexists with a '${CONTAINER}-old' backup, a previous roll failed — restore the backup (docker rename ${CONTAINER}-old ${live}; docker start ${live}) or remove the dead container, then re-run. If '${live}' was stopped but not renamed, restart it with: docker start '${live}'"
  ssh_run "$host" "sudo docker run -d --name ${CONTAINER} --restart unless-stopped \
      --network host --cap-drop ALL --cap-add NET_BIND_SERVICE --read-only --tmpfs /tmp \
      --env-file ${env_file} \
      ${IMAGE} >/dev/null" \
    || { rollback_hint "$host"; die "${host}: docker run failed"; }
}

# A relay that presents the Foundation token forces node_class=foundation, and it
# exits during startup if the broker attests any other class (cmd/relay refuses a
# mismatched attestation; the broker 403s a foundation claim without the token).
# So a container that logged a registration AND is running cleanly (no restarts —
# a post-registration crash loop keeps the old log line while the relay flaps) IS
# the proof the broker attested Foundation. This holds only because convert
# always writes a validated non-empty token and update asserts one is present.
# An ssh failure during polling is treated as transient — never as evidence the
# container died — so a network blip cannot trigger a false rollback hint.
verify_foundation() {
  local host="$1" i=0 line state ps_out
  while [ "$i" -lt 30 ]; do
    if ssh_run "$host" "sudo docker logs ${CONTAINER} 2>&1 | grep -q 'registered relay'"; then
      if state="$(ssh_run "$host" "sudo docker inspect -f '{{.State.Running}} {{.RestartCount}}' ${CONTAINER} 2>/dev/null")"; then
        if [ "$state" = "true 0" ]; then
          line="$(ssh_run "$host" "sudo docker logs ${CONTAINER} 2>&1 | grep 'registered relay' | tail -1" | scrub | head -c 300)" || line=""
          log "registered and running: ${line}"
          return 0
        fi
        rollback_hint "$host"
        die "${host}: container registered but is not running cleanly (state: $(printf '%s' "$state" | scrub | head -c 40)); check: docker logs ${CONTAINER}"
      fi
    elif ps_out="$(ssh_run "$host" "sudo docker ps --format '{{.Names}}'" 2>/dev/null)"; then
      if ! printf '%s\n' "$ps_out" | grep -qx "${CONTAINER}"; then
        rollback_hint "$host"
        die "${host}: container is not running; check: docker logs ${CONTAINER}"
      fi
    fi
    i=$((i + 1)); sleep 2
  done
  rollback_hint "$host"
  die "${host}: no registration within 60s; check: docker logs ${CONTAINER}"
}

# --- commands ---------------------------------------------------------------

convert_host() {
  local host="$1" live ident public_host label
  echo "==> convert ${host}"
  live="$(detect_container "$host")"
  [ -n "$live" ] || die "${host}: no relay container found; provision it first (lightsail-up.sh or 'create')"

  # Everything already configured on the host is preserved; only the broker URL
  # and the token are (re)written. The identity readback below is display-only.
  stage_preserved_env "$host" "$live"
  ident="$(ssh_run "$host" "sudo sed -n -e 's/^OPENRUNG_PUBLIC_HOST=/public_host=/p' -e 's/^OPENRUNG_LABEL=/label=/p' ${ENV_FILE}.tmp")"
  public_host="$(printf '%s\n' "$ident" | sed -n 's/^public_host=//p' | tail -1 | scrub | head -c 100)"
  label="$(printf '%s\n' "$ident" | sed -n 's/^label=//p' | tail -1 | scrub | head -c 100)"

  log "identity: label=${label:--} public_host=${public_host} (preserved)"
  install_env_file "$host"
  log "wrote ${ENV_FILE} (root:root 0600; existing settings preserved, broker URL + token updated)"
  recreate_container "$host" "$ENV_FILE" "$live"
  verify_foundation "$host"
  cleanup_legacy_env "$host"
}

cmd_convert() {
  [ "$#" -ge 1 ] || usage
  require_https_broker
  validate_config
  load_token
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
    [ -n "$live" ] || die "${host}: no relay container found to update; run 'convert' (or 'create') first"
    log "reusing ${env_file}; image ${IMAGE}"
    recreate_container "$host" "$env_file" "$live"
    verify_foundation "$host"
  done
}

cmd_create() {
  require_https_broker
  validate_config
  load_token   # fail before provisioning, not after

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

  # Pin before the first connection; the token transits this session, so an
  # unverifiable key is fatal (see pin_host_key). Host keys are published
  # shortly after boot; retry while the instance settles.
  pin_host_key "$ip" "$name" 6

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
