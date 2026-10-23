#!/usr/bin/env bash
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="${HERE}/foundation-up.sh"

# shellcheck source=foundation-up.sh
source "$SCRIPT"
set +e

reset_script_functions() {
  # Function mocks are global in bash; re-source between test groups so one
  # failure-injection setup cannot contaminate the next group.
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
  apply_egress_limit() { printf 'egress\n' >> "$MUTATION_LOG"; }
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
  apply_egress_limit() { :; }
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
  apply_egress_limit() { printf 'egress\n' >> "$MUTATION_LOG"; }
  : > "$MUTATION_LOG"
  (set -e; convert_host 203.0.113.10) >"$OUTPUT" 2>&1
  assert_eq $'pull\nstage\ninstall\nroll\negress' "$(<"$MUTATION_LOG")" "convert mutation order"
}

# The host new-destination limit is loaded only after a verified roll, from
# the image that roll put in service, and never when the roll fails.
test_egress_limit_follows_verified_roll() {
  reset_script_functions
  pin_host_key() { :; }
  inspect_host_state() { printf '1 1 0 0 0 0 0 1 0 0 0'; }
  preflight_host() { :; }
  prepare_image() { printf 'pull\n' >> "$MUTATION_LOG"; }
  roll_host() { printf 'roll\n' >> "$MUTATION_LOG"; }
  apply_egress_limit() { printf 'egress\n' >> "$MUTATION_LOG"; }
  : > "$MUTATION_LOG"
  (set -e; cmd_update 203.0.113.10 203.0.113.11) >"$OUTPUT" 2>&1
  assert_eq $'pull\nroll\negress\npull\nroll\negress' "$(<"$MUTATION_LOG")" "update loads the limit after each verified roll"

  roll_host() { printf 'roll\n' >> "$MUTATION_LOG"; return 1; }
  : > "$MUTATION_LOG"
  if (set -e; cmd_update 203.0.113.10) >"$OUTPUT" 2>&1; then fail "update succeeded after a failed roll"; else pass; fi
  assert_eq $'pull\nroll' "$(<"$MUTATION_LOG")" "a failed roll never loads the limit"
}

test_new_dest_rate_validation() {
  reset_script_functions
  local rate
  for rate in 10 1 250 off; do
    if (NEW_DEST_RATE="$rate"; validate_config) >/dev/null 2>&1; then pass; else fail "rate ${rate} rejected"; fi
  done
  for rate in 0 010 -5 abc '10;reboot' ''; do
    if (NEW_DEST_RATE="$rate"; validate_config) >/dev/null 2>&1; then fail "rate '${rate}' accepted"; else pass; fi
  done
  if (NEW_DEST_RATE=off; cmd_audit 203.0.113.10) >/dev/null 2>&1; then fail "audit accepted rate off"; else pass; fi
}

# Run the exact remote command apply_egress_limit sends, against stubs, so the
# quoting, the container flags, and the install order are what reach the host.
# The stub docker hands out a fake egress-limit that records how the host runs it.
test_foundation_contact_is_managed_only_when_set() {
  reset_script_functions
  local contact
  for contact in ops@openrung.example ops+relay@sub.openrung.example; do
    if (FOUNDATION_CONTACT="$contact"; validate_config) >/dev/null 2>&1; then pass; else fail "contact ${contact} rejected"; fi
  done
  for contact in 'two words@example.org' 'no-at-sign' 'ops@.example.org' "ops@example.org;reboot" "\$(id)@example.org"; do
    if (FOUNDATION_CONTACT="$contact"; validate_config) >/dev/null 2>&1; then fail "contact '${contact}' accepted"; else pass; fi
  done

  FOUNDATION_CONTACT=""
  assert_eq '/^OPENRUNG_(BROKER_URL|FOUNDATION_TOKEN|VOLUNTEER_TOKEN|NODE_CLASS|XRAY_PATH|CONFIG_OUT)=/d' "$(preserve_drop)" "unset contact preserves the host's contact"
  FOUNDATION_CONTACT=ops@openrung.example
  assert_contains "$(preserve_drop)" '|OPERATOR_CONTACT)=' "set contact replaces the host's contact"

  local sent="$TEST_TMP/install-stdin"
  ssh_run() { cat > "$sent"; }
  TOKEN=test-token
  install_env_file 203.0.113.10
  assert_contains "$(<"$sent")" 'OPENRUNG_OPERATOR_CONTACT=ops@openrung.example' "convert writes the Foundation contact"
  FOUNDATION_CONTACT=""
  install_env_file 203.0.113.10
  if grep -q OPERATOR_CONTACT "$sent"; then fail "empty Foundation contact was written"; else pass; fi
  TOKEN=""
}

