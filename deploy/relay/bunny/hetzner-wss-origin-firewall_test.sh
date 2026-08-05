#!/usr/bin/env bash
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="${HERE}/hetzner-wss-origin-firewall.sh"
TEST_TMP="$(mktemp -d)"
trap 'rm -rf "$TEST_TMP"' EXIT

FAKE_BIN="${TEST_TMP}/bin"
FAKE_HOME="${TEST_TMP}/home"
EDGE_FILE="${TEST_TMP}/edges.json"
FIREWALL_STATE="${TEST_TMP}/firewall.json"
INITIAL_STATE="${TEST_TMP}/initial-firewall.json"
OUTPUT="${TEST_TMP}/output"
ERRORS="${TEST_TMP}/errors"
CURL_ARGS="${TEST_TMP}/curl-args"
CURLRC_SEEN="${TEST_TMP}/curlrc-seen"
HCLOUD_LOG="${TEST_TMP}/hcloud-log"
MUTATIONS="${TEST_TMP}/mutations"
mkdir -p "$FAKE_BIN" "$FAKE_HOME"

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); }
fail() { printf 'FAIL: %s\n' "$*" >&2; FAIL=$((FAIL + 1)); }

assert_eq() { # want got context
  local want="$1" got="$2" context="$3"
  if [[ "$want" == "$got" ]]; then
    pass
  else
    fail "${context}: want '${want}', got '${got}'"
  fi
}

assert_contains() { # file needle context
  local file="$1" needle="$2" context="$3"
  if grep -Fq -- "$needle" "$file" 2>/dev/null; then
    pass
  else
    fail "${context}: missing '${needle}'"
  fi
}

assert_unchanged() { # context
  if cmp -s "$INITIAL_STATE" "$FIREWALL_STATE"; then
    pass
  else
    fail "$1: firewall state changed"
  fi
}

mutation_count() {
  if [[ -f "$MUTATIONS" ]]; then
    wc -l <"$MUTATIONS" | tr -d '[:space:]'
  else
    printf '0'
  fi
}

cat >"${FAKE_BIN}/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$@" >"$MOCK_CURL_ARGS"
if [[ -f "${HOME}/.curlrc" ]]; then
  printf 'seen\n' >"$MOCK_CURLRC_SEEN"
fi
# A real curl reads its default config before normal options.  Treat any call
# without --disable in argv[0] as poisoned by the fixture's insecure curlrc.
[[ "${1:-}" == --disable ]] || exit 97
[[ "${MOCK_CURL_MODE:-success}" != fail ]] || exit 22

