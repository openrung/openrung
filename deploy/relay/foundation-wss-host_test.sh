#!/usr/bin/env bash
# Regression tests for foundation-wss-host.sh.
#
# The production helper is a command-line program, not a sourceable library.
# These tests therefore run the real entry point with fake ssh, ssh-keygen,
# stat, and sudo commands.  No network connection or privileged host path is
# touched.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="${HERE}/foundation-wss-host.sh"
TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/openrung-foundation-wss-host-test.XXXXXX")"
trap 'rm -rf "$TEST_TMP"' EXIT

FAKE_BIN="${TEST_TMP}/bin"
SSH_KEY="${TEST_TMP}/ssh-key"
KNOWN_HOSTS="${TEST_TMP}/known-hosts"
TICKET_FILE="${TEST_TMP}/ticket-public-keys"
TOKENS_FILE="${TEST_TMP}/origin-tokens.json"
OUTPUT="${TEST_TMP}/output"
ORIGINAL_PATH="$PATH"
PASS=0
FAIL=0

mkdir -p "$FAKE_BIN"
printf 'fake private key\n' >"$SSH_KEY"
printf 'relay.example.test ssh-ed25519 fake\n' >"$KNOWN_HOSTS"
printf 'QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE=\n' >"$TICKET_FILE"
printf '["0123456789abcdef0123456789abcdef"]\n' >"$TOKENS_FILE"
chmod 0600 "$SSH_KEY" "$KNOWN_HOSTS" "$TICKET_FILE" "$TOKENS_FILE"

cat >"${FAKE_BIN}/ssh-keygen" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

cat >"${FAKE_BIN}/ssh" <<'EOF'
#!/usr/bin/env bash
set -eu

counter_file="${SSH_CAPTURE_DIR}/counter"
count=0
if [[ -f "$counter_file" ]]; then
  count="$(<"$counter_file")"
fi
count=$((count + 1))
printf '%s\n' "$count" >"$counter_file"

remote_command=""
for argument in "$@"; do
  remote_command="$argument"
done
printf '%s' "$remote_command" >"${SSH_CAPTURE_DIR}/command.${count}"
if [[ "$remote_command" == *'/dev/stdin'* ]]; then
  cat >"${SSH_CAPTURE_DIR}/stdin.${count}"
else
  : >"${SSH_CAPTURE_DIR}/stdin.${count}"
fi

if [[ "${SSH_EXECUTE_REMOTE_AUDIT:-0}" == 1 ]]; then
  mapped_command="${remote_command//\/etc\/openrung/${AUDIT_ROOT}\/etc\/openrung}"
  mapped_command="${mapped_command//\/etc\/caddy/${AUDIT_ROOT}\/etc\/caddy}"
  PATH="${AUDIT_FAKE_BIN}:${AUDIT_ORIGINAL_PATH}" STAT_SCENARIO=audit \
    bash -c "$mapped_command"
fi
EOF

cat >"${FAKE_BIN}/stat" <<'EOF'
#!/usr/bin/env bash
set -u
printf '%s\n' "$*" >>"$STAT_LOG"

case "${STAT_SCENARIO:-bsd}" in
  audit)
    printf '600 root:root\n'
    exit 0
    ;;
  bsd)
    if [[ "${1:-}" == -f ]]; then
      printf '600\n'
      exit 0
    fi
    exit 90
    ;;
  gnu)
    if [[ "${1:-}" == -f ]]; then
      # GNU stat can emit output for a valid operand even though another
      # operand made the overall BSD-style probe fail.  That output must not
      # be retained as part of the eventual mode.
      printf 'filesystem-status-from-failed-probe\n'
      exit 1
    fi
    if [[ "${1:-}" == -c ]]; then
      printf '600\n'
      exit 0
    fi
    exit 91
    ;;
  unreadable-mode)
    printf '644\n'
    exit 0
    ;;
  fail)
    printf 'untrusted-probe-output\n'
    exit 1
    ;;
  *)
    echo "unknown STAT_SCENARIO: ${STAT_SCENARIO:-}" >&2
    exit 92
    ;;
esac
EOF

cat >"${FAKE_BIN}/docker" <<'EOF'
#!/usr/bin/env bash
set -eu

[[ "${1:-}" == inspect ]] || exit 93
shift
format=
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    -f|--format)
      format="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
