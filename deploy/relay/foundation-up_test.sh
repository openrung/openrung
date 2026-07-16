#!/usr/bin/env bash
# SC2329 ("function is never invoked") is disabled file-wide: nearly every test
# here defines mock functions that shadow the real ones and are called only
# indirectly, from the foundation-up.sh code under test. That is the harness's
# whole design, so the check fires on every mock and cannot distinguish a
# correct one from a mock whose name has drifted out of sync with the script.
# shellcheck disable=SC2329
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="${HERE}/foundation-up.sh"

# shellcheck source=foundation-up.sh
source "$SCRIPT"
set +e

reset_script_functions() {
  # Function mocks are global in bash; re-source between test groups so one
  # failure-injection setup cannot contaminate the next group.
  # shellcheck source=foundation-up.sh
  source "$SCRIPT"
  set +e
}

TEST_TMP="$(mktemp -d)"
trap 'rm -rf "$TEST_TMP"' EXIT
MUTATION_LOG="${TEST_TMP}/mutations"
OUTPUT="${TEST_TMP}/output"
PASS=0
FAIL=0

pass() { PASS=$((PASS + 1)); }
fail() {
  echo "FAIL: $*" >&2
  FAIL=$((FAIL + 1))
}

assert_eq() { # want got context
  local want="$1" got="$2" context="$3"
  if [ "$want" = "$got" ]; then pass; else fail "${context}: want '${want}', got '${got}'"; fi
}

assert_contains() { # haystack needle context
  local haystack="$1" needle="$2" context="$3"
  if [[ "$haystack" == *"$needle"* ]]; then pass; else fail "${context}: missing '${needle}'"; fi
}

assert_before() { # haystack first second context
  local haystack="$1" first="$2" second="$3" context="$4"
  if [[ "$haystack" == *"$first"*"$second"* ]]; then
    pass
  else
    fail "${context}: '${first}' does not precede '${second}'"
  fi
}

state_from_mask() {
  local mask="$1" i out=""
  for i in 0 1 2 3 4 5 6 7 8 9 10; do
    out="${out}${out:+ }$(( (mask >> i) & 1 ))"
  done
  printf '%s' "$out"
}

expected_clean() { # mode state
  local mode="$1" state="$2"
  local live live_running old old_running new legacy legacy_old env legacy_env tmp unexpected
  read -r live live_running old old_running new legacy legacy_old env legacy_env tmp unexpected <<< "$state"
  [ "$live" = 1 ] && [ "$live_running" = 1 ] \
    && [ "$old" = 0 ] && [ "$old_running" = 0 ] && [ "$new" = 0 ] \
    && [ "$legacy" = 0 ] && [ "$legacy_old" = 0 ] \
    && [ "$legacy_env" = 0 ] && [ "$tmp" = 0 ] && [ "$unexpected" = 0 ] \
    && { [ "$mode" != update ] || [ "$env" = 1 ]; }
}

expected_rollback() { # state
  local state="$1"
  local live live_running old old_running new legacy legacy_old env legacy_env tmp unexpected
  read -r live live_running old old_running new legacy legacy_old env legacy_env tmp unexpected <<< "$state"
  [ "$live" = 0 ] && [ "$live_running" = 0 ] \
    && [ "$old" = 1 ] && [ "$old_running" = 0 ] \
    && [ "$legacy" = 0 ] && [ "$legacy_old" = 0 ] \
    && [ "$legacy_env" = 0 ] && [ "$tmp" = 0 ] && [ "$unexpected" = 0 ]
}

install_matrix_mocks() {
  pin_host_key() { :; }
  print_manual_state_help() { :; }
  load_token() {
    TOKEN="test-token"
    unset OPENRUNG_FOUNDATION_TOKEN OPENRUNG_FOUNDATION_TOKEN_CMD
  }
  inspect_host_state() { printf '%s' "$MOCK_STATE"; }
  require_env_has_token() { :; }
  convert_host() { printf 'convert\n' >> "$MUTATION_LOG"; }
  prepare_image() { printf 'pull\n' >> "$MUTATION_LOG"; }
  roll_host() { printf 'roll\n' >> "$MUTATION_LOG"; }
  rollback_host() { printf 'rollback\n' >> "$MUTATION_LOG"; }
}

