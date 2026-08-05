#!/usr/bin/env bash
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="${HERE}/bunny-wss-front.sh"
TEST_TMP="$(mktemp -d)"
trap 'rm -rf "$TEST_TMP"' EXIT

FAKE_BIN="${TEST_TMP}/bin"
API_TOKEN_FILE="${TEST_TMP}/api-token"
ORIGIN_TOKEN_FILE="${TEST_TMP}/origin-token"
ZONES_FILE="${TEST_TMP}/zones.json"
LIST_OVERRIDE_FILE="${TEST_TMP}/list-override.json"
INVENTORY_FILE="${TEST_TMP}/inventory.json"
ZONE_STATE_FILE="${TEST_TMP}/zone-state.json"
OUTPUT="${TEST_TMP}/output"
ERRORS="${TEST_TMP}/errors"
CURL_ARGS="${TEST_TMP}/curl-args"
CURL_ENV="${TEST_TMP}/curl-env"
CAPTURED_HEADER="${TEST_TMP}/captured-header"
BUNNY_CALLS="${TEST_TMP}/bunny-calls"
BUNNY_ARGS="${TEST_TMP}/bunny-args"
BUNNY_ENV="${TEST_TMP}/bunny-env"
BUNNY_BODIES="${TEST_TMP}/bunny-bodies"
mkdir -p "$FAKE_BIN"

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); }
fail() { printf 'FAIL: %s\n' "$*" >&2; FAIL=$((FAIL + 1)); }

assert_eq() { # want got context
  local want="$1" got="$2" context="$3"
  if [[ "$want" == "$got" ]]; then pass; else fail "${context}: want '${want}', got '${got}'"; fi
}

assert_contains() { # file needle context
  local file="$1" needle="$2" context="$3"
  if grep -Fq -- "$needle" "$file"; then pass; else fail "${context}: missing '${needle}'"; fi
}

assert_not_contains() { # file needle context
  local file="$1" needle="$2" context="$3"
  if grep -Fq -- "$needle" "$file"; then fail "${context}: unexpectedly contained '${needle}'"; else pass; fi
}

assert_json() { # file jq-expression context
  local file="$1" expression="$2" context="$3"
  if jq -e "$expression" "$file" >/dev/null 2>&1; then pass; else fail "${context}"; fi
}

reset_bunny_logs() {
  : >"$BUNNY_CALLS"
  : >"$BUNNY_ARGS"
  : >"$BUNNY_ENV"
  : >"$BUNNY_BODIES"
  : >"$OUTPUT"
  : >"$ERRORS"
}

cat >"${FAKE_BIN}/bunny" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ "$#" == 5 && "$1" == api && "$4" == -o && "$5" == json ]]
method="$2"
path="$3"
printf '%s\t%s\n' "$method" "$path" >>"$MOCK_BUNNY_CALLS"
{
  printf 'CALL'
  printf '\t<%s>' "$@"
  printf '\n'
} >>"$MOCK_BUNNY_ARGS"
{
  printf 'CALL %s %s\n' "$method" "$path"
  env
} >>"$MOCK_BUNNY_ENV"