case "$format" in
  *'.Config.Entrypoint'*)
    printf '["/usr/local/bin/wss-sidecar"] null ["ALL"] ["no-new-privileges:true"]\n'
    ;;
  *'.Mounts'*)
    printf 'volume openrung-wss-replay-relay-a true\n'
    ;;
  *'.Config.Image'*'.HostConfig.ReadonlyRootfs'*)
    printf '%s true 0 host true\n' "$AUDIT_IMAGE"
    ;;
  *)
    exit 94
    ;;
esac
EOF

cat >"${FAKE_BIN}/ss" <<'EOF'
#!/usr/bin/env bash
printf 'LISTEN 0 4096 127.0.0.1:8081 0.0.0.0:*\n'
printf 'LISTEN 0 4096 0.0.0.0:8443 0.0.0.0:*\n'
EOF

cat >"${FAKE_BIN}/systemctl" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

# Execute a captured remote writer locally.  Root ownership flags are removed,
# while every other install argument and the rm/install/mv ordering are kept.
cat >"${FAKE_BIN}/sudo" <<'EOF'
#!/usr/bin/env bash
set -eu

if [[ "${1:-}" == install ]]; then
  shift
  directory=0
  mode=
  previous=
  destination=
  while [[ "$#" -gt 0 ]]; do
    case "$1" in
      -o|-g)
        shift 2
        ;;
      -m)
        mode="$2"
        shift 2
        ;;
      -d)
        directory=1
        shift
        ;;
      *)
        previous="$destination"
        destination="$1"
        shift
        ;;
    esac
  done
  if [[ "$directory" -eq 1 ]]; then
    mkdir -p "$destination"
    [[ -z "$mode" ]] || chmod "$mode" "$destination"
    exit 0
  fi

  # Model the GNU behavior behind the regression: installing /dev/stdin
  # directly over an existing destination fails after install unlinks it.
  # The production writer avoids that case by targeting a removed .new file.
  if [[ "$previous" == /dev/stdin ]]; then
    [[ ! -e "$destination" ]] || exit 71
    cat >"$destination"
  else
    cp "$previous" "$destination"
  fi
  [[ -z "$mode" ]] || chmod "$mode" "$destination"
  exit 0
fi
exec "$@"
EOF

chmod +x "${FAKE_BIN}/ssh-keygen" "${FAKE_BIN}/ssh" "${FAKE_BIN}/stat" \
  "${FAKE_BIN}/sudo" "${FAKE_BIN}/docker" "${FAKE_BIN}/ss" "${FAKE_BIN}/systemctl"

pass() { PASS=$((PASS + 1)); }
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  FAIL=$((FAIL + 1))
}

assert_eq() { # want got context
  local want="$1" got="$2" context="$3"
  if [[ "$want" == "$got" ]]; then
    pass
  else
    fail "${context}: want '${want}', got '${got}'"
  fi
}

assert_contains() { # haystack needle context
  local haystack="$1" needle="$2" context="$3"
  if [[ "$haystack" == *"$needle"* ]]; then
    pass
  else
    fail "${context}: missing '${needle}'"
  fi
}

assert_not_contains() { # haystack needle context
  local haystack="$1" needle="$2" context="$3"
  if [[ "$haystack" != *"$needle"* ]]; then
    pass
  else
    fail "${context}: unexpectedly contains '${needle}'"
  fi
}

assert_success() { # rc context
  if [[ "$1" -eq 0 ]]; then
    pass
  else
    fail "$2: exit status $1; output: $(<"$OUTPUT")"
  fi
}

assert_failure() { # rc context
  if [[ "$1" -ne 0 ]]; then
    pass
  else
    fail "$2: unexpectedly succeeded"
  fi
}

new_capture() { # name
  SSH_CAPTURE_DIR="${TEST_TMP}/capture-$1"
  STAT_LOG="${SSH_CAPTURE_DIR}/stat.log"
  mkdir -p "$SSH_CAPTURE_DIR"
  : >"$STAT_LOG"
  export SSH_CAPTURE_DIR STAT_LOG
}