run_matrix() { # mode
  local mode="$1" mask state expected rc mutations
  for ((mask=0; mask<2048; mask++)); do
    state="$(state_from_mask "$mask")"
    MOCK_STATE="$state"
    : > "$MUTATION_LOG"
    if "cmd_${mode}" 203.0.113.10 >"$OUTPUT" 2>&1; then rc=0; else rc=$?; fi
    mutations="$(wc -l < "$MUTATION_LOG" | tr -d '[:space:]')"
    expected=1
    if [ "$mode" = rollback ]; then
      expected_rollback "$state" && expected=0
    else
      expected_clean "$mode" "$state" && expected=0
    fi
    if [ "$expected" = 0 ]; then
      [ "$rc" = 0 ] || fail "${mode} rejected supported state ${state}"
      [ "$mutations" -gt 0 ] || fail "${mode} did not reach mutation for supported state ${state}"
    else
      [ "$rc" != 0 ] || fail "${mode} accepted unsupported state ${state}"
      [ "$mutations" = 0 ] || fail "${mode} mutated rejected state ${state}"
    fi
  done
  pass
}

test_state_matrix() {
  reset_script_functions
  install_matrix_mocks
  run_matrix convert
  run_matrix update
  run_matrix rollback
}

test_fleet_preflight_is_all_or_nothing() {
  reset_script_functions
  local token_log="${TEST_TMP}/token-reads"
  pin_host_key() { :; }
  load_token() { printf 'read\n' >> "$token_log"; TOKEN="test-token"; }
  inspect_host_state() {
    if [ "$1" = 203.0.113.11 ]; then
      printf '1 1 0 0 0 1 0 1 0 0 0' # second host has a legacy container
    else
      printf '1 1 0 0 0 0 0 1 0 0 0'
    fi
  }
  convert_host() { printf 'convert\n' >> "$MUTATION_LOG"; }
  : > "$MUTATION_LOG"
  : > "$token_log"
  if (set -e; cmd_convert 203.0.113.10 203.0.113.11) >"$OUTPUT" 2>&1; then
    fail "fleet convert accepted a legacy second host"
  else
    pass
  fi
  assert_eq 0 "$(wc -l < "$MUTATION_LOG" | tr -d '[:space:]')" "fleet preflight mutation count"
  assert_eq 0 "$(wc -l < "$token_log" | tr -d '[:space:]')" "rejected fleet does not invoke token source"
  assert_contains "$(<"$OUTPUT")" "manual migration required" "fleet preflight guidance"
}

test_token_export_is_removed() {
  reset_script_functions
  local out rc
  if out="$(TOKEN=preexisting OPENRUNG_FOUNDATION_TOKEN=foundation-secret bash -c '
      export TOKEN OPENRUNG_FOUNDATION_TOKEN
      source "$1"
      load_token
      declare -p TOKEN
      if env | grep -q "^TOKEN="; then exit 9; fi
    ' bash "$SCRIPT" 2>&1)"; then rc=0; else rc=$?; fi
  assert_eq 0 "$rc" "token export regression"
  assert_contains "$out" 'declare -- TOKEN="foundation-secret"' "TOKEN is a non-exported shell variable"
}

test_update_unsets_unused_token_sources() {
  reset_script_functions
  MOCK_STATE='1 1 0 0 0 0 0 1 0 0 0'
  inspect_host_state() { printf '%s' "$MOCK_STATE"; }
  require_env_has_token() { :; }
  pin_host_key() {
    if env | grep -q '^OPENRUNG_FOUNDATION_TOKEN='; then
      echo leaked >&2
      return 1
    fi
  }
  prepare_image() { :; }
  roll_host() { :; }
  # SC2030/SC2031: confining the token to this subshell is the point -- the test
  # asserts cmd_update unexports it before spawning preflight children, and the
  # subshell keeps the secret out of the rest of the harness either way.
  # shellcheck disable=SC2030
  if (set -e; OPENRUNG_FOUNDATION_TOKEN=unused-secret; export OPENRUNG_FOUNDATION_TOKEN; cmd_update 203.0.113.10) >"$OUTPUT" 2>&1; then
    pass
  else
    fail "update leaked an unused token source to preflight children"
  fi
}

