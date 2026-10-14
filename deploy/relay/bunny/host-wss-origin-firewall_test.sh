#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="${HERE}/host-wss-origin-firewall.sh"
TEST_TMP="$(mktemp -d)"
trap 'rm -rf "$TEST_TMP"' EXIT

FAKE_BIN="${TEST_TMP}/bin"
EDGE_FILE="${TEST_TMP}/edges.json"
CURRENT_FILE="${TEST_TMP}/current.nft"
SSH_LOG="${TEST_TMP}/ssh-log"
SSH_STDIN_DIR="${TEST_TMP}/ssh-stdin"
OUTPUT="${TEST_TMP}/output"
ERRORS="${TEST_TMP}/errors"
KNOWN_HOSTS="${TEST_TMP}/known_hosts"
SSH_KEY="${TEST_TMP}/key"
mkdir -p "$FAKE_BIN" "$SSH_STDIN_DIR"
: >"$SSH_KEY"
printf '%s\n' '192.0.2.10 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA' >"$KNOWN_HOSTS"

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); }
fail() { printf 'FAIL: %s\n' "$*" >&2; FAIL=$((FAIL + 1)); }

assert_eq() { # want got context
  if [[ "$1" == "$2" ]]; then pass; else fail "$3: want '$1', got '$2'"; fi
}

assert_contains() { # file needle context
  if grep -Fq -- "$2" "$1" 2>/dev/null; then pass; else fail "$3: missing '$2'"; fi
}

assert_absent() { # file needle context
  if grep -Fq -- "$2" "$1" 2>/dev/null; then fail "$3: unexpected '$2'"; else pass; fi
}

cat >"${FAKE_BIN}/curl" <<'SH'
#!/usr/bin/env bash
[[ -n "${FAKE_CURL_FAIL:-}" ]] && exit 22
out=""
while (($#)); do
  if [[ "$1" == -o ]]; then out="$2"; shift; fi
  shift
done
cp "$EDGE_FILE" "$out"
SH

cat >"${FAKE_BIN}/ssh" <<'SH'
#!/usr/bin/env bash
command="${!#}"
n=$(($(wc -l <"$SSH_LOG" 2>/dev/null || echo 0) + 1))
printf '%s\n' "$command" | tr '\n' ' ' >>"$SSH_LOG"
printf '\n' >>"$SSH_LOG"
cat >"${SSH_STDIN_DIR}/${n}"
case "$command" in
  *"sudo cat "*) cat "$CURRENT_FILE" 2>/dev/null || true ;;
  *"nft list table"*) printf 'loaded=no\nunit=missing\n' ;;
esac
SH
chmod +x "${FAKE_BIN}/curl" "${FAKE_BIN}/ssh"

export EDGE_FILE CURRENT_FILE SSH_LOG SSH_STDIN_DIR
export OPENRUNG_SSH_KEY="$SSH_KEY" OPENRUNG_KNOWN_HOSTS="$KNOWN_HOSTS"
export OPENRUNG_MIN_EDGE_ADDRESSES=3

write_edges() { # json
  printf '%s\n' "$1" >"$EDGE_FILE"
}