if [[ "$method" == GET ]]; then
  case "$path" in
    '/pullzone?perPage=1000')
      if [[ -f "$MOCK_LIST_OVERRIDE_FILE" ]]; then
        cat "$MOCK_LIST_OVERRIDE_FILE"
      else
        jq 'if type == "object" then
          [{Id,Name,OriginUrl,Enabled,Hostnames}]
        else [] end' "$MOCK_ZONE_STATE_FILE"
      fi
      ;;
    /pullzone/*)
      cat "$MOCK_ZONE_STATE_FILE"
      ;;
    *) exit 98 ;;
  esac
  exit
fi

[[ "$method" == POST ]]
[[ -p /dev/stdin ]] || exit 96
body_file="$(mktemp "${MOCK_TEST_TMP}/request.XXXXXX")"
trap 'rm -f "$body_file" "${body_file}.next"' EXIT
cat >"$body_file"
{
  printf 'BODY %s\n' "$path"
  cat "$body_file"
  printf '\nEND BODY\n'
} >>"$MOCK_BUNNY_BODIES"

case "$path" in
  /pullzone)
    # Enabled is response-only in Bunny's strict request schema.
    jq -e 'has("Enabled") | not' "$body_file" >/dev/null || exit 95
    jq -n --slurpfile request "$body_file" '
      $request[0] + {
        Id:42,
        Enabled:true,
        Suspended:false,
        Hostnames:[{Value:($request[0].Name + ".b-cdn.net")}],
        EdgeRules:[]
      }
    ' >"${body_file}.next"
    mv "${body_file}.next" "$MOCK_ZONE_STATE_FILE"
    printf '{"Id":42}\n'
    ;;
  /pullzone/42)
    # Enabled is response-only in Bunny's strict request schema.
    jq -e 'has("Enabled") | not' "$body_file" >/dev/null || exit 95
    jq --slurpfile request "$body_file" '
      . as $zone
      | reduce ($request[0] | to_entries[]) as $entry
          ($zone; .[$entry.key] = $entry.value)
    ' "$MOCK_ZONE_STATE_FILE" >"${body_file}.next"
    mv "${body_file}.next" "$MOCK_ZONE_STATE_FILE"
    printf '{"Id":42}\n'
    ;;
  /pullzone/42/edgerules/addOrUpdate)
    if [[ "${MOCK_FAIL_EDGE:-0}" == 1 ]]; then
      cat "$body_file"
      cat "$body_file" >&2
      exit 77
    fi
    jq --slurpfile request "$body_file" --arg stale "${MOCK_STALE_TOKEN:-0}" '
      ($request[0]) as $incoming
      | (if $incoming.Guid == null then
           $incoming + {Guid:(if $incoming.ActionParameter1 == "X-OpenRung-Origin-Token"
                              then "guid-origin" else "guid-viewer" end)}
         else $incoming end) as $rule
      | (if $stale == "1" and $rule.ActionParameter1 == "X-OpenRung-Origin-Token" then
           ($rule + {ActionParameter2:
             ([.EdgeRules[]? | select(.ActionParameter1 == "X-OpenRung-Origin-Token")
               | .ActionParameter2][0] // "stale-token-0123456789abcdef0123456789")})
         else $rule end) as $effective
      | if any(.EdgeRules[]?; .Guid == $effective.Guid) then
          .EdgeRules |= map(if .Guid == $effective.Guid then $effective else . end)
        else .EdgeRules += [$effective]
        end
    ' "$MOCK_ZONE_STATE_FILE" >"${body_file}.next"
    mv "${body_file}.next" "$MOCK_ZONE_STATE_FILE"
    printf '{"ok":true}\n'
    ;;
  *) exit 97 ;;
esac
EOF

cat >"${FAKE_BIN}/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%q ' "$@" >"$MOCK_CURL_ARGS"
printf '\n' >>"$MOCK_CURL_ARGS"
env >"$MOCK_CURL_ENV"
header_spec=""
output_file=""
while (($#)); do
  case "$1" in
    --header)
      (($# >= 2)) || exit 91
      header_spec="$2"
      shift 2
      ;;
    --output)
      (($# >= 2)) || exit 93
      output_file="$2"
      shift 2
      ;;
    --write-out)
      (($# >= 2)) || exit 94
      [[ "$2" == '%{http_code}' ]] || exit 95
      shift 2
      ;;
    *) shift ;;
  esac
done
[[ "$header_spec" == @* ]] || exit 92
[[ -n "$output_file" ]] || exit 96
cp "${header_spec#@}" "$MOCK_CAPTURED_HEADER"
cp "$MOCK_INVENTORY_FILE" "$output_file"
printf '%s' "${MOCK_HTTP_STATUS:-200}"
EOF

# GNU stat can print filesystem data for the BSD probe before returning an
# error. Ensure the fallback replaces that noise instead of appending to it.
cat >"${FAKE_BIN}/stat" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$#" == 3 && "$1" == -f && "$2" == '%Lp' ]]; then
  printf 'gnu-filesystem-noise\n'
  exit 1
fi
if [[ "$#" == 3 && "$1" == -c && "$2" == '%a' ]]; then
  if [[ "$(/usr/bin/uname -s)" == Darwin ]]; then
    exec /usr/bin/stat -f '%Lp' "$3"
  fi
  exec /usr/bin/stat -c '%a' "$3"
fi
exit 97
EOF

chmod +x "${FAKE_BIN}/bunny" "${FAKE_BIN}/curl" "${FAKE_BIN}/stat"
printf '%s\n' 'inventory-secret-0123456789abcdef' >"$API_TOKEN_FILE"
printf '%s\n' 'origin-token-0123456789abcdef0123456789abcdef' >"$ORIGIN_TOKEN_FILE"
chmod 600 "$API_TOKEN_FILE" "$ORIGIN_TOKEN_FILE"
printf 'null\n' >"$ZONE_STATE_FILE"

write_complete_inventory() {
  jq -n '
    [range(97; 122) as $n
      | {id: ("relay-" + ([$n] | implode)), label: ("relay-" + ([$n] | implode)), node_class: "foundation",
         wss_fronts: (if $n == 121 then
           [{id: "bunny-y", url: "wss://opaque-y.b-cdn.net/api/v1/wss-bridge"}]
         else [] end)}] as $relays
    | {count: ($relays | length), server_time: "2026-08-04T12:00:00Z",
       not_after: "2026-08-04T12:05:00Z", key_id: "0123456789abcdef",
       channel: "inventory", relays: $relays}
  ' >"$INVENTORY_FILE"
}

write_inventory_zones() {
  jq -n '[{
    Id:42, Name:"opaque-y", Enabled:true,
    OriginUrl:"https://relay-y.example:8443",
    Hostnames:[{Value:"opaque-y.b-cdn.net"}]
  }]' >"$ZONES_FILE"
}

write_zone_state() { # token
  local token="$1"
  jq -n --arg token "$token" '
    def trigger: {
      Type:0,
      PatternMatches:["https://opaque-y.b-cdn.net/api/v1/wss-bridge"],
      PatternMatchingType:0,
      Parameter1:null
    };
    {
      Id:42,
      Name:"opaque-y",
      Enabled:true,
      Suspended:false,
      OriginUrl:"https://relay-y.example:8443",
      Type:0,
      EnableWebSockets:true,
      MaxWebSocketConnections:500,
      EnableLogging:false,
      LoggingIPAnonymizationEnabled:true,
      VerifyOriginSSL:true,
      AddHostHeader:false,
      OriginHostHeader:"relay-y.example",
      CacheControlMaxAgeOverride:0,
      CacheControlPublicMaxAgeOverride:0,
      CacheErrorResponses:false,
      DisableCookies:true,
      EnableGeoZoneUS:true,
      EnableGeoZoneEU:true,
      EnableGeoZoneASIA:true,
      EnableGeoZoneSA:true,
      EnableGeoZoneAF:true,
      MonthlyBandwidthLimit:500000000000,
      Hostnames:[{Value:"opaque-y.b-cdn.net"}],
      EdgeRules:[
        {
          Guid:"guid-origin",
          OrderIndex:0,
          ReadOnly:false,
          ActionParameter3:null,
          Enabled:true,
          ActionType:6,
          ActionParameter1:"X-OpenRung-Origin-Token",
          ActionParameter2:$token,
          Triggers:[trigger],
          ExtraActions:[],
          TriggerMatchingType:0,
          Description:"OpenRung origin token for front bunny-y"
        },
        {
          Guid:"guid-viewer",
          OrderIndex:1,
          ReadOnly:false,
          ActionParameter3:null,
          Enabled:true,
          ActionType:6,
          ActionParameter1:"X-OpenRung-Viewer-Address",
          ActionParameter2:"%{User.IP}:443",
          Triggers:[trigger],
          ExtraActions:[],
          TriggerMatchingType:0,
          Description:"OpenRung viewer address for front bunny-y"
        }
      ]
    }
  ' >"$ZONE_STATE_FILE"
}

run_inventory() {
  PATH="${FAKE_BIN}:$PATH" \
  MOCK_ZONES_FILE="$ZONES_FILE" \
  MOCK_INVENTORY_FILE="$INVENTORY_FILE" \
  MOCK_CURL_ARGS="$CURL_ARGS" \
  MOCK_CURL_ENV="$CURL_ENV" \
  MOCK_CAPTURED_HEADER="$CAPTURED_HEADER" \
  MOCK_BUNNY_CALLS="$BUNNY_CALLS" \
  MOCK_BUNNY_ARGS="$BUNNY_ARGS" \
  MOCK_BUNNY_ENV="$BUNNY_ENV" \
  MOCK_BUNNY_BODIES="$BUNNY_BODIES" \
  MOCK_TEST_TMP="$TEST_TMP" \
  MOCK_ZONE_STATE_FILE="$ZONE_STATE_FILE" \
  MOCK_LIST_OVERRIDE_FILE="$ZONES_FILE" \
  OPENRUNG_API_TOKEN='raw-environment-secret-must-not-reach-children' \
  OPENRUNG_API_TOKEN_FILE="$API_TOKEN_FILE" \
  OPENRUNG_BROKER_URL="${1:-https://broker-origin.openrung.org}" \
    "$SCRIPT" inventory >"$OUTPUT" 2>"$ERRORS"
}

run_create() {
  local origin_host="${1:-relay-y.example}"
  PATH="${FAKE_BIN}:$PATH" \
  MOCK_BUNNY_CALLS="$BUNNY_CALLS" \
  MOCK_BUNNY_ARGS="$BUNNY_ARGS" \
  MOCK_BUNNY_ENV="$BUNNY_ENV" \
  MOCK_BUNNY_BODIES="$BUNNY_BODIES" \
  MOCK_TEST_TMP="$TEST_TMP" \
  MOCK_ZONE_STATE_FILE="$ZONE_STATE_FILE" \
  MOCK_LIST_OVERRIDE_FILE="$LIST_OVERRIDE_FILE" \
  MOCK_STALE_TOKEN="${MOCK_STALE_TOKEN_FLAG:-0}" \
  MOCK_FAIL_EDGE="${MOCK_FAIL_EDGE_FLAG:-0}" \
  OPENRUNG_ORIGIN_TOKEN='raw-origin-secret-must-not-reach-children' \
  OPENRUNG_ORIGIN_TOKEN_FILE="$ORIGIN_TOKEN_FILE" \
    "$SCRIPT" create relay-y "$origin_host" bunny-y opaque-y >"$OUTPUT" 2>"$ERRORS"
}

run_audit() {
  PATH="${FAKE_BIN}:$PATH" \
  MOCK_BUNNY_CALLS="$BUNNY_CALLS" \
  MOCK_BUNNY_ARGS="$BUNNY_ARGS" \
  MOCK_BUNNY_ENV="$BUNNY_ENV" \
  MOCK_BUNNY_BODIES="$BUNNY_BODIES" \
  MOCK_TEST_TMP="$TEST_TMP" \
  MOCK_ZONE_STATE_FILE="$ZONE_STATE_FILE" \
  MOCK_LIST_OVERRIDE_FILE="$LIST_OVERRIDE_FILE" \
    "$SCRIPT" audit relay-y relay-y.example bunny-y opaque-y >"$OUTPUT" 2>"$ERRORS"
}

assert_origin_secret_boundaries() { # secret
  local secret="$1"
  assert_not_contains "$BUNNY_ARGS" "$secret" "origin token in bunny argv"
  assert_not_contains "$BUNNY_ENV" "$secret" "origin token in bunny environment"
  assert_not_contains "$OUTPUT" "$secret" "origin token in stdout"
  assert_not_contains "$ERRORS" "$secret" "origin token in stderr"
  assert_not_contains "$BUNNY_ENV" 'raw-origin-secret-must-not-reach-children' "raw origin token environment"
  assert_not_contains "$BUNNY_ENV" 'OPENRUNG_ORIGIN_TOKEN_FILE=' "origin token file variable environment"
}

test_complete_inventory_and_secret_boundary() {
  local secret
  write_inventory_zones
  write_complete_inventory
  reset_bunny_logs
  if run_inventory; then
    pass
  else
    fail "complete inventory was rejected: $(<"$ERRORS")"
    return
  fi
  assert_eq 25 "$(jq -r '.count' "$INVENTORY_FILE")" "fixture exceeds the public directory cap"
  assert_eq relay-y "$(jq -r '.[0].advertised_by' "$OUTPUT")" "relay below the old page cut is inventoried"
  assert_contains "$CURL_ARGS" 'https://broker-origin.openrung.org/admin/api/relays/inventory' "operational endpoint URL"
  if [[ "$(<"$CURL_ARGS")" == --disable\ * ]]; then pass; else fail "curl config was not disabled first"; fi
  assert_not_contains "$CURL_ARGS" '--location' "curl redirect options"
  assert_eq 'Authorization: Bearer inventory-secret-0123456789abcdef' "$(<"$CAPTURED_HEADER")" "file-backed Authorization header"
  secret="$(<"$API_TOKEN_FILE")"
  assert_not_contains "$CURL_ARGS" "$secret" "curl argv"
  assert_not_contains "$CURL_ENV" "$secret" "curl environment"
  assert_not_contains "$CURL_ENV" 'raw-environment-secret-must-not-reach-children' "unused raw API token environment"
  assert_not_contains "$CURL_ENV" 'OPENRUNG_API_TOKEN_FILE=' "API token file variable environment"
}

test_inventory_completeness_contract() {
  write_inventory_zones
  write_complete_inventory
  jq '.channel = "api" | .limit = 20 | .relays = .relays[:20] | .count = 20' \
    "$INVENTORY_FILE" >"${INVENTORY_FILE}.tmp"
  mv "${INVENTORY_FILE}.tmp" "$INVENTORY_FILE"
  reset_bunny_logs
  if run_inventory; then fail "ranked client page was accepted"; else pass; fi
  assert_contains "$ERRORS" 'not a complete, stable relay inventory' "ranked-page rejection"

  write_complete_inventory
  jq '.count = 26' "$INVENTORY_FILE" >"${INVENTORY_FILE}.tmp"
  mv "${INVENTORY_FILE}.tmp" "$INVENTORY_FILE"
  if run_inventory; then fail "inventory count mismatch was accepted"; else pass; fi
  assert_contains "$ERRORS" 'not a complete, stable relay inventory' "count mismatch rejection"
}

test_inventory_origin_and_transport_guards() {
  write_inventory_zones
  write_complete_inventory
  rm -f "$CURL_ARGS"
  if run_inventory https://broker.openrung.org; then fail "public broker front was accepted"; else pass; fi
  assert_contains "$ERRORS" 'direct broker origin' "public-front rejection"
  if [[ -e "$CURL_ARGS" ]]; then fail "curl ran after public-front rejection"; else pass; fi

  for origin in https://BROKER.OPENRUNG.ORG:8443 https://broker.openrung.org. 'https://broker%2eopenrung%2eorg'; do
    rm -f "$CURL_ARGS"
    if run_inventory "$origin"; then fail "public-front variant was accepted: $origin"; else pass; fi
    if [[ -e "$CURL_ARGS" ]]; then fail "curl ran for rejected origin: $origin"; else pass; fi
  done

  export MOCK_HTTP_STATUS=302
  if run_inventory; then fail "HTTP redirect was accepted"; else pass; fi
  unset MOCK_HTTP_STATUS
  assert_contains "$ERRORS" 'returned HTTP 302, want 200' "redirect rejection"
}

test_token_permissions_fail_closed() {
  write_inventory_zones
  write_complete_inventory
  chmod 644 "$API_TOKEN_FILE"
  if run_inventory; then fail "world-readable API token was accepted"; else pass; fi
  assert_contains "$ERRORS" 'mode 0600' "API token permission rejection"
  chmod 600 "$API_TOKEN_FILE"

  printf 'null\n' >"$ZONE_STATE_FILE"
  chmod 644 "$ORIGIN_TOKEN_FILE"
  reset_bunny_logs
  if run_create; then fail "world-readable origin token was accepted"; else pass; fi
  assert_contains "$ERRORS" 'mode 0600' "origin token permission rejection"
  assert_eq 0 "$(wc -l <"$BUNNY_CALLS" | tr -d ' ')" "no API call before token validation"
  chmod 600 "$ORIGIN_TOKEN_FILE"
}

test_websocket_limit_must_be_a_provider_tier() {
  printf 'null\n' >"$ZONE_STATE_FILE"
  reset_bunny_logs
  export OPENRUNG_WSS_MAX_EDGE_CONNECTIONS=501
  if run_create; then fail "non-tier WebSocket limit was accepted"; else pass; fi
  unset OPENRUNG_WSS_MAX_EDGE_CONNECTIONS
  assert_contains "$ERRORS" 'must be a bunny tier' "non-tier WebSocket limit rejection"
  assert_eq 0 "$(wc -l <"$BUNNY_CALLS" | tr -d ' ')" "WebSocket limit is validated before Bunny API calls"
}

test_origin_hostname_is_canonical_before_suffix_checks() {
  local origin
  printf 'null\n' >"$ZONE_STATE_FILE"
  for origin in OPAQUE.B-CDN.NET D.CLOUDFRONT.NET Relay-Y.example; do
    reset_bunny_logs
    if run_create "$origin"; then fail "noncanonical origin hostname was accepted: $origin"; else pass; fi
    assert_contains "$ERRORS" 'lowercase DNS hostname' "noncanonical origin rejection"
    assert_eq 0 "$(wc -l <"$BUNNY_CALLS" | tr -d ' ')" "origin is rejected before Bunny call"
  done
}

test_unadvertised_zone_is_reported() {
  jq -n '[
    {Id:42,Name:"opaque-y",Enabled:true,OriginUrl:"https://relay-y.example:8443",
     Hostnames:[{Value:"opaque-y.b-cdn.net"}]},
    {Id:43,Name:"orphan-z",Enabled:true,OriginUrl:"https://relay-z.example:8443",
     Hostnames:[{Value:"orphan-z.b-cdn.net"}]}
  ]' >"$ZONES_FILE"
  write_complete_inventory
  reset_bunny_logs
  if run_inventory; then pass; else fail "inventory dropped an unadvertised zone: $(<"$ERRORS")"; return; fi
  assert_eq 2 "$(jq -r 'length' "$OUTPUT")" "all pull zones reported"
  assert_eq orphan-z.b-cdn.net \
    "$(jq -r '.[] | select(.advertised_by == null) | .front_host' "$OUTPUT")" \
    "unadvertised zone listed"
}

test_create_and_idempotent_rerun() {
  local secret
  rm -f "$LIST_OVERRIDE_FILE"
  printf 'null\n' >"$ZONE_STATE_FILE"
  printf '%s\n' 'origin-token-0123456789abcdef0123456789abcdef' >"$ORIGIN_TOKEN_FILE"
  reset_bunny_logs
  if run_create; then pass; else fail "fresh create failed: $(<"$ERRORS")"; return; fi
  assert_eq 3 "$(awk -F '\t' '$1 == "POST" {n++} END {print n+0}' "$BUNNY_CALLS")" "create performs zone and two rule writes"
  assert_json "$ZONE_STATE_FILE" '
    .Enabled == true and .Suspended == false and .Type == 0 and
    .EnableWebSockets == true and .MaxWebSocketConnections == 500 and
    .EnableLogging == false and .LoggingIPAnonymizationEnabled == true and
    .VerifyOriginSSL == true and .AddHostHeader == false and
    .OriginHostHeader == "relay-y.example" and
    .CacheControlMaxAgeOverride == 0 and .CacheControlPublicMaxAgeOverride == 0 and
    .CacheErrorResponses == false and .DisableCookies == true and
    .EnableGeoZoneUS == true and .EnableGeoZoneEU == true and
    .EnableGeoZoneASIA == true and .EnableGeoZoneSA == true and .EnableGeoZoneAF == true and
    .MonthlyBandwidthLimit == 500000000000 and (.EdgeRules | length) == 2
  ' "fresh zone has the complete contract and safe cap"
  secret="$(<"$ORIGIN_TOKEN_FILE")"
  assert_eq "$secret" "$(jq -r '.EdgeRules[] | select(.ActionParameter1 == "X-OpenRung-Origin-Token") | .ActionParameter2' "$ZONE_STATE_FILE")" "created token equality"
  assert_origin_secret_boundaries "$secret"

  reset_bunny_logs
  if run_create; then pass; else fail "idempotent rerun failed: $(<"$ERRORS")"; return; fi
  assert_eq 0 "$(awk -F '\t' '$1 == "POST" {n++} END {print n+0}' "$BUNNY_CALLS")" "converged rerun performs no writes"
  assert_eq 2 "$(jq -r '.EdgeRules | length' "$ZONE_STATE_FILE")" "rerun does not append rules"
  assert_origin_secret_boundaries "$secret"
}

test_existing_settings_converge() {
  local secret
  secret='origin-token-0123456789abcdef0123456789abcdef'
  write_zone_state "$secret"
  jq '
    .Type=1 |
    .EnableWebSockets=false |
    .MaxWebSocketConnections=2500 |
    .EnableLogging=true |
    .LoggingIPAnonymizationEnabled=false |
    .VerifyOriginSSL=false |
    .AddHostHeader=true |
    .OriginHostHeader="wrong.example" |
    .CacheControlMaxAgeOverride=99 |
    .CacheControlPublicMaxAgeOverride=99 |
    .CacheErrorResponses=true |
    .DisableCookies=false |
    .EnableGeoZoneUS=false |
    .EnableGeoZoneEU=false |
    .EnableGeoZoneASIA=false |
    .EnableGeoZoneSA=false |
    .EnableGeoZoneAF=false |
    .MonthlyBandwidthLimit=1
  ' "$ZONE_STATE_FILE" >"${ZONE_STATE_FILE}.tmp"
  mv "${ZONE_STATE_FILE}.tmp" "$ZONE_STATE_FILE"
  reset_bunny_logs
  if run_create; then pass; else fail "settings convergence failed: $(<"$ERRORS")"; return; fi
  assert_eq 1 "$(awk -F '\t' '$1 == "POST" {n++} END {print n+0}' "$BUNNY_CALLS")" "only settings update was needed"
  assert_contains "$BUNNY_CALLS" $'POST\t/pullzone/42' "settings endpoint used"
  assert_json "$ZONE_STATE_FILE" '
    .Enabled == true and .Type == 0 and .EnableWebSockets == true and .MaxWebSocketConnections == 500 and
    .EnableLogging == false and .LoggingIPAnonymizationEnabled == true and
    .VerifyOriginSSL == true and .AddHostHeader == false and
    .OriginHostHeader == "relay-y.example" and
    .CacheControlMaxAgeOverride == 0 and .CacheControlPublicMaxAgeOverride == 0 and
    .CacheErrorResponses == false and .DisableCookies == true and
    .EnableGeoZoneUS == true and .EnableGeoZoneEU == true and
    .EnableGeoZoneASIA == true and .EnableGeoZoneSA == true and .EnableGeoZoneAF == true and
    .MonthlyBandwidthLimit == 500000000000
  ' "all mutable settings converged"
}

test_disabled_zone_fails_before_write() {
  local secret
  secret="$(<"$ORIGIN_TOKEN_FILE")"
  write_zone_state "$secret"
  jq '.Enabled=false' "$ZONE_STATE_FILE" >"${ZONE_STATE_FILE}.tmp"
  mv "${ZONE_STATE_FILE}.tmp" "$ZONE_STATE_FILE"
  reset_bunny_logs
  if run_create; then fail "disabled existing zone was mutated"; else pass; fi
  assert_contains "$ERRORS" 'preflight safety' "disabled-zone preflight rejection"
  assert_eq 0 "$(awk -F '\t' '$1 == "POST" {n++} END {print n+0}' "$BUNNY_CALLS")" "disabled zone fails before writes"
}

test_token_rotation_and_stale_write_detection() {
  local old new
  old='origin-token-0123456789abcdef0123456789abcdef'
  new='rotated-token-abcdef0123456789abcdef0123456789'
  write_zone_state "$old"
  printf '%s\n' "$new" >"$ORIGIN_TOKEN_FILE"
  reset_bunny_logs
  if run_create; then pass; else fail "token rotation failed: $(<"$ERRORS")"; return; fi
  assert_eq 1 "$(awk -F '\t' '$1 == "POST" {n++} END {print n+0}' "$BUNNY_CALLS")" "rotation writes one rule"
  assert_eq "$new" "$(jq -r '.EdgeRules[] | select(.ActionParameter1 == "X-OpenRung-Origin-Token") | .ActionParameter2' "$ZONE_STATE_FILE")" "rotated token equality"
  assert_eq 2 "$(jq -r '.EdgeRules | length' "$ZONE_STATE_FILE")" "rotation reuses rule GUID"
  assert_origin_secret_boundaries "$new"

  write_zone_state "$old"
  reset_bunny_logs
  MOCK_STALE_TOKEN_FLAG=1
  if run_create; then fail "stale token write was accepted"; else pass; fi
  unset MOCK_STALE_TOKEN_FLAG
  assert_contains "$ERRORS" 'origin token did not converge' "stale token rejection"
  assert_origin_secret_boundaries "$new"
}

test_conflicts_fail_before_write() {
  local secret
  secret="$(<"$ORIGIN_TOKEN_FILE")"
  write_zone_state "$secret"
  jq '[. | {Id,Name,OriginUrl:"https://other.example:8443",Enabled,Hostnames}]' \
    "$ZONE_STATE_FILE" >"$LIST_OVERRIDE_FILE"
  reset_bunny_logs
  if run_create; then fail "same-name different-origin conflict was accepted"; else pass; fi
  assert_contains "$ERRORS" 'already exists' "identity conflict reason"
  assert_eq 0 "$(awk -F '\t' '$1 == "POST" {n++} END {print n+0}' "$BUNNY_CALLS")" "identity conflict precedes writes"

  # Simulate the list changing between enumeration and GET. The full resource
  # identity must be checked again before convergence.
  write_zone_state "$secret"
  jq '.OriginUrl="https://other.example:8443"' "$ZONE_STATE_FILE" >"${ZONE_STATE_FILE}.tmp"
  mv "${ZONE_STATE_FILE}.tmp" "$ZONE_STATE_FILE"
  jq -n '[{Id:42,Name:"opaque-y",OriginUrl:"https://relay-y.example:8443",Enabled:true,
    Hostnames:[{Value:"opaque-y.b-cdn.net"}]}]' >"$LIST_OVERRIDE_FILE"
  reset_bunny_logs
  if run_create; then fail "stale list identity was accepted"; else pass; fi
  assert_contains "$ERRORS" 'preflight safety' "GET identity preflight"
  assert_eq 0 "$(awk -F '\t' '$1 == "POST" {n++} END {print n+0}' "$BUNNY_CALLS")" "stale identity precedes writes"
  rm -f "$LIST_OVERRIDE_FILE"
}

test_extra_actions_and_origin_overrides_are_rejected() {
  local secret action
  secret="$(<"$ORIGIN_TOKEN_FILE")"
  for action in 5 2; do
    write_zone_state "$secret"
    jq --argjson action "$action" '
      .EdgeRules[0].ExtraActions=[{
        ActionType:$action,
        ActionParameter1:"X-OpenRung-Origin-Token",
        ActionParameter2:"leak-or-override"
      }]
    ' "$ZONE_STATE_FILE" >"${ZONE_STATE_FILE}.tmp"
    mv "${ZONE_STATE_FILE}.tmp" "$ZONE_STATE_FILE"
    reset_bunny_logs
    if run_audit; then fail "ExtraActions action ${action} bypassed audit"; else pass; fi
    assert_contains "$ERRORS" 'structural audit' "ExtraActions action ${action} audit rejection"

    reset_bunny_logs
    if run_create; then fail "ExtraActions action ${action} bypassed create preflight"; else pass; fi
    assert_contains "$ERRORS" 'preflight safety' "ExtraActions action ${action} preflight rejection"
    assert_eq 0 "$(awk -F '\t' '$1 == "POST" {n++} END {print n+0}' "$BUNNY_CALLS")" "ExtraActions rejection precedes writes"
  done

  write_zone_state "$secret"
  jq '.EdgeRules += [{Guid:"danger",Enabled:false,ActionType:2,ExtraActions:[]}]' \
    "$ZONE_STATE_FILE" >"${ZONE_STATE_FILE}.tmp"
  mv "${ZONE_STATE_FILE}.tmp" "$ZONE_STATE_FILE"
  if run_audit; then fail "dormant origin override passed audit"; else pass; fi
}

test_malformed_list_envelopes_fail_closed() {
  local fixture
  printf 'null\n' >"$ZONE_STATE_FILE"
  for fixture in \
    '{"Items":{}}' \
    '{"Items":[],"CurrentPage":1,"TotalItems":0}' \
    '{"Items":[],"CurrentPage":1,"TotalItems":1,"HasMoreItems":false}' \
    '{"Items":[],"CurrentPage":1,"TotalItems":1,"HasMoreItems":true}' \
    '[{"Id":"42","Name":"opaque-y","OriginUrl":"https://relay-y.example:8443","Hostnames":[{"Value":"opaque-y.b-cdn.net"}]}]' \
    '[{"Id":42,"Name":null,"OriginUrl":"https://relay-y.example:8443","Hostnames":[{"Value":"opaque-y.b-cdn.net"}]}]' \
    '[{"Id":42,"Name":"opaque-y","OriginUrl":null,"Hostnames":[{"Value":"opaque-y.b-cdn.net"}]}]' \
    '[{"Id":42,"Name":"opaque-y","OriginUrl":"https://relay-y.example:8443","Hostnames":null}]' \
    '[{"Id":42,"Name":"opaque-y","OriginUrl":"https://relay-y.example:8443","Hostnames":[]}]'; do
    printf '%s\n' "$fixture" >"$LIST_OVERRIDE_FILE"
    reset_bunny_logs
    if run_create; then fail "malformed pull-zone list was accepted: $fixture"; else pass; fi
    assert_contains "$ERRORS" 'unexpected or incomplete shape' "malformed-envelope rejection"
    assert_eq 0 "$(awk -F '\t' '$1 == "POST" {n++} END {print n+0}' "$BUNNY_CALLS")" "malformed envelope precedes writes"
  done

  printf '%s\n' '{"Items":[],"CurrentPage":1,"TotalItems":0,"HasMoreItems":false}' >"$LIST_OVERRIDE_FILE"
  reset_bunny_logs
  if run_create; then pass; else fail "complete typed list envelope was rejected: $(<"$ERRORS")"; fi
  rm -f "$LIST_OVERRIDE_FILE"
}

test_audit_covers_every_setting_and_trigger_field() {
  local secret key
  secret="$(<"$ORIGIN_TOKEN_FILE")"
  write_zone_state "$secret"
  if run_audit; then pass; else fail "canonical fixture failed audit: $(<"$ERRORS")"; return; fi

  for key in OriginUrl Type EnableWebSockets MaxWebSocketConnections EnableLogging \
    LoggingIPAnonymizationEnabled VerifyOriginSSL AddHostHeader OriginHostHeader \
    CacheControlMaxAgeOverride CacheControlPublicMaxAgeOverride CacheErrorResponses \
    DisableCookies EnableGeoZoneUS EnableGeoZoneEU EnableGeoZoneASIA EnableGeoZoneSA \
    EnableGeoZoneAF MonthlyBandwidthLimit; do
    write_zone_state "$secret"
    jq --arg key "$key" '
      .[$key] = (if (.[$key] | type) == "boolean" then (.[$key] | not)
                 elif (.[$key] | type) == "number" then .[$key] + 1
                 else "drift.example" end)
    ' "$ZONE_STATE_FILE" >"${ZONE_STATE_FILE}.tmp"
    mv "${ZONE_STATE_FILE}.tmp" "$ZONE_STATE_FILE"
    if run_audit; then fail "audit omitted required setting $key"; else pass; fi
  done

  for mutation in \
    '.EdgeRules[0].TriggerMatchingType=1' \
    '.EdgeRules[0].Triggers[0].Type=1' \
    '.EdgeRules[0].Triggers[0].PatternMatches=["https://wrong.example/"]' \
    '.EdgeRules[0].Triggers[0].PatternMatchingType=1' \
    '.EdgeRules[0].Triggers[0].Parameter1="unexpected"' \
    'del(.EdgeRules[0].Triggers[0].Parameter1)'; do
    write_zone_state "$secret"
    jq "$mutation" "$ZONE_STATE_FILE" >"${ZONE_STATE_FILE}.tmp"
    mv "${ZONE_STATE_FILE}.tmp" "$ZONE_STATE_FILE"
    if run_audit; then fail "audit omitted trigger mutation: $mutation"; else pass; fi
  done

  write_zone_state "$secret"
  jq '.Enabled=false' "$ZONE_STATE_FILE" >"${ZONE_STATE_FILE}.tmp" && mv "${ZONE_STATE_FILE}.tmp" "$ZONE_STATE_FILE"
  if run_audit; then fail "disabled zone passed audit"; else pass; fi
  write_zone_state "$secret"
  jq '.Suspended=true' "$ZONE_STATE_FILE" >"${ZONE_STATE_FILE}.tmp" && mv "${ZONE_STATE_FILE}.tmp" "$ZONE_STATE_FILE"
  if run_audit; then fail "suspended zone passed audit"; else pass; fi
}

test_failed_secret_write_is_redacted() {
  local secret
  secret='failure-secret-0123456789abcdef0123456789abcdef'
  write_zone_state 'old-token-0123456789abcdef0123456789abcdef'
  printf '%s\n' "$secret" >"$ORIGIN_TOKEN_FILE"
  reset_bunny_logs
  MOCK_FAIL_EDGE_FLAG=1
  if run_create; then fail "forced edge-rule failure unexpectedly succeeded"; else pass; fi
  unset MOCK_FAIL_EDGE_FLAG
  assert_contains "$ERRORS" 'response withheld' "API failure is redacted"
  assert_origin_secret_boundaries "$secret"
}

command -v jq >/dev/null || { echo 'FAIL: jq is required for tests' >&2; exit 1; }
test_complete_inventory_and_secret_boundary
test_inventory_completeness_contract
test_inventory_origin_and_transport_guards
test_token_permissions_fail_closed
test_websocket_limit_must_be_a_provider_tier
test_origin_hostname_is_canonical_before_suffix_checks
test_unadvertised_zone_is_reported
test_create_and_idempotent_rerun
test_existing_settings_converge
test_disabled_zone_fails_before_write
test_token_rotation_and_stale_write_detection
test_conflicts_fail_before_write
test_extra_actions_and_origin_overrides_are_rejected
test_malformed_list_envelopes_fail_closed
test_audit_covers_every_setting_and_trigger_field
test_failed_secret_write_is_redacted

if ((FAIL != 0)); then
  printf '%d assertions passed, %d failed\n' "$PASS" "$FAIL" >&2
  exit 1
fi
printf '%d assertions passed\n' "$PASS"