test_convert_unexports_token_before_preflight() {
  reset_script_functions
  inspect_host_state() { printf '1 1 0 0 0 0 0 1 0 0 0'; }
  pin_host_key() {
    if env | grep -q '^OPENRUNG_FOUNDATION_TOKEN='; then
      echo leaked >&2
      return 1
    fi
  }
  # shellcheck disable=SC2031
  load_token() { TOKEN="$OPENRUNG_FOUNDATION_TOKEN"; unset OPENRUNG_FOUNDATION_TOKEN; }
  convert_host() { :; }
  if (set -e; OPENRUNG_FOUNDATION_TOKEN=foundation-secret; export OPENRUNG_FOUNDATION_TOKEN; cmd_convert 203.0.113.10) >"$OUTPUT" 2>&1; then
    pass
  else
    fail "convert leaked its token source to preflight children"
  fi
}

test_roll_order_and_failure_boundaries() {
  reset_script_functions
  local got rc
  begin_roll() { printf 'begin\n' >> "$MUTATION_LOG"; }
  run_candidate() { printf 'run:%s\n' "$2" >> "$MUTATION_LOG"; }
  verify_foundation() { printf 'verify:%s\n' "$2" >> "$MUTATION_LOG"; }
  commit_candidate() { printf 'commit\n' >> "$MUTATION_LOG"; }
  : > "$MUTATION_LOG"
  (set -e; roll_host 203.0.113.10 /etc/openrung/relay.env)
  got="$(<"$MUTATION_LOG")"
  assert_eq $'begin\nrun:/etc/openrung/relay.env\nverify:openrung-relay-new\ncommit' "$got" "candidate transaction order"

  run_candidate() { printf 'run\n' >> "$MUTATION_LOG"; return 1; }
  : > "$MUTATION_LOG"
  if (set -e; roll_host 203.0.113.10 /etc/openrung/relay.env) >/dev/null 2>&1; then rc=0; else rc=$?; fi
  assert_eq 1 "$rc" "candidate start failure return code"
  got="$(<"$MUTATION_LOG")"
  assert_eq $'begin\nrun' "$got" "candidate start failure stops transaction"

  run_candidate() { printf 'run\n' >> "$MUTATION_LOG"; }
  verify_foundation() { printf 'verify\n' >> "$MUTATION_LOG"; return 1; }
  : > "$MUTATION_LOG"
  if (set -e; roll_host 203.0.113.10 /etc/openrung/relay.env) >/dev/null 2>&1; then rc=0; else rc=$?; fi
  assert_eq 1 "$rc" "verification failure return code"
  got="$(<"$MUTATION_LOG")"
  assert_eq $'begin\nrun\nverify' "$got" "verification failure cannot commit candidate"
}

test_convert_prepares_image_before_env_write() {
  reset_script_functions
  preflight_host() { :; }
  prepare_image() { printf 'pull\n' >> "$MUTATION_LOG"; }
  stage_preserved_env() { printf 'stage\n' >> "$MUTATION_LOG"; }
  ssh_run() { printf 'public_host=203.0.113.10\nlabel=test\n'; }
  install_env_file() { printf 'install\n' >> "$MUTATION_LOG"; }
  roll_host() { printf 'roll\n' >> "$MUTATION_LOG"; }
  : > "$MUTATION_LOG"
  (set -e; convert_host 203.0.113.10) >"$OUTPUT" 2>&1
  assert_eq $'pull\nstage\ninstall\nroll' "$(<"$MUTATION_LOG")" "convert mutation order"
}