run_sidecar() { # viewer-header-or-__unset__ stat-scenario
  local viewer_header="$1" stat_scenario="$2"
  (
    export PATH="${FAKE_BIN}:${ORIGINAL_PATH}"
    export OPENRUNG_SSH_KEY="$SSH_KEY"
    export OPENRUNG_KNOWN_HOSTS="$KNOWN_HOSTS"
    export OPENRUNG_WSS_TICKET_PUBLIC_KEYS_FILE="$TICKET_FILE"
    export OPENRUNG_WSS_ORIGIN_TOKENS_FILE="$TOKENS_FILE"
    export STAT_SCENARIO="$stat_scenario"
    if [[ "$viewer_header" == __unset__ ]]; then
      unset OPENRUNG_WSS_VIEWER_ADDRESS_HEADER
    else
      export OPENRUNG_WSS_VIEWER_ADDRESS_HEADER="$viewer_header"
    fi
    "$SCRIPT" sidecar relay-a relay.example.test \
      relay_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bunny-a example.test/openrung:latest
  ) >"$OUTPUT" 2>&1
}

run_origin_tls() {
  (
    export PATH="${FAKE_BIN}:${ORIGINAL_PATH}"
    export OPENRUNG_SSH_KEY="$SSH_KEY"
    export OPENRUNG_KNOWN_HOSTS="$KNOWN_HOSTS"
    "$SCRIPT" origin-tls relay-a relay.example.test origin.example.test
  ) >"$OUTPUT" 2>&1
}

prepare_audit_root() { # name front-url viewer-header-lines-or-__unset__
  local name="$1" front_url="$2" viewer_headers="$3"
  AUDIT_ROOT="${TEST_TMP}/audit-$name"
  mkdir -p "${AUDIT_ROOT}/etc/openrung" "${AUDIT_ROOT}/etc/caddy"
  {
    printf 'OPENRUNG_WSS_FRONTS=front-a=%s\n' "$front_url"
  } >"${AUDIT_ROOT}/etc/openrung/relay.env"
  : >"${AUDIT_ROOT}/etc/openrung/relay.env.pre-wss-advertise"
  {
    printf 'OPENRUNG_WSS_MAX_SESSIONS_PER_SOURCE=512\n'
    if [[ "$viewer_headers" != __unset__ ]]; then
      printf '%s\n' "$viewer_headers"
    fi
  } >"${AUDIT_ROOT}/etc/openrung/wss.env"
  printf 'reverse_proxy 127.0.0.1:8081\n' >"${AUDIT_ROOT}/etc/caddy/Caddyfile"
  export AUDIT_ROOT
}

run_audit() { # front-url
  local front_url="$1"
  (
    export PATH="${FAKE_BIN}:${ORIGINAL_PATH}"
    export OPENRUNG_SSH_KEY="$SSH_KEY"
    export OPENRUNG_KNOWN_HOSTS="$KNOWN_HOSTS"
    export SSH_EXECUTE_REMOTE_AUDIT=1
    export AUDIT_FAKE_BIN="$FAKE_BIN"
    export AUDIT_ORIGINAL_PATH="$ORIGINAL_PATH"
    export AUDIT_IMAGE=example.test/openrung:latest
    "$SCRIPT" audit relay-a relay.example.test "$AUDIT_IMAGE" front-a "$front_url"
  ) >"$OUTPUT" 2>&1
}

exercise_writer_twice() { # command source-dir mapped-dir destination-name context
  local command="$1" source_dir="$2" mapped_dir="$3" destination="$4" context="$5"
  local mapped_command rc content
  mkdir -p "$mapped_dir"
  mapped_command="${command//$source_dir/$mapped_dir}"

  if printf 'first payload\n' | PATH="${FAKE_BIN}:${ORIGINAL_PATH}" bash -c "$mapped_command" >"$OUTPUT" 2>&1; then
    rc=0
  else
    rc=$?
  fi
  assert_success "$rc" "${context} first write"
  content="$(<"${mapped_dir}/${destination}")"
  assert_eq 'first payload' "$content" "${context} first payload"

  if printf 'replacement payload\n' | PATH="${FAKE_BIN}:${ORIGINAL_PATH}" bash -c "$mapped_command" >"$OUTPUT" 2>&1; then
    rc=0
  else
    rc=$?
  fi
  assert_success "$rc" "${context} replacement write"
  content="$(<"${mapped_dir}/${destination}")"
  assert_eq 'replacement payload' "$content" "${context} replacement payload"
  if [[ ! -e "${mapped_dir}/${destination}.new" ]]; then
    pass
  else
    fail "${context}: sibling temporary remains after replacement"
  fi
}