output_file=""
while (($#)); do
  case "$1" in
    -o|--output)
      (($# >= 2)) || exit 98
      output_file="$2"
      shift 2
      ;;
    *) shift ;;
  esac
done
[[ -n "$output_file" ]] || exit 99
cp "$MOCK_EDGE_FILE" "$output_file"
EOF

cat >"${FAKE_BIN}/hcloud" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%q ' "$@" >>"$MOCK_HCLOUD_LOG"
printf '\n' >>"$MOCK_HCLOUD_LOG"

if [[ "$#" == 5 && "$1" == firewall && "$2" == describe && "$4" == -o && "$5" == json ]]; then
  cat "$MOCK_FIREWALL_STATE"
  exit 0
fi

if [[ "$#" == 5 && "$1" == firewall && "$2" == replace-rules && "$4" == --rules-file ]]; then
  printf 'replace\n' >>"$MOCK_MUTATIONS"
  state_tmp="${MOCK_FIREWALL_STATE}.tmp"
  jq --slurpfile rules "$5" '.rules = $rules[0]' "$MOCK_FIREWALL_STATE" >"$state_tmp"
  mv "$state_tmp" "$MOCK_FIREWALL_STATE"

  case "${MOCK_HCLOUD_AFTER_MODE:-normal}" in
    normal) ;;
    reorder)
      # Model harmless API normalization: reorder rules and address arrays and
      # materialize optional empty/null fields.
      jq '
        .rules |= (
          map(
            .port = (.port // null)
            | .description = (.description // null)
            | .source_ips = ((.source_ips // []) | reverse)
            | .destination_ips = ((.destination_ips // []) | reverse)
          )
          | reverse
        )
      ' "$MOCK_FIREWALL_STATE" >"$state_tmp"
      mv "$state_tmp" "$MOCK_FIREWALL_STATE"
      ;;
    drop-udp)
      jq '.rules |= map(select(.description != "udp-exact"))' \
        "$MOCK_FIREWALL_STATE" >"$state_tmp"
      mv "$state_tmp" "$MOCK_FIREWALL_STATE"
      ;;
    *) exit 96 ;;
  esac
  exit 0
fi

exit 95
EOF

chmod +x "${FAKE_BIN}/curl" "${FAKE_BIN}/hcloud"

write_edges() {
  jq -n '["203.0.113.10", "203.0.113.20", "203.0.113.30"]' >"$EDGE_FILE"
}

write_base_firewall() {
  jq -n '
    {
      id: 123,
      name: "relay-fw",
      rules: [
        {
          direction: "in", protocol: "tcp", port: "22",
          source_ips: ["0.0.0.0/0", "::/0"], description: "ssh"
        },
        {
          direction: "in", protocol: "tcp", port: "9000-9100",
          source_ips: ["192.0.2.0/24"], description: "unrelated-tcp-range"
        },
        {
          direction: "in", protocol: "udp", port: "8443",
          source_ips: ["192.0.2.44/32"], description: "udp-exact"
        },
        {
          direction: "out", protocol: "tcp", port: null,
          destination_ips: ["0.0.0.0/0", "::/0"], description: "outbound-all"
        },
        {
          direction: "in", protocol: "icmp",
          source_ips: ["0.0.0.0/0", "::/0"], description: "icmp"
        },
        {
          direction: "in", protocol: "tcp", port: "8443",
          source_ips: ["198.51.100.7/32"], description: "old-origin"
        }
      ]
    }
  ' >"$FIREWALL_STATE"
}

reset_fixture() {
  CASE_CURL_MODE=success
  CASE_HCLOUD_AFTER_MODE=normal
  CASE_SOURCES_PER_RULE=2
  rm -f "$OUTPUT" "$ERRORS" "$CURL_ARGS" "$CURLRC_SEEN" "$HCLOUD_LOG" "$MUTATIONS"
  write_edges
  write_base_firewall
  cp "$FIREWALL_STATE" "$INITIAL_STATE"
  printf '%s\n' '--insecure' >"${FAKE_HOME}/.curlrc"
}

snapshot_firewall() {
  cp "$FIREWALL_STATE" "$INITIAL_STATE"
}

run_firewall() { # check|apply
  PATH="${FAKE_BIN}:${PATH}" \
  HOME="$FAKE_HOME" \
  MOCK_CURL_ARGS="$CURL_ARGS" \
  MOCK_CURLRC_SEEN="$CURLRC_SEEN" \
  MOCK_CURL_MODE="$CASE_CURL_MODE" \
  MOCK_EDGE_FILE="$EDGE_FILE" \
  MOCK_HCLOUD_LOG="$HCLOUD_LOG" \
  MOCK_HCLOUD_AFTER_MODE="$CASE_HCLOUD_AFTER_MODE" \
  MOCK_FIREWALL_STATE="$FIREWALL_STATE" \
  MOCK_MUTATIONS="$MUTATIONS" \
  OPENRUNG_WSS_ORIGIN_PORT=8443 \
  OPENRUNG_BUNNY_EDGE_LIST_URL='https://edges.example.test/list.json' \
  OPENRUNG_BUNNY_AGGREGATE_PREFIX='' \
  OPENRUNG_MIN_EDGE_ADDRESSES=3 \
  OPENRUNG_MAX_EDGE_ADDRESSES=10 \
  OPENRUNG_MAX_EFFECTIVE_RULES=50 \
  OPENRUNG_SOURCES_PER_RULE="$CASE_SOURCES_PER_RULE" \
    "$SCRIPT" "$1" relay-fw >"$OUTPUT" 2>"$ERRORS"
}

add_rule() { # JSON object
  local state_tmp="${FIREWALL_STATE}.tmp"
  jq --argjson rule "$1" '.rules += [$rule]' "$FIREWALL_STATE" >"$state_tmp"
  mv "$state_tmp" "$FIREWALL_STATE"
  snapshot_firewall
}

assert_rejected_without_mutation() { # mode error-fragment context
  local mode="$1" fragment="$2" context="$3"
  if run_firewall "$mode"; then
    fail "${context}: unsafe firewall was accepted"
  else
    pass
  fi
  assert_contains "$ERRORS" "$fragment" "${context} diagnostic"
  assert_eq 0 "$(mutation_count)" "${context} replace-rules calls"
  assert_unchanged "$context"
}

test_check_is_read_only_and_disables_curlrc() {
  reset_fixture
  if run_firewall check; then
    pass
  else
    fail "check failed: $(<"$ERRORS")"
  fi
  assert_eq --disable "$(head -n 1 "$CURL_ARGS")" "curl config disabled before option parsing"
  if [[ -s "$CURLRC_SEEN" ]]; then pass; else fail "curlrc fixture was not visible to fake curl"; fi
  assert_eq 0 "$(mutation_count)" "check replace-rules calls"
  assert_unchanged "check"
  assert_contains "$OUTPUT" 'added=3 removed=1' "check change summary"
  assert_contains "$OUTPUT" '203.0.113.10/32' "check added ranges"
  assert_contains "$OUTPUT" '198.51.100.7/32' "check removed ranges"
}

test_apply_converges_and_preserves_unrelated_rules() {
  reset_fixture
  CASE_HCLOUD_AFTER_MODE=reorder
  if run_firewall apply; then
    pass
  else
    fail "apply failed: $(<"$ERRORS")"
    return
  fi
  assert_eq 1 "$(mutation_count)" "apply replace-rules calls"
  assert_contains "$OUTPUT" 'firewall=relay-fw port=8443 converged' "apply convergence output"
  assert_eq 2 "$(jq '[.rules[] | select(.direction == "in" and .protocol == "tcp" and .port == "8443")] | length' "$FIREWALL_STATE")" "managed rule chunks"
  assert_eq '["203.0.113.10/32","203.0.113.20/32","203.0.113.30/32"]' \
    "$(jq -c '[.rules[] | select(.direction == "in" and .protocol == "tcp" and .port == "8443") | .source_ips[]] | sort' "$FIREWALL_STATE")" \
    "managed origin sources"
  assert_eq 1 "$(jq '[.rules[] | select(.description == "udp-exact" and .protocol == "udp" and .port == "8443")] | length' "$FIREWALL_STATE")" "same-port UDP rule preserved"
  assert_eq 1 "$(jq '[.rules[] | select(.description == "unrelated-tcp-range" and .port == "9000-9100")] | length' "$FIREWALL_STATE")" "non-overlapping TCP range preserved"
  assert_eq 1 "$(jq '[.rules[] | select(.description == "outbound-all" and .direction == "out")] | length' "$FIREWALL_STATE")" "outbound all-port rule preserved"
  assert_eq 1 "$(jq '[.rules[] | select(.description == "ssh" and .port == "22")] | length' "$FIREWALL_STATE")" "SSH rule preserved"
  assert_eq 1 "$(jq '[.rules[] | select(.description == "icmp" and .protocol == "icmp")] | length' "$FIREWALL_STATE")" "ICMP rule preserved"
  assert_eq 0 "$(jq '[.rules[] | select(.description == "old-origin")] | length' "$FIREWALL_STATE")" "old managed rule replaced"

  if run_firewall check; then
    pass
  else
    fail "post-apply convergence check failed: $(<"$ERRORS")"
  fi
  assert_contains "$OUTPUT" 'added=0 removed=0' "post-apply idempotent diff"
  assert_eq 1 "$(mutation_count)" "post-apply check remained read-only"
}

test_overlapping_ranges_and_all_ports_fail_closed() {
  local rule

  reset_fixture
  rule='{"direction":"in","protocol":"tcp","port":"8000-9000","source_ips":["0.0.0.0/0"],"description":"broad"}'
  add_rule "$rule"
  assert_rejected_without_mutation check 'overlaps managed origin port 8443' "straddling range"

  reset_fixture
  rule='{"direction":"in","protocol":"tcp","port":"8443-9000","source_ips":["0.0.0.0/0"],"description":"broad"}'
  add_rule "$rule"
  assert_rejected_without_mutation apply 'overlaps managed origin port 8443' "range starting at origin"

  reset_fixture
  rule='{"direction":"in","protocol":"tcp","port":"8443-8443","source_ips":["0.0.0.0/0"],"description":"range-not-single"}'
  add_rule "$rule"
  assert_rejected_without_mutation apply 'overlaps managed origin port 8443' "degenerate range"

  reset_fixture
  rule='{"direction":"in","protocol":"tcp","source_ips":["0.0.0.0/0"],"description":"all-ports"}'
  add_rule "$rule"
  assert_rejected_without_mutation apply 'all-port rule' "all-port rule"

  reset_fixture
  rule='{"direction":"in","protocol":"tcp","port":null,"source_ips":["0.0.0.0/0"],"description":"all-ports-null"}'
  add_rule "$rule"
  assert_rejected_without_mutation apply 'all-port rule' "null all-port rule"
}

test_invalid_port_syntax_fails_closed() {
  local port rule
  for port in '80--90' '9000-8000' '0' '08443' '65536'; do
    reset_fixture
    rule="$(jq -nc --arg port "$port" '{direction:"in", protocol:"tcp", port:$port, source_ips:["0.0.0.0/0"], description:"invalid"}')"
    add_rule "$rule"
    assert_rejected_without_mutation apply 'invalid port or range' "invalid port ${port}"
  done

  reset_fixture
  rule='{"direction":"in","protocol":"tcp","port":8443,"source_ips":["0.0.0.0/0"],"description":"numeric-port"}'
  add_rule "$rule"
  assert_rejected_without_mutation apply 'non-string port' "numeric port"
}

test_fetch_and_feed_failures_never_mutate() {
  reset_fixture
  CASE_CURL_MODE=fail
  assert_rejected_without_mutation apply 'could not fetch the bunny edge list' "failed edge fetch"
  if [[ -e "$HCLOUD_LOG" ]]; then
    fail "failed fetch called hcloud"
  else
    pass
  fi

  reset_fixture
  printf '%s\n' '{not-json' >"$EDGE_FILE"
  assert_rejected_without_mutation apply 'edge list is not valid JSON' "invalid JSON feed"

  reset_fixture
  jq -n '["203.0.113.10", "not-an-ip", "203.0.113.30"]' >"$EDGE_FILE"
  assert_rejected_without_mutation apply 'invalid IP address' "invalid address feed"

  reset_fixture
  jq -n '["203.0.113.10", "203.0.113.20"]' >"$EDGE_FILE"
  assert_rejected_without_mutation apply 'implausible unique IPv4 size: 2' "truncated edge feed"

  reset_fixture
  jq -n '["203.0.113.10", "203.0.113.10", "203.0.113.10"]' >"$EDGE_FILE"
  assert_rejected_without_mutation apply 'unique IPv4 size: 1' "duplicate-inflated edge feed"

  reset_fixture
  jq -n '["203.0.113.10", "2001:db8::10", "2001:db8::20"]' >"$EDGE_FILE"
  assert_rejected_without_mutation apply 'unique IPv4 size: 1' "mostly-IPv6 edge feed"
}

test_sources_per_rule_provider_cap_fails_closed() {
  reset_fixture
  CASE_SOURCES_PER_RULE=101
  assert_rejected_without_mutation apply 'sources per rule must be between 1 and 100' "provider source-range cap"
}

test_complete_convergence_check_catches_unrelated_loss() {
  reset_fixture
  CASE_HCLOUD_AFTER_MODE=drop-udp
  if run_firewall apply; then
    fail "apply accepted a post-replacement firewall missing its UDP rule"
  else
    pass
  fi
  assert_eq 1 "$(mutation_count)" "divergent apply replace-rules calls"
  assert_contains "$ERRORS" 'complete intended ruleset' "complete convergence diagnostic"
}

command -v jq >/dev/null || { echo 'FAIL: jq is required for tests' >&2; exit 1; }
test_check_is_read_only_and_disables_curlrc
test_apply_converges_and_preserves_unrelated_rules
test_overlapping_ranges_and_all_ports_fail_closed
test_invalid_port_syntax_fails_closed
test_fetch_and_feed_failures_never_mutate
test_sources_per_rule_provider_cap_fails_closed
test_complete_convergence_check_catches_unrelated_loss

if ((FAIL != 0)); then
  printf '%d assertions passed, %d failed\n' "$PASS" "$FAIL" >&2
  exit 1
fi
printf '%d assertions passed\n' "$PASS"