# Execute the script's real multiline remote commands against a tiny file-backed
# Docker model. This checks command ordering and state transitions rather than
# merely checking which shell helper was called.
test_real_transaction_and_rollback_commands() {
  reset_script_functions
  local sim_dir="${TEST_TMP}/docker-state" sim_log="${TEST_TMP}/docker-mutations" rc before
  SIM_DIR="$sim_dir"
  SIM_LOG="$sim_log"
  ENV_FILE="${TEST_TMP}/relay.env"
  LEGACY_ENV_FILE="${TEST_TMP}/volunteer.env"
  export SIM_DIR SIM_LOG

  sudo() { "$@"; }
  docker() {
    local op="${1:-}" fmt="" name="" src="" dst="" running restart registered path all=0 arg
    [ "$#" -gt 0 ] && shift
    case "$op" in
      inspect)
        if [ "${1:-}" = -f ] || [ "${1:-}" = --format ]; then
          fmt="$2"
          shift 2
        fi
        name="${1:-}"
        [ "${SIM_FAIL_INSPECT_NAME:-}" != "$name" ] || return 1
        [ -f "${SIM_DIR}/${name}" ] || return 1
        read -r running restart registered < "${SIM_DIR}/${name}"
        if [[ "$fmt" == *RestartCount* ]]; then
          printf '%s %s\n' "$running" "$restart"
        elif [[ "$fmt" == *State.Running* ]]; then
          printf '%s\n' "$running"
        fi
        ;;
      pull)
        printf 'pull %s\n' "${1:-}" >> "$SIM_LOG"
        [ "${SIM_FAIL_OP:-}" != pull ]
        ;;
      stop)
        name="${1:-}"
        printf 'stop %s\n' "$name" >> "$SIM_LOG"
        [ "${SIM_FAIL_OP:-}" != stop ] || return 1
        [ -f "${SIM_DIR}/${name}" ] || return 1
        read -r running restart registered < "${SIM_DIR}/${name}"
        printf 'false %s %s\n' "$restart" "$registered" > "${SIM_DIR}/${name}"
        ;;
      rename)
        src="${1:-}"; dst="${2:-}"
        printf 'rename %s %s\n' "$src" "$dst" >> "$SIM_LOG"
        [ "${SIM_FAIL_OP:-}" != rename ] || return 1
        [ -f "${SIM_DIR}/${src}" ] && [ ! -e "${SIM_DIR}/${dst}" ] || return 1
        mv "${SIM_DIR}/${src}" "${SIM_DIR}/${dst}"
        ;;
      run)
        while [ "$#" -gt 0 ]; do
          if [ "$1" = --name ]; then name="$2"; shift 2; else shift; fi
        done
        printf 'run %s\n' "$name" >> "$SIM_LOG"
        [ "${SIM_FAIL_OP:-}" != run ] || return 1
        [ -n "$name" ] && [ ! -e "${SIM_DIR}/${name}" ] || return 1
        registered=yes
        [ "${SIM_NO_REGISTER:-}" != 1 ] || registered=no
        printf 'true 0 %s\n' "$registered" > "${SIM_DIR}/${name}"
        ;;
      logs)
        name="${1:-}"
        [ -f "${SIM_DIR}/${name}" ] || return 1
        read -r running restart registered < "${SIM_DIR}/${name}"
        [ "$registered" = yes ] && printf 'registered relay (simulated)\n'
        ;;
      ps)
        for arg in "$@"; do [ "$arg" = -a ] && all=1; done
        for path in "${SIM_DIR}"/*; do
          [ -f "$path" ] || continue
          read -r running restart registered < "$path"
          if [ "$all" = 1 ] || [ "$running" = true ]; then
            printf '%s\n' "${path##*/}"
          fi
        done
        ;;
      rm)
        [ "${1:-}" = -f ] && shift
        name="${1:-}"
        printf 'rm %s\n' "$name" >> "$SIM_LOG"
        [ "${SIM_FAIL_OP:-}" != rm ] || return 1
        [ -f "${SIM_DIR}/${name}" ] || return 1
        rm "${SIM_DIR}/${name}"
        ;;
      start)
        name="${1:-}"
        printf 'start %s\n' "$name" >> "$SIM_LOG"
        [ "${SIM_FAIL_OP:-}" != start ] || return 1
        [ -f "${SIM_DIR}/${name}" ] || return 1
        read -r running restart registered < "${SIM_DIR}/${name}"
        printf 'true %s %s\n' "$restart" "$registered" > "${SIM_DIR}/${name}"
        ;;
      *) return 2 ;;
    esac
  }
  export -f sudo docker

  ssh_run() {
    local host="$1"; shift
    SIM_FAIL_OP="${SIM_FAIL_OP:-}" SIM_NO_REGISTER="${SIM_NO_REGISTER:-}" \
      SIM_FAIL_INSPECT_NAME="${SIM_FAIL_INSPECT_NAME:-}" bash -c "$*"
  }
  sim_reset() {
    rm -rf "$SIM_DIR"
    mkdir -p "$SIM_DIR"
    : > "$SIM_LOG"
    printf 'OPENRUNG_FOUNDATION_TOKEN=simulated\n' > "$ENV_FILE"
    rm -f "$LEGACY_ENV_FILE" "${ENV_FILE}.tmp"
    unset SIM_FAIL_OP SIM_NO_REGISTER SIM_FAIL_INSPECT_NAME
  }
  sim_put() { printf '%s 0 %s\n' "$2" "${3:-yes}" > "${SIM_DIR}/$1"; }
  sim_running() {
    local running
    read -r running _ < "${SIM_DIR}/$1"
    printf '%s' "$running"
  }
  sim_snapshot() {
    for path in "${SIM_DIR}"/*; do
      [ -f "$path" ] || continue
      printf '%s ' "${path##*/}"
      cksum "$path"
    done | sort
  }

  # The actual begin/run/verify/commit commands return to the sole steady state.
  sim_reset
  sim_put "$CONTAINER" true
  if roll_host 203.0.113.10 "$ENV_FILE" >"$OUTPUT" 2>&1; then rc=0; else rc=$?; fi
  assert_eq 0 "$rc" "real candidate transaction succeeds"
  assert_eq true "$(sim_running "$CONTAINER")" "committed live container is running"
  # SC2319 on the next four assertions: `$?` after a `[ ... ] && [ ... ]` list
  # is the status of the list itself -- 0 only when every path test holds --
  # which is exactly the value being compared. Intentional, not a stray `$?`.
  # shellcheck disable=SC2319
  assert_eq 0 "$([ ! -e "${SIM_DIR}/${OLD_CONTAINER}" ] && [ ! -e "${SIM_DIR}/${NEW_CONTAINER}" ]; echo $?)" "success removes old and candidate names"
  assert_before "$(<"$SIM_LOG")" "rename ${CONTAINER} ${OLD_CONTAINER}" "run ${NEW_CONTAINER}" "candidate starts after live becomes old"
  assert_before "$(<"$SIM_LOG")" "rename ${NEW_CONTAINER} ${CONTAINER}" "rm ${OLD_CONTAINER}" "backup removal follows promotion"

  # A run failure leaves old-only; the real rollback command restores it.
  sim_reset
  sim_put "$CONTAINER" true
  SIM_FAIL_OP="run"
  if roll_host 203.0.113.10 "$ENV_FILE" >"$OUTPUT" 2>&1; then rc=0; else rc=$?; fi
  assert_eq 1 "$rc" "candidate start failure propagates"
  # shellcheck disable=SC2319
  assert_eq 0 "$([ -e "${SIM_DIR}/${OLD_CONTAINER}" ] && [ ! -e "${SIM_DIR}/${CONTAINER}" ]; echo $?)" "run failure leaves stopped old only"
  unset SIM_FAIL_OP
  if rollback_host 203.0.113.10 >"$OUTPUT" 2>&1; then rc=0; else rc=$?; fi
  assert_eq 0 "$rc" "old-only rollback succeeds"
  assert_eq true "$(sim_running "$CONTAINER")" "old-only rollback restarts live"

  # A verification failure leaves old+new; rollback removes only the candidate.
  sim_reset
  sim_put "$CONTAINER" true
  verify_foundation() { return 1; }
  if roll_host 203.0.113.10 "$ENV_FILE" >"$OUTPUT" 2>&1; then rc=0; else rc=$?; fi
  assert_eq 1 "$rc" "verification failure propagates"
  # shellcheck disable=SC2319
  assert_eq 0 "$([ -e "${SIM_DIR}/${OLD_CONTAINER}" ] && [ -e "${SIM_DIR}/${NEW_CONTAINER}" ] && [ ! -e "${SIM_DIR}/${CONTAINER}" ]; echo $?)" "verification failure leaves exact rollback state"
  if rollback_host 203.0.113.10 >"$OUTPUT" 2>&1; then rc=0; else rc=$?; fi
  assert_eq 0 "$rc" "old+new rollback succeeds"
  assert_eq true "$(sim_running "$CONTAINER")" "old+new rollback restarts live"

  # Cleanup failure leaves live+old. That ambiguous backup is then read-only.
  sim_reset
  sim_put "$OLD_CONTAINER" false
  sim_put "$NEW_CONTAINER" true
  SIM_FAIL_OP="rm"
  if commit_candidate 203.0.113.10 >"$OUTPUT" 2>&1; then rc=0; else rc=$?; fi
  assert_eq 1 "$rc" "backup cleanup failure propagates"
  # shellcheck disable=SC2319
  assert_eq 0 "$([ -e "${SIM_DIR}/${CONTAINER}" ] && [ -e "${SIM_DIR}/${OLD_CONTAINER}" ] && [ ! -e "${SIM_DIR}/${NEW_CONTAINER}" ]; echo $?)" "cleanup failure leaves visible live plus old"
  unset SIM_FAIL_OP
  : > "$SIM_LOG"
  before="$(sim_snapshot)"
  if preflight_host 203.0.113.10 update >"$OUTPUT" 2>&1; then rc=0; else rc=$?; fi
  assert_eq 1 "$rc" "live plus backup is rejected"
  assert_eq 0 "$(wc -l < "$SIM_LOG" | tr -d '[:space:]')" "ambiguous backup triggers no Docker mutation"
  assert_eq "$before" "$(sim_snapshot)" "ambiguous backup state stays unchanged"

  # Existence comes from docker ps -a, not a second fallible inspect. A backup
  # listed in the snapshot therefore cannot disappear from the policy decision.
  sim_reset
  sim_put "$CONTAINER" true
  sim_put "$OLD_CONTAINER" false
  SIM_FAIL_INSPECT_NAME="$OLD_CONTAINER"
  if preflight_host 203.0.113.10 convert >"$OUTPUT" 2>&1; then rc=0; else rc=$?; fi
  assert_eq 1 "$rc" "listed backup remains visible when its inspect fails"
  assert_eq 0 "$(wc -l < "$SIM_LOG" | tr -d '[:space:]')" "inspect failure cannot turn backup into a mutable clean state"
  if begin_roll 203.0.113.10 >"$OUTPUT" 2>&1; then rc=0; else rc=$?; fi
  assert_eq 1 "$rc" "mutation guard rejects backup directly from name snapshot"
  assert_eq 0 "$(wc -l < "$SIM_LOG" | tr -d '[:space:]')" "hidden backup cannot trigger stop or rename"
  unset SIM_FAIL_INSPECT_NAME

  # Unrecognized relay-prefixed backups are part of the refused state space,
  # even though they do not collide with the three canonical transaction names.
  sim_reset
  sim_put "$CONTAINER" true
  sim_put "${CONTAINER}-old-2026" false
  before="$(sim_snapshot)"
  if preflight_host 203.0.113.10 convert >"$OUTPUT" 2>&1; then rc=0; else rc=$?; fi
  assert_eq 1 "$rc" "unrecognized relay backup is rejected by real snapshot"
  assert_contains "$(<"$OUTPUT")" "unrecognized OpenRung relay container" "unexpected-container manual guidance"
  assert_eq 0 "$(wc -l < "$SIM_LOG" | tr -d '[:space:]')" "unexpected container preflight triggers no Docker mutation"
  assert_eq "$before" "$(sim_snapshot)" "unexpected container state stays unchanged"

  # The immediate mutation guard repeats the check in case such a container
  # appears after fleet preflight.
  : > "$SIM_LOG"
  if prepare_image 203.0.113.10 >"$OUTPUT" 2>&1; then rc=0; else rc=$?; fi
  assert_eq 1 "$rc" "unexpected container blocks image preparation"
  assert_eq 0 "$(wc -l < "$SIM_LOG" | tr -d '[:space:]')" "unexpected container blocks image pull"
  if begin_roll 203.0.113.10 >"$OUTPUT" 2>&1; then rc=0; else rc=$?; fi
  assert_eq 1 "$rc" "unexpected container blocks live-container swap"
  assert_eq 0 "$(wc -l < "$SIM_LOG" | tr -d '[:space:]')" "unexpected container blocks stop and rename"

  export -n -f sudo docker 2>/dev/null || true
  unset -f sudo docker
}