test_sidecar_writer_and_default_header() {
  local rc payload command
  new_capture sidecar-default
  if run_sidecar __unset__ bsd; then rc=0; else rc=$?; fi
  assert_success "$rc" "sidecar with default viewer header"
  payload="$(<"${SSH_CAPTURE_DIR}/stdin.1")"
  assert_not_contains "$payload" 'OPENRUNG_WSS_VIEWER_ADDRESS_HEADER=' \
    "unset viewer header preserves sidecar default"

  command="$(<"${SSH_CAPTURE_DIR}/command.1")"
  assert_contains "$command" \
    'sudo rm -f /etc/openrung/wss.env.new && sudo install -m 0600 -o root -g root /dev/stdin /etc/openrung/wss.env.new && sudo mv /etc/openrung/wss.env.new /etc/openrung/wss.env' \
    "sidecar atomic writer construction"
  exercise_writer_twice "$command" /etc/openrung "${TEST_TMP}/remote-openrung" wss.env \
    "sidecar environment writer"
}

test_viewer_header_emission_and_validation() {
  local rc payload count
  new_capture sidecar-viewer-header
  if run_sidecar X-Viewer-Address bsd; then rc=0; else rc=$?; fi
  assert_success "$rc" "sidecar with explicit viewer header"
  payload="$(<"${SSH_CAPTURE_DIR}/stdin.1")"
  assert_contains "$payload" 'OPENRUNG_WSS_VIEWER_ADDRESS_HEADER=X-Viewer-Address' \
    "explicit viewer header emission"
  count="$(grep -c '^OPENRUNG_WSS_VIEWER_ADDRESS_HEADER=X-Viewer-Address$' "${SSH_CAPTURE_DIR}/stdin.1")"
  assert_eq 1 "$count" "viewer header emitted exactly once"

  new_capture sidecar-invalid-viewer-header
  if run_sidecar 'X Viewer Address' bsd; then rc=0; else rc=$?; fi
  assert_failure "$rc" "invalid viewer header"
  assert_contains "$(<"$OUTPUT")" 'viewer address header name is invalid' \
    "invalid viewer header diagnostic"
  if [[ ! -e "${SSH_CAPTURE_DIR}/counter" ]]; then
    pass
  else
    fail "invalid viewer header reached ssh"
  fi
}

test_origin_tls_writer_is_idempotent() {
  local rc command payload
  new_capture origin-tls
  if run_origin_tls; then rc=0; else rc=$?; fi
  assert_success "$rc" "origin TLS setup"
  command="$(<"${SSH_CAPTURE_DIR}/command.2")"
  assert_contains "$command" \
    'sudo rm -f /etc/caddy/Caddyfile.new && sudo install -m 0644 -o root -g root /dev/stdin /etc/caddy/Caddyfile.new && sudo mv /etc/caddy/Caddyfile.new /etc/caddy/Caddyfile' \
    "Caddyfile atomic writer construction"
  payload="$(<"${SSH_CAPTURE_DIR}/stdin.2")"
  assert_contains "$payload" 'https://origin.example.test:8443 {' "Caddyfile payload"
  exercise_writer_twice "$command" /etc/caddy "${TEST_TMP}/remote-caddy" Caddyfile \
    "Caddyfile writer"
}

test_bsd_stat_probe_stops_after_success() {
  local rc bsd_count gnu_count
  new_capture stat-bsd
  if run_sidecar __unset__ bsd; then rc=0; else rc=$?; fi
  assert_success "$rc" "BSD stat probe"
  bsd_count="$(grep -c '^-f %Lp ' "$STAT_LOG" || true)"
  gnu_count="$(grep -c '^-c %a ' "$STAT_LOG" || true)"
  assert_eq 2 "$bsd_count" "BSD stat probes both secret files"
  assert_eq 0 "$gnu_count" "successful BSD stat does not invoke GNU fallback"
}