test_access_host_validation_and_env() {
  reset_script_functions
  local value sent="$TEST_TMP/access-install-stdin"
  for value in 2001:db8:1::10 2a01:4f9:c010:1234::a off ''; do
    if (ACCESS_HOST="$value"; validate_config) >/dev/null 2>&1; then pass; else fail "access host '${value}' rejected"; fi
  done
  for value in 203.0.113.7 '[2001:db8::1]' '2001:db8::1%eth0' relay.example.org '2001:db8::1;reboot' "\$(id)"; do
    if (ACCESS_HOST="$value"; validate_config) >/dev/null 2>&1; then fail "access host '${value}' accepted"; else pass; fi
  done

  ACCESS_HOST=""
  case "$(preserve_drop)" in *ACCESS_HOST*) fail "unset access host drops the host's setting" ;; *) pass ;; esac
  for value in 2001:db8:1::10 off; do
    ACCESS_HOST="$value"
    assert_contains "$(preserve_drop)" '|ACCESS_HOST)=' "access host ${value} replaces the host's setting"
  done

  ssh_run() { cat > "$sent"; }
  TOKEN=test-token
  ACCESS_HOST=2001:db8:1::10
  install_env_file 203.0.113.10
  assert_contains "$(<"$sent")" 'OPENRUNG_ACCESS_HOST=2001:db8:1::10' "convert writes the access host"
  for value in off ''; do
    ACCESS_HOST="$value"
    install_env_file 203.0.113.10
    if grep -q ACCESS_HOST "$sent"; then fail "access host '${value}' was written"; else pass; fi
  done
  TOKEN=""
}

# The address is added before the roll, so the candidate relay finds it at its
# first registration; the unit is swapped only after a verified roll; a failed
# roll removes only what this run added and never reaches the commit.
test_convert_orders_access_address_around_roll() {
  reset_script_functions
  preflight_host() { :; }
  prepare_image() { printf 'pull\n' >> "$MUTATION_LOG"; }
  stage_preserved_env() { printf 'stage\n' >> "$MUTATION_LOG"; }
  ssh_run() { printf 'public_host=203.0.113.10\nlabel=test\n'; }
  install_env_file() { printf 'install\n' >> "$MUTATION_LOG"; }
  prepare_access_address() { printf 'access-prepare\n' >> "$MUTATION_LOG"; }
  abort_access_address() { printf 'access-abort\n' >> "$MUTATION_LOG"; }
  commit_access_address() { printf 'access-commit\n' >> "$MUTATION_LOG"; }
  roll_host() { printf 'roll\n' >> "$MUTATION_LOG"; }
  apply_egress_limit() { printf 'egress\n' >> "$MUTATION_LOG"; }
  : > "$MUTATION_LOG"
  (set -e; convert_host 203.0.113.10) >"$OUTPUT" 2>&1
  assert_eq $'pull\nstage\ninstall\naccess-prepare\nroll\naccess-commit\negress' "$(<"$MUTATION_LOG")" "convert adds the address before the roll and commits the unit after it"

  roll_host() { printf 'roll\n' >> "$MUTATION_LOG"; return 1; }
  : > "$MUTATION_LOG"
  if (set -e; convert_host 203.0.113.10) >"$OUTPUT" 2>&1; then fail "convert succeeded after a failed roll"; else pass; fi
  assert_eq $'pull\nstage\ninstall\naccess-prepare\nroll\naccess-abort' "$(<"$MUTATION_LOG")" "a failed roll aborts the access change and never commits it"

  prepare_access_address() { printf 'access-prepare\n' >> "$MUTATION_LOG"; return 1; }
  : > "$MUTATION_LOG"
  if (set -e; convert_host 203.0.113.10) >"$OUTPUT" 2>&1; then fail "convert continued after the access address failed"; else pass; fi
  assert_eq $'pull\nstage\ninstall\naccess-prepare' "$(<"$MUTATION_LOG")" "a failed access address never rolls the relay"
}