test_commit_rechecks_before_deleting_old() {
  reset_script_functions
  local calls rc command_log
  calls=0
  ssh_run() {
    calls=$((calls + 1))
    printf '%s\n' "$*" > "${TEST_TMP}/last-command"
    printf 'guard-promote\n' >> "$MUTATION_LOG"
    if [ "$calls" = 1 ]; then return 42; fi
    return 0
  }
  : > "$MUTATION_LOG"
  if commit_candidate 203.0.113.10 >"$OUTPUT" 2>&1; then rc=0; else rc=$?; fi
  assert_eq 1 "$rc" "commit refuses candidate that changed after verification"
  assert_eq 1 "$calls" "failed commit never reaches old deletion"
  assert_contains "$(<"${TEST_TMP}/last-command")" "RestartCount" "commit atomically rechecks candidate health"

  calls=0
  ssh_run() {
    calls=$((calls + 1))
    printf '%s\n' "$*" > "${TEST_TMP}/last-command"
    return 0
  }
  : > "$MUTATION_LOG"
  if commit_candidate 203.0.113.10 >"$OUTPUT" 2>&1; then rc=0; else rc=$?; fi
  assert_eq 0 "$rc" "healthy candidate commits"
  assert_eq 1 "$calls" "promotion and old removal share one guarded SSH transaction"
  command_log="$(<"${TEST_TMP}/last-command")"
  assert_before "$command_log" "docker rename ${NEW_CONTAINER} ${CONTAINER}" "docker rm -f ${OLD_CONTAINER}" "old removal follows guarded candidate promotion"
}