test_gnu_stat_fallback_discards_failed_probe_output() {
  local rc bsd_count gnu_count
  new_capture stat-gnu
  if run_sidecar __unset__ gnu; then rc=0; else rc=$?; fi
  assert_success "$rc" "GNU stat fallback after noisy failed BSD probe"
  bsd_count="$(grep -c '^-f %Lp ' "$STAT_LOG" || true)"
  gnu_count="$(grep -c '^-c %a ' "$STAT_LOG" || true)"
  assert_eq 2 "$bsd_count" "GNU host attempts BSD probe for both files"
  assert_eq 2 "$gnu_count" "GNU host checks both files with GNU fallback"
}

test_stat_probe_fails_closed() {
  local rc
  new_capture stat-fail
  if run_sidecar __unset__ fail; then rc=0; else rc=$?; fi
  assert_failure "$rc" "failed stat probes"
  assert_contains "$(<"$OUTPUT")" 'could not inspect sidecar input file permissions' \
    "failed stat probes diagnostic"
  if [[ ! -e "${SSH_CAPTURE_DIR}/counter" ]]; then
    pass
  else
    fail "failed stat probes reached ssh"
  fi

  new_capture stat-world-readable
  if run_sidecar __unset__ unreadable-mode; then rc=0; else rc=$?; fi
  assert_failure "$rc" "world-readable secret input"
  assert_contains "$(<"$OUTPUT")" 'sidecar input files must have mode 0600' \
    "world-readable secret diagnostic"
  if [[ ! -e "${SSH_CAPTURE_DIR}/counter" ]]; then
    pass
  else
    fail "world-readable secret reached ssh"
  fi
}

test_provider_specific_viewer_header_audit() {
  local bunny_url='wss://front-a.b-cdn.net/api/v1/wss-bridge'
  local cloudfront_url='wss://aaaaaaaaaaaaaa.cloudfront.net/api/v1/wss-bridge'
  local rc

  new_capture audit-bunny-exact
  prepare_audit_root bunny-exact "$bunny_url" \
    'OPENRUNG_WSS_VIEWER_ADDRESS_HEADER=X-OpenRung-Viewer-Address'
  if run_audit "$bunny_url"; then rc=0; else rc=$?; fi
  assert_success "$rc" "Bunny audit with exact viewer header"

  new_capture audit-bunny-missing
  prepare_audit_root bunny-missing "$bunny_url" __unset__
  if run_audit "$bunny_url"; then rc=0; else rc=$?; fi
  assert_failure "$rc" "Bunny audit without viewer header"

  new_capture audit-bunny-wrong
  prepare_audit_root bunny-wrong "$bunny_url" \
    'OPENRUNG_WSS_VIEWER_ADDRESS_HEADER=CloudFront-Viewer-Address'
  if run_audit "$bunny_url"; then rc=0; else rc=$?; fi
  assert_failure "$rc" "Bunny audit with wrong viewer header"

  new_capture audit-bunny-duplicate
  prepare_audit_root bunny-duplicate "$bunny_url" \
    $'OPENRUNG_WSS_VIEWER_ADDRESS_HEADER=X-OpenRung-Viewer-Address\nOPENRUNG_WSS_VIEWER_ADDRESS_HEADER=X-OpenRung-Viewer-Address'
  if run_audit "$bunny_url"; then rc=0; else rc=$?; fi
  assert_failure "$rc" "Bunny audit with duplicate viewer headers"

  new_capture audit-cloudfront-default
  prepare_audit_root cloudfront-default "$cloudfront_url" __unset__
  if run_audit "$cloudfront_url"; then rc=0; else rc=$?; fi
  assert_success "$rc" "CloudFront audit using sidecar default viewer header"

  new_capture audit-cloudfront-override
  prepare_audit_root cloudfront-override "$cloudfront_url" \
    'OPENRUNG_WSS_VIEWER_ADDRESS_HEADER=CloudFront-Viewer-Address'
  if run_audit "$cloudfront_url"; then rc=0; else rc=$?; fi
  assert_failure "$rc" "CloudFront audit with explicit viewer header override"
}

test_sidecar_writer_and_default_header
test_viewer_header_emission_and_validation
test_origin_tls_writer_is_idempotent
test_bsd_stat_probe_stops_after_success
test_gnu_stat_fallback_discards_failed_probe_output
test_stat_probe_fails_closed
test_provider_specific_viewer_header_audit

if [[ "$FAIL" -ne 0 ]]; then
  printf '%s assertions passed, %s failed\n' "$PASS" "$FAIL" >&2
  exit 1
fi
printf '%s assertions passed\n' "$PASS"