# Run the exact remote commands against stubs that keep the host's address
# list in a file: ip adds and deletes lines, and systemctl start/stop run the
# unit's own ExecStart/ExecStop addresses (or fail to start on request).
test_access_address_remote_commands() {
  reset_script_functions
  local stub="${TEST_TMP}/access-stub" calls="${TEST_TMP}/access-calls" root="${TEST_TMP}/access-root"
  local addrs="${TEST_TMP}/access-addrs" failstart="${TEST_TMP}/access-failstart" old new
  mkdir -p "$stub" "$root"
  ACCESS_UNIT="${root}/openrung-access-address.service"
  ACCESS_REASSERT_SERVICE="${root}/openrung-access-address-reassert.service"
  ACCESS_REASSERT_SCRIPT="${root}/openrung-access-address-reassert"
  ACCESS_REASSERT_TIMER="${root}/openrung-access-address-reassert.timer"
  cat > "${stub}/sudo" <<'EOF'
#!/bin/sh
exec "$@"
EOF
  cat > "${stub}/ip" <<EOF
#!/bin/sh
echo "ip \$*" >> "$calls"
case "\$*" in
  "-6 route show default") echo "default via fe80::1 dev eth0 proto static metric 1024 pref medium" ;;
  "-6 -o address show to "*) a="\${6%/128}"; grep " \$a/" "$addrs" || true ;;
  "-6 address replace "*) a="\${4%/128}"; grep -v " \$a/" "$addrs" > "$addrs.tmp"; mv "$addrs.tmp" "$addrs"; echo "2: \$6    inet6 \$a/128 scope global deprecated nodad" >> "$addrs" ;;
  "-6 address delete "*) a="\${4%/128}"; grep -v " \$a/" "$addrs" > "$addrs.tmp"; mv "$addrs.tmp" "$addrs" ;;
esac
EOF
  cat > "${stub}/systemctl" <<EOF
#!/bin/sh
echo "systemctl \$*" >> "$calls"
unit_addr() { sed -n 's/^ExecStart=.* replace \([^/]*\)\/128 .*/\1/p' "$ACCESS_UNIT"; }
case "\$*" in
  "enable --now openrung-access-address.service")
    [ -e "$failstart" ] && grep -q "\$(cat "$failstart")" "$ACCESS_UNIT" && exit 1
    a=\$(unit_addr); grep -v " \$a/" "$addrs" > "$addrs.tmp"; mv "$addrs.tmp" "$addrs"
    echo "2: eth0    inet6 \$a/128 scope global deprecated nodad" >> "$addrs" ;;
  "stop openrung-access-address.service"|"disable --now openrung-access-address.service")
    a=\$(unit_addr); grep -v " \$a/" "$addrs" > "$addrs.tmp"; mv "$addrs.tmp" "$addrs" ;;
  *"stop "*"openrung-access-address-reassert.service"*)
    # A re-assert run is in flight: stop returns only once it has finished,
    # so model that run completing here — with the address missing, it
    # re-adds whatever the unit currently names.
    [ -x "$ACCESS_REASSERT_SCRIPT" ] && "$ACCESS_REASSERT_SCRIPT" >/dev/null || true ;;