test_fail_closed_function_boundaries() {
  reset_script_functions
  inspect_host_state() { return 1; }
  if preflight_host 203.0.113.10 convert >"$OUTPUT" 2>&1; then
    fail "preflight swallowed a failed state snapshot"
  else
    pass
  fi

  read_token() { return 1; }
  if load_token >"$OUTPUT" 2>&1; then
    fail "load_token swallowed a failed token source"
  else
    pass
  fi
}

test_host_key_write_failure_is_fatal() {
  reset_script_functions
  local rc
  if (
    ssh-keygen() { return 1; }
    aws() {
      case "${2:-}" in
        get-instances) printf 'simulated-instance\n' ;;
        get-instance-access-details) printf 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAISimulated\n' ;;
        *) return 2 ;;
      esac
    }
    mkdir() { return 1; }
    HOME="${TEST_TMP}/unwritable-home"
    pin_host_key 203.0.113.10
  ) >"$OUTPUT" 2>&1; then rc=0; else rc=$?; fi
  assert_eq 1 "$rc" "host-key directory write failure propagates"
  assert_contains "$(<"$OUTPUT")" "host key was not pinned" "host-key write failure is explicit"
  if [[ "$(<"$OUTPUT")" == *"pinned 203.0.113.10"* ]]; then
    fail "host-key write failure falsely reported success"
  else
    pass
  fi
}