reset_logs() {
  rm -f "$SSH_LOG" "$CURRENT_FILE" "${SSH_STDIN_DIR}"/*
}

run() { # mode
  PATH="${FAKE_BIN}:$PATH" "$SCRIPT" "$1" relay-y 192.0.2.10 </dev/null >"$OUTPUT" 2>"$ERRORS"
}

ssh_calls() {
  if [[ -f "$SSH_LOG" ]]; then wc -l <"$SSH_LOG" | tr -d ' '; else printf '0'; fi
}

# The ruleset is the stdin of the first write after the read of the saved copy.
ruleset_file() {
  grep -n 'wss-origin-firewall.nft.new' "$SSH_LOG" | head -1 | cut -d: -f1 | sed "s|^|${SSH_STDIN_DIR}/|"
}

test_apply_renders_allowlist_and_loads_before_saving() {
  reset_logs
  write_edges '["198.51.100.1","198.51.100.0","198.51.100.2","198.51.100.3","203.0.113.9"," 203.0.113.9","2001:db8::1"]'
  if run apply; then pass; else fail "apply failed: $(<"$ERRORS")"; return; fi
  local rules
  rules="$(ruleset_file)"
  assert_contains "$rules" '198.51.100.0/30' "adjacent edges collapse"
  assert_contains "$rules" '203.0.113.9/32' "single edge kept exactly"
  assert_absent "$rules" '2001:db8' "IPv6 edges never enter the IPv4 set"
  assert_contains "$rules" 'delete table inet openrung_wss_origin' "load replaces the table atomically"
  assert_contains "$rules" 'tcp dport 8443 ip saddr @bunny_v4 accept' "edge accept rule"
  assert_contains "$rules" 'tcp dport 8443 drop' "non-edge drop rule"
  assert_contains "$OUTPUT" 'relay=relay-y ranges=2 rules=applied' "apply summary"
  # CI runs this as root with nft installed, so the ruleset is parsed by the
  # real nft rather than only pattern-matched.
  if [[ $EUID -eq 0 ]] && command -v nft >/dev/null; then
    if nft -c -f "$rules"; then pass; else fail "nft rejected the rendered ruleset"; fi
  fi
  local load
  load="$(grep -n 'nft -f' "$SSH_LOG" | head -1)"
  [[ "$load" == *'nft -c -f /etc/openrung/wss-origin-firewall.nft.new'*'nft -f /etc/openrung/wss-origin-firewall.nft.new'*'mv /etc/openrung/wss-origin-firewall.nft.new /etc/openrung/wss-origin-firewall.nft'* ]] \
    && pass || fail "ruleset is not validated and loaded before it is saved"
  assert_contains "$SSH_LOG" 'systemctl enable openrung-wss-origin-firewall.service' "boot unit enabled"
  local unit
  unit="$(grep -n 'openrung-wss-origin-firewall.service.new' "$SSH_LOG" | head -1 | cut -d: -f1 | sed "s|^|${SSH_STDIN_DIR}/|")"
  assert_contains "$unit" 'ExecStart=/usr/sbin/nft -f /etc/openrung/wss-origin-firewall.nft' "unit reloads saved rules"
  assert_absent "$unit" 'ExecStop' "stopping the unit never reopens the port"
}

test_check_reports_drift_without_writing() {
  reset_logs
  write_edges '["198.51.100.1","198.51.100.0","198.51.100.2","198.51.100.3","203.0.113.9"]'
  printf 'elements = { 198.51.100.0/30, 192.0.2.0/32 }\n' >"$CURRENT_FILE"
  if run check; then pass; else fail "check failed: $(<"$ERRORS")"; return; fi
  assert_contains "$OUTPUT" '+ 203.0.113.9/32' "added range reported"
  assert_contains "$OUTPUT" '- 192.0.2.0/32' "removed range reported"
  assert_absent "$OUTPUT" '198.51.100.0/30' "unchanged range not reported"
  assert_contains "$OUTPUT" 'rules=drift loaded=no unit=missing' "status line"
  assert_absent "$SSH_LOG" '.new' "check never writes"
}

test_feed_failures_leave_host_untouched() {
  local feed
  for feed in \
    '["198.51.100.1","198.51.100.2"]' \
    '["198.51.100.1","not-an-ip","198.51.100.2","198.51.100.3"]' \
    '{"edges":[]}' \
    'not json'; do
    reset_logs
    write_edges "$feed"
    if run apply; then fail "apply accepted feed: $feed"; else pass; fi
    assert_eq 0 "$(ssh_calls)" "no SSH call for feed: $feed"
  done
  reset_logs
  write_edges '["198.51.100.1","198.51.100.2","198.51.100.3"]'
  if FAKE_CURL_FAIL=1 run apply; then fail "apply survived a fetch failure"; else pass; fi
  assert_contains "$ERRORS" 'host left unchanged' "fetch failure message"
  assert_eq 0 "$(ssh_calls)" "no SSH call after fetch failure"
}

test_unpinned_host_is_refused() {
  reset_logs
  write_edges '["198.51.100.1","198.51.100.2","198.51.100.3"]'
  if PATH="${FAKE_BIN}:$PATH" "$SCRIPT" apply relay-y 192.0.2.99 >"$OUTPUT" 2>"$ERRORS"; then
    fail "unpinned host accepted"
  else
    pass
  fi
  assert_contains "$ERRORS" 'host key is not pinned' "unpinned host message"
  assert_eq 0 "$(ssh_calls)" "no SSH call to an unpinned host"
}

test_apply_renders_allowlist_and_loads_before_saving
test_check_reports_drift_without_writing
test_feed_failures_leave_host_untouched
test_unpinned_host_is_refused

if ((FAIL > 0)); then
  printf '%d assertions failed, %d passed\n' "$FAIL" "$PASS" >&2
  exit 1
fi
printf '%d assertions passed\n' "$PASS"