esac
exit 0
EOF
  chmod +x "$stub"/*
  ssh_run() { PATH="${stub}:${PATH}" bash -c "$2"; }
  has_addr() { grep -q " $1/" "$addrs"; }
  old=2001:db8:1::10
  new=2001:db8:1::20

  # Unset: nothing on the host is touched at either step.
  : > "$calls"; : > "$addrs"; rm -f "$ACCESS_UNIT" "$failstart"
  ACCESS_HOST=""
  (set -e; prepare_access_address 203.0.113.10; commit_access_address 203.0.113.10) >"$OUTPUT" 2>&1
  if [ -s "$calls" ]; then fail "unset access host touched the host: $(<"$calls")"; else pass; fi

  # Fresh host: added before the roll, kept by the unit after it.
  ACCESS_HOST="$old"
  if (set -e; prepare_access_address 203.0.113.10; commit_access_address 203.0.113.10) >"$OUTPUT" 2>&1; then pass; else fail "fresh access address failed: $(<"$OUTPUT")"; fi
  has_addr "$old" && pass || fail "fresh access address missing"
  assert_contains "$(<"$ACCESS_UNIT")" "ExecStart=/sbin/ip -6 address replace ${old}/128 dev eth0 preferred_lft 0 nodad" "unit adds the address deprecated on the default-route interface"
  assert_contains "$(<"$ACCESS_UNIT")" "ExecStop=-/sbin/ip -6 address delete ${old}/128 dev eth0" "unit removes the address on stop"
  assert_contains "$(<"$ACCESS_UNIT")" "Before=docker.service" "unit runs before docker starts the relay"
  assert_contains "$(<"$ACCESS_REASSERT_SERVICE")" "ExecCondition=/bin/systemctl is-active --quiet openrung-access-address.service" "re-assert acts only while the unit is active"
  assert_contains "$(<"$ACCESS_REASSERT_SERVICE")" "ExecStart=${ACCESS_REASSERT_SCRIPT}" "re-assert runs the script"
  assert_contains "$(<"$ACCESS_REASSERT_SERVICE")" "LogLevelMax=notice" "re-assert keeps systemd's per-run lines out of the journal"
  if [ -x "$ACCESS_REASSERT_SCRIPT" ]; then pass; else fail "re-assert script missing or not executable"; fi
  if [ -e "${ACCESS_REASSERT_SCRIPT}.new" ]; then fail "re-assert script staging copy left behind"; else pass; fi
  assert_contains "$(<"$ACCESS_REASSERT_TIMER")" "OnUnitActiveSec=1min" "re-assert runs every minute"
  assert_contains "$(<"$calls")" "systemctl enable --now openrung-access-address-reassert.timer" "re-assert timer is enabled and started"
  case "$(<"$ACCESS_REASSERT_SERVICE")$(<"$ACCESS_REASSERT_TIMER")$(<"$ACCESS_REASSERT_SCRIPT")" in *"${old}"*) fail "the re-assert files name the address" ;; *) pass ;; esac
  if [ -e "${ACCESS_UNIT}.new" ] || [ -e "${ACCESS_UNIT}.old" ]; then fail "staging files left behind"; else pass; fi

  # Re-running with the same address changes nothing and never stops the unit.
  : > "$calls"
  if (set -e; prepare_access_address 203.0.113.10; commit_access_address 203.0.113.10) >"$OUTPUT" 2>&1; then pass; else fail "re-run failed: $(<"$OUTPUT")"; fi
  case "$(<"$calls")" in *"systemctl stop"*|*"address replace"*|*"address delete"*) fail "re-run disturbed the address: $(<"$calls")" ;; *) pass ;; esac
  has_addr "$old" && pass || fail "re-run lost the address"

  # A changed address whose roll fails: the run's own address is removed and
  # the previous address and unit are untouched.
  local before; before="$(<"$ACCESS_UNIT")"
  ACCESS_HOST="$new"
  roll_check() { if has_addr "$old" && has_addr "$new"; then echo beside; fi; }
  (set -e; prepare_access_address 203.0.113.10; roll_check > "${TEST_TMP}/access-during-roll"; abort_access_address 203.0.113.10) >"$OUTPUT" 2>&1
  assert_eq beside "$(<"${TEST_TMP}/access-during-roll")" "the new address sits beside the old one during the roll"
  if has_addr "$old" && ! has_addr "$new"; then pass; else fail "abort must leave exactly the previous address: $(<"$addrs")"; fi
  assert_eq "$before" "$(<"$ACCESS_UNIT")" "abort leaves the previous unit untouched"

  # A changed address whose new unit fails to start after a verified roll:
  # the previous unit is restored with its address, and the relay keeps the
  # new address it is serving.
  echo "$new" > "$failstart"
  if (set -e; prepare_access_address 203.0.113.10; commit_access_address 203.0.113.10) >"$OUTPUT" 2>&1; then fail "commit succeeded although the unit did not start"; else pass; fi
  assert_contains "$(<"$OUTPUT")" "previous unit was restored" "commit failure says the previous unit was restored"
  assert_eq "$before" "$(<"$ACCESS_UNIT")" "commit failure restores the previous unit"
  assert_contains "$(<"$calls")" "systemctl start openrung-access-address-reassert.timer" "commit failure restarts the re-assert timer for the restored unit"
  if has_addr "$old" && has_addr "$new"; then pass; else fail "commit failure must keep the previous and the served address: $(<"$addrs")"; fi
  if [ -e "${ACCESS_UNIT}.new" ] || [ -e "${ACCESS_UNIT}.old" ]; then fail "staging files left behind after restore"; else pass; fi

  # The same change once the unit starts: the previous address goes with the
  # previous unit, the new unit keeps the new one.
  rm -f "$failstart"
  : > "$calls"
  if (set -e; prepare_access_address 203.0.113.10; commit_access_address 203.0.113.10) >"$OUTPUT" 2>&1; then pass; else fail "address change failed: $(<"$OUTPUT")"; fi
  if ! has_addr "$old" && has_addr "$new"; then pass; else fail "after the change only the new address may remain: $(<"$addrs")"; fi
  assert_before "$(<"$calls")" "systemctl stop openrung-access-address.service" "systemctl enable --now" "the previous unit stops (removing its address) before the new one starts"
  assert_before "$(<"$calls")" "systemctl stop openrung-access-address-reassert.timer openrung-access-address-reassert.service" "systemctl stop openrung-access-address.service" "re-assert is stopped and drained before the previous address is removed"
  assert_contains "$(<"$ACCESS_UNIT")" "replace ${new}/128" "the new unit names the new address"

  # The host's own preferred address is refused before anything changes.
  : > "$calls"
  echo "2: eth0    inet6 2001:db8:1::30/64 scope global" >> "$addrs"
  before="$(<"$ACCESS_UNIT")"
  ACCESS_HOST=2001:db8:1::30
  if (set -e; prepare_access_address 203.0.113.10) >"$OUTPUT" 2>&1; then fail "a preferred host address was accepted"; else pass; fi
  assert_contains "$(<"$OUTPUT")" "already a preferred address" "refusal names the reason"
  case "$(<"$calls")" in *"address replace"*|*systemctl*) fail "a refused address changed the host: $(<"$calls")" ;; *) pass ;; esac
  assert_eq "$before" "$(<"$ACCESS_UNIT")" "a refused address leaves the unit untouched"

  # off changes nothing before the roll, then removes the unit and its address.
  ACCESS_HOST=off
  : > "$calls"
  (set -e; prepare_access_address 203.0.113.10) >"$OUTPUT" 2>&1
  if [ -s "$calls" ]; then fail "off changed the host before the roll: $(<"$calls")"; else pass; fi
  if (set -e; prepare_access_address 203.0.113.10; commit_access_address 203.0.113.10) >"$OUTPUT" 2>&1; then pass; else fail "off failed: $(<"$OUTPUT")"; fi
  assert_contains "$(<"$calls")" "systemctl disable --now openrung-access-address.service" "off stops and disables the unit"
  assert_before "$(<"$calls")" "systemctl disable --now openrung-access-address-reassert.timer" "systemctl disable --now openrung-access-address.service" "off stops the re-assert timer before the unit, so it cannot re-add the address"
  assert_before "$(<"$calls")" "systemctl stop openrung-access-address-reassert.service" "systemctl disable --now openrung-access-address.service" "off drains a re-assert run in progress before the address is removed"
  if [ -e "$ACCESS_UNIT" ]; then fail "off left the unit behind"; else pass; fi
  if [ -e "$ACCESS_REASSERT_SERVICE" ] || [ -e "$ACCESS_REASSERT_TIMER" ] || [ -e "$ACCESS_REASSERT_SCRIPT" ]; then fail "off left the re-assert files behind"; else pass; fi
  if has_addr "$new"; then fail "off left the address behind"; else pass; fi
  # A commit without a prepared interface refuses instead of writing a unit.
  ACCESS_HOST="$new"; ACCESS_DEV=""
  if (set -e; commit_access_address 203.0.113.10) >"$OUTPUT" 2>&1; then fail "commit ran without prepare"; else pass; fi
  ACCESS_HOST=""
}

# commit_candidate reports whether the candidate went live, whatever the
# commit's outcome, so a failed roll can tell a cleanup failure from a failed
# deployment.
# The re-assert script itself, against a stub `ip`: silent and hands-off while
# the address is present and deprecated; re-adds it and logs a warning when it
# is missing or no longer deprecated; refuses an unreadable unit.
test_access_reassert_script() {
  reset_script_functions
  local stub="${TEST_TMP}/reassert-stub" calls="${TEST_TMP}/reassert-calls" addrs="${TEST_TMP}/reassert-addrs" script="${TEST_TMP}/reassert.sh" out rc
  mkdir -p "$stub"
  ACCESS_UNIT="${TEST_TMP}/reassert-unit.service"
  access_address_unit 2001:db8:1::10 eth0 > "$ACCESS_UNIT"
  access_reassert_script > "$script"
  chmod +x "$script"
  cat > "${stub}/ip" <<EOF
#!/bin/sh
echo "ip \$*" >> "$calls"
case "\$*" in "-6 -o address show to "*) cat "$addrs" ;; esac
EOF
  chmod +x "${stub}/ip"

  : > "$calls"; echo "2: eth0    inet6 2001:db8:1::10/128 scope global deprecated nodad" > "$addrs"
  out="$(PATH="${stub}:${PATH}" sh "$script")"; rc=$?
  assert_eq 0 "$rc" "present address exits cleanly"
  assert_eq "" "$out" "present address logs nothing"
  case "$(<"$calls")" in *"address replace"*) fail "present address was re-added" ;; *) pass ;; esac

  : > "$calls"; : > "$addrs"
  out="$(PATH="${stub}:${PATH}" sh "$script")"; rc=$?
  assert_eq 0 "$rc" "missing address exits cleanly"
  assert_contains "$(<"$calls")" "ip -6 address replace 2001:db8:1::10/128 dev eth0 preferred_lft 0 nodad" "missing address is re-added deprecated on the unit's interface"
  assert_eq "<4>re-added access address 2001:db8:1::10 on eth0" "$out" "re-adding logs one warning"

  : > "$calls"; echo "2: eth0    inet6 2001:db8:1::10/128 scope global" > "$addrs"
  out="$(PATH="${stub}:${PATH}" sh "$script")"
  assert_contains "$(<"$calls")" "address replace 2001:db8:1::10/128" "a preferred address is made deprecated again"
  assert_contains "$out" "<4>re-added" "and that is logged"

  echo "[Service]" > "$ACCESS_UNIT"; : > "$calls"
  out="$(PATH="${stub}:${PATH}" sh "$script")"; rc=$?
  assert_eq 1 "$rc" "an unreadable unit fails"
  assert_contains "$out" "<3>cannot read the access address" "and says so"
  case "$(<"$calls")" in *"address replace"*) fail "an unreadable unit re-added something" ;; *) pass ;; esac
}

test_commit_candidate_reports_promotion() {
  reset_script_functions
  local rc_in want
  for rc_in in 0:1 43:1 42:0 1:unknown 255:unknown; do
    want="${rc_in#*:}"
    eval "ssh_run() { return ${rc_in%%:*}; }"
    ROLL_PROMOTED=sentinel
    commit_candidate 203.0.113.10 >"$OUTPUT" 2>&1
    assert_eq "$want" "$ROLL_PROMOTED" "commit exit ${rc_in%%:*} reports promotion"
  done
}

# A failed roll undoes the access change only when the candidate never went
# live. The real roll_host and commit_candidate run here, with the cleanup of
# the old container failing after promotion (exit 43): the live relay keeps
# the address and its unit is committed.
test_failed_roll_keeps_access_address_after_promotion() {
  reset_script_functions
  preflight_host() { :; }
  prepare_image() { :; }
  stage_preserved_env() { :; }
  install_env_file() { :; }
  apply_egress_limit() { printf 'egress\n' >> "$MUTATION_LOG"; }
  prepare_access_address() { printf 'access-prepare\n' >> "$MUTATION_LOG"; }
  abort_access_address() { printf 'access-abort\n' >> "$MUTATION_LOG"; }
  commit_access_address() { printf 'access-commit\n' >> "$MUTATION_LOG"; }
  begin_roll() { :; }
  run_candidate() { :; }
  verify_foundation() { :; }
  ssh_run() {
    case "$*" in
      *"docker rename ${NEW_CONTAINER} ${CONTAINER}"*) return "$COMMIT_RC" ;;
      *) printf 'public_host=203.0.113.10\nlabel=test\n' ;;
    esac
  }
  local case_rc want
  for case_rc in $'43\naccess-prepare\naccess-commit' $'42\naccess-prepare\naccess-abort' $'1\naccess-prepare'; do
    COMMIT_RC="${case_rc%%$'\n'*}"
    want="${case_rc#*$'\n'}"
    : > "$MUTATION_LOG"
    if (set -e; convert_host 203.0.113.10) >"$OUTPUT" 2>&1; then fail "convert succeeded with commit exit ${COMMIT_RC}"; else pass; fi
    assert_eq "$want" "$(<"$MUTATION_LOG")" "commit exit ${COMMIT_RC}: access change after the failed roll"
  done

  # A roll that fails before the commit never promoted anything.
  verify_foundation() { return 1; }
  : > "$MUTATION_LOG"
  if (set -e; convert_host 203.0.113.10) >"$OUTPUT" 2>&1; then fail "convert succeeded after a failed verification"; else pass; fi
  assert_eq $'access-prepare\naccess-abort' "$(<"$MUTATION_LOG")" "a failed verification undoes the access change"
}

test_apply_egress_limit_remote_command() {
  reset_script_functions
  local stub="${TEST_TMP}/egress-stub" calls="${TEST_TMP}/egress-calls" root="${TEST_TMP}/egress-root" out
  mkdir -p "$stub" "$root"
  : > "$calls"
  cat > "${stub}/sudo" <<'EOF'
#!/bin/sh
exec "$@"
EOF
  cat > "${stub}/fake-egress-limit" <<EOF
#!/bin/sh
echo "egress-limit \$*" >> "$calls"
case "\$1" in unit) printf '[Unit]\\nDescription=test unit\\n' ;; esac
EOF
  cat > "${stub}/docker" <<EOF
#!/bin/sh
echo "docker \$*" >> "$calls"
case "\$*" in *" cat "*) cat "${stub}/fake-egress-limit" ;; esac
EOF
  for cmd in systemctl apt-get install; do
    cat > "${stub}/${cmd}" <<EOF
#!/bin/sh
echo "${cmd} \$*" >> "$calls"
EOF
  done
  chmod +x "$stub"/*
  ssh_run() { PATH="${stub}:${PATH}" bash -c "$2"; }
  IMAGE="ghcr.io/openrung/openrung-relay:0.2.3"
  EGRESS_LIMIT_BIN="${root}/openrung-egress-limit"
  EGRESS_LIMIT_UNIT="${root}/openrung-egress-limit.service"
  NEW_DEST_RATE=10
  if (set -e; apply_egress_limit 203.0.113.10) >"$OUTPUT" 2>&1; then pass; else fail "apply_egress_limit failed: $(<"$OUTPUT")"; fi
  out="$(<"$calls")"
  assert_contains "$out" "docker run --rm --network none --cap-drop ALL --entrypoint cat ghcr.io/openrung/openrung-relay:0.2.3 /usr/local/bin/egress-limit" "the image's script is copied out with no capabilities and no network"
  case "$out" in *NET_ADMIN*|*"--network host"*) fail "no container may touch the host firewall: ${out}" ;; *) pass ;; esac
  assert_contains "$out" "egress-limit apply 10" "the host runs the image's script with its own nft"
  assert_before "$out" "egress-limit apply 10" "egress-limit unit" "rules load before the unit is written"
  assert_contains "$out" "systemctl enable openrung-egress-limit.service" "boot unit is enabled"
  case "$out" in *restart*|*"enable --now"*) fail "the unit must never be restarted: ${out}" ;; *) pass ;; esac
  cmp -s "${stub}/fake-egress-limit" "$EGRESS_LIMIT_BIN" && pass || fail "the installed script is not the image's copy"
  [ -x "$EGRESS_LIMIT_BIN" ] && pass || fail "the installed script is not executable"
  [ ! -e "${EGRESS_LIMIT_BIN}.new" ] && pass || fail "staging copy left behind"
  assert_contains "$(<"$EGRESS_LIMIT_UNIT")" "Description=test unit" "the script's unit text reaches the unit file"

  # An image without the script must fail before anything is installed or run.
  rm -f "$EGRESS_LIMIT_BIN" "$EGRESS_LIMIT_UNIT"
  : > "$calls"
  cat > "${stub}/docker" <<EOF
#!/bin/sh
echo "docker \$*" >> "$calls"
EOF
  if (set -e; apply_egress_limit 203.0.113.10) >"$OUTPUT" 2>&1; then fail "apply succeeded from an image without the script"; else pass; fi
  [ ! -e "$EGRESS_LIMIT_BIN" ] && [ ! -e "$EGRESS_LIMIT_UNIT" ] && pass || fail "files were installed from an image without the script"
  case "$(<"$calls")" in *"egress-limit "*) fail "a script ran from an image without one" ;; *) pass ;; esac

  # A script that cannot print its unit must not leave an empty unit behind.
  : > "$calls"
  cat > "${stub}/docker" <<EOF
#!/bin/sh
echo "docker \$*" >> "$calls"
case "\$*" in *" cat "*) printf '#!/bin/sh\\n[ "\$1" = unit ] && exit 1\\nexit 0\\n' ;; esac
EOF
  if (set -e; apply_egress_limit 203.0.113.10) >"$OUTPUT" 2>&1; then fail "apply succeeded when the script could not print its unit"; else pass; fi
  [ ! -e "$EGRESS_LIMIT_UNIT" ] && pass || fail "an empty unit was written after a failed render"

  : > "$calls"
  NEW_DEST_RATE=off
  cat > "${stub}/docker" <<EOF
#!/bin/sh
echo "docker \$*" >> "$calls"
case "\$*" in *" cat "*) cat "${stub}/fake-egress-limit" ;; esac
EOF
  (set -e; apply_egress_limit 203.0.113.10) >"$OUTPUT" 2>&1
  assert_contains "$(<"$calls")" "egress-limit apply off" "rate off removes the limit through the same path"
}

test_audit_remote_command() {
  reset_script_functions
  local stub="${TEST_TMP}/audit-stub" out
  mkdir -p "$stub"
  cat > "${stub}/sudo" <<'EOF'
#!/bin/sh
exec "$@"
EOF
  cat > "${stub}/docker" <<'EOF'
#!/bin/sh
case "$1" in
  inspect) echo sha256:abc ;;
  logs) echo "2026/10/07 INFO starting relay version=0.2.2 revision=x" ;;
  run) echo "#!/bin/sh" ;;
esac
EOF
  cat > "${stub}/nft" <<'EOF'
#!/bin/sh
exit 0
EOF
  cat > "${stub}/systemctl" <<'EOF'
#!/bin/sh
echo enabled
EOF
  chmod +x "$stub"/*
  pin_host_key() { :; }
  ssh_run() { PATH="${stub}:${PATH}" bash -c "$2"; }
  NEW_DEST_RATE=10
  out="$(cmd_audit 203.0.113.10 2>&1)"
  assert_contains "$out" "203.0.113.10 relay=0.2.2 " "audit reports the relay version"
  assert_contains "$out" "host_nft=yes loaded=yes" "audit reports the host nftables and the loaded table"
  assert_contains "$out" "unit=enabled" "audit reports the boot unit"
  case "$out" in *"script=current"*|*"script=missing"*|*"script=stale"*) pass ;; *) fail "audit reported no script state: ${out}" ;; esac
  case "$out" in *"rules=current"*|*"rules=missing"*|*"rules=stale"*) pass ;; *) fail "audit reported no rules state: ${out}" ;; esac

  # The access address the env file names, and whether the host carries it.
  local envfile="${TEST_TMP}/audit-relay.env" addrs="${TEST_TMP}/audit-addrs"
  : > "$envfile"
  : > "$addrs"
  cat > "${stub}/ip" <<EOF
#!/bin/sh
cat "$addrs"
EOF
  chmod +x "${stub}/ip"
  ENV_FILE="$envfile"
  out="$(cmd_audit 203.0.113.10 2>&1)"
  assert_contains "$out" "access=none" "audit reports no access address"
  echo "OPENRUNG_ACCESS_HOST=2001:db8:1::10" > "$envfile"
  out="$(cmd_audit 203.0.113.10 2>&1)"
  assert_contains "$out" "access=2001:db8:1::10 access_state=missing access_unit=enabled access_reassert=" "audit flags a missing access address and reports the re-assert timer"
  echo "2: eth0    inet6 2001:db8:1::10/128 scope global deprecated nodad" > "$addrs"
  out="$(cmd_audit 203.0.113.10 2>&1)"
  assert_contains "$out" "access_state=present" "audit reports a configured access address"
  echo "2: eth0    inet6 2001:db8:1::10/64 scope global" > "$addrs"
  out="$(cmd_audit 203.0.113.10 2>&1)"
  assert_contains "$out" "access_state=preferred" "audit flags an access address the kernel may source egress from"
  ENV_FILE=/etc/openrung/relay.env

  # Without the host's nftables the boot unit cannot restore the rules; audit
  # must say so rather than report the table as unloaded.
  rm "${stub}/nft"
  out="$(PATH="/usr/bin:/bin" cmd_audit 203.0.113.10 2>&1)"
  if command -v nft >/dev/null 2>&1; then
    pass  # this machine has a real nft on PATH; the missing case cannot be staged here
  else
    assert_contains "$out" "host_nft=missing loaded=unknown" "audit flags a host without nftables"
  fi
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
  assert_eq 0 "$([ ! -e "${SIM_DIR}/${OLD_CONTAINER}" ] && [ ! -e "${SIM_DIR}/${NEW_CONTAINER}" ]; echo $?)" "success removes old and candidate names"
  assert_before "$(<"$SIM_LOG")" "rename ${CONTAINER} ${OLD_CONTAINER}" "run ${NEW_CONTAINER}" "candidate starts after live becomes old"
  assert_before "$(<"$SIM_LOG")" "rename ${NEW_CONTAINER} ${CONTAINER}" "rm ${OLD_CONTAINER}" "backup removal follows promotion"

  # A run failure leaves old-only; the real rollback command restores it.
  sim_reset
  sim_put "$CONTAINER" true
  SIM_FAIL_OP=run
  if roll_host 203.0.113.10 "$ENV_FILE" >"$OUTPUT" 2>&1; then rc=0; else rc=$?; fi
  assert_eq 1 "$rc" "candidate start failure propagates"
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
  assert_eq 0 "$([ -e "${SIM_DIR}/${OLD_CONTAINER}" ] && [ -e "${SIM_DIR}/${NEW_CONTAINER}" ] && [ ! -e "${SIM_DIR}/${CONTAINER}" ]; echo $?)" "verification failure leaves exact rollback state"
  if rollback_host 203.0.113.10 >"$OUTPUT" 2>&1; then rc=0; else rc=$?; fi
  assert_eq 0 "$rc" "old+new rollback succeeds"
  assert_eq true "$(sim_running "$CONTAINER")" "old+new rollback restarts live"

  # Cleanup failure leaves live+old. That ambiguous backup is then read-only.
  sim_reset
  sim_put "$OLD_CONTAINER" false
  sim_put "$NEW_CONTAINER" true
  SIM_FAIL_OP=rm
  if commit_candidate 203.0.113.10 >"$OUTPUT" 2>&1; then rc=0; else rc=$?; fi
  assert_eq 1 "$rc" "backup cleanup failure propagates"
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
test_egress_limit_follows_verified_roll
test_new_dest_rate_validation
test_foundation_contact_is_managed_only_when_set
test_access_host_validation_and_env
test_convert_orders_access_address_around_roll
test_access_address_remote_commands
test_access_reassert_script
test_commit_candidate_reports_promotion
test_failed_roll_keeps_access_address_after_promotion
test_apply_egress_limit_remote_command
test_audit_remote_command
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