test_malformed_host_keys_cannot_enable_tofu() {
  reset_script_functions
  local rc
  if (
    ssh-keygen() { return 1; }
    aws() {
      case "${2:-}" in
        get-instances) printf 'simulated-instance\n' ;;
        get-instance-access-details) printf 'malformed-only\n' ;;
        *) return 2 ;;
      esac
    }
    HOME="${TEST_TMP}/malformed-key-home"
    pin_host_key 203.0.113.10
  ) >"$OUTPUT" 2>&1; then rc=0; else rc=$?; fi
  assert_eq 1 "$rc" "malformed host-key response propagates"
  assert_contains "$(<"$OUTPUT")" "no valid SSH host keys" "malformed host-key failure is explicit"
  if [[ "$(<"$OUTPUT")" == *"pinned 203.0.113.10"* ]]; then
    fail "malformed host-key response falsely reported success"
  else
    pass
  fi
}

test_manual_help_uses_configured_ssh() {
  reset_script_functions
  local out
  SSH_KEY="/tmp/key with spaces"
  SSH_OPTS=(-o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=/tmp/known_hosts -i "$SSH_KEY")
  out="$(print_manual_state_help 203.0.113.10 "ambiguous" 2>&1)"
  assert_contains "$out" "key\\ with\\ spaces" "manual help shell-quotes configured key"
  assert_contains "$out" "foundation-up.sh rollback 203.0.113.10" "manual help points to validated rollback"
  assert_contains "$out" "no relay container, env-file, image, backup, or candidate changes were made" "manual help states fail-closed behavior"
}

test_state_matrix
test_fleet_preflight_is_all_or_nothing
test_token_export_is_removed
test_update_unsets_unused_token_sources
test_convert_unexports_token_before_preflight
test_roll_order_and_failure_boundaries
test_convert_prepares_image_before_env_write
test_real_transaction_and_rollback_commands
test_commit_rechecks_before_deleting_old
test_fail_closed_function_boundaries
test_host_key_write_failure_is_fatal
test_malformed_host_keys_cannot_enable_tofu
test_manual_help_uses_configured_ssh

if [ "$FAIL" -ne 0 ]; then
  echo "foundation-up tests: ${PASS} passed, ${FAIL} failed" >&2
  exit 1
fi
echo "foundation-up tests: ${PASS} passed"
