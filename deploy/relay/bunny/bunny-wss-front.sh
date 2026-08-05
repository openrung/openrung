#!/usr/bin/env bash
# Create or audit one dedicated bunny.net WSS pull zone for one relay.
#
#   bunny-wss-front.sh create|audit RELAY_NAME ORIGIN_HOST FRONT_ID ZONE_NAME
#   bunny-wss-front.sh inventory
#
# `inventory` reconstructs the fleet's front mapping live, from the bunny
# account and the broker's authenticated, untruncated operational inventory.
# There is deliberately no checked-in inventory file: a front's whole value is
# that it can be rotated when it is blocked, and a committed mapping would
# preserve every hostname this project has ever used, permanently and
# pre-correlated, in git history. The operational API token must be supplied in
# a mode-0600 file through OPENRUNG_API_TOKEN_FILE; it is never put in argv or
# passed through the environment to curl.
#
# The origin token must be supplied in a mode-0600 file through
# OPENRUNG_ORIGIN_TOKEN_FILE.  It is never accepted in argv or printed.  The
# resulting state file contains only the pull zone ID/hostname and public
# relay/front metadata.
#
# Authentication comes from the already-configured `bunny` CLI profile; no API
# key is read, written, or echoed here.
#
# ZONE_NAME becomes <ZONE_NAME>.b-cdn.net, which is the advertised front host.
# Choose a name that does not name this project: the hostname is visible to a
# resolver-level censor even though the client omits it from the ClientHello,
# and bunny zone names are globally unique, so a guessable one can be probed
# and blocked wholesale.  See bunny-wss.md.
set -euo pipefail
{ set +x; } 2>/dev/null

MODE="${1:-}"
RELAY_NAME="${2:-}"
ORIGIN_HOST="${3:-}"
FRONT_ID="${4:-}"
ZONE_NAME="${5:-}"
ORIGIN_PORT="${OPENRUNG_WSS_ORIGIN_PORT:-8443}"
STATE_FILE="${OPENRUNG_WSS_FRONT_STATE_FILE:-}"
# Pay-as-you-go has no implicit ceiling, so pin the deployed initial 500 GB cap
# unless the operator deliberately chooses another positive byte limit.
BANDWIDTH_LIMIT="${OPENRUNG_WSS_MONTHLY_BANDWIDTH_LIMIT:-500000000000}"
MAX_WS_CONNECTIONS="${OPENRUNG_WSS_MAX_EDGE_CONNECTIONS:-500}"

# bunny.net EdgeRule.ActionType.  SetRequestHeader is what sends the token to
# the origin; SetResponseHeader is the adjacent value and would publish that
# same token to every client, so both are pinned and audited.
readonly ACTION_SET_REQUEST_HEADER=6
readonly ACTION_SET_RESPONSE_HEADER=5
readonly ACTION_OVERRIDE_ORIGIN=2
readonly ORIGIN_TOKEN_HEADER="X-OpenRung-Origin-Token"
# The sidecar needs the viewer address for its per-source limits, and bunny
# sends nothing in the ip:port shape that CloudFront-Viewer-Address has.  Its
# own X-Real-Ip does carry the true client address, but bare, so an edge rule
# builds the expected shape from %{User.IP} instead.
#
# The port is a fixed placeholder.  The sidecar parses ip:port purely to accept
# CloudFront's format and then discards the port, keeping only the address, so
# nothing downstream reads this value — it must simply be non-zero to parse.
#
# X-Forwarded-For is NOT usable here: bunny fills it with an internal hop
# address, identical for every viewer, which would collapse the whole world
# onto one source key and silently disable per-source limiting.  Both this
# header and X-Real-Ip were confirmed to overwrite, not append to, a
# viewer-supplied value, so neither can be spoofed by a client.
readonly VIEWER_ADDRESS_HEADER="X-OpenRung-Viewer-Address"
readonly VIEWER_ADDRESS_VALUE='%{User.IP}:443'

die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# Normalize the two documented pull-zone list representations to one array and
# reject partial or weakly typed envelopes.  In particular, treating a malformed
# or truncated envelope as an empty list would turn an audit failure into a
# write against the wrong zone (or create a duplicate zone).
normalize_pull_zone_list() { # input-file output-file
  local input="$1" output="$2"
  jq -e '
    def integer: type == "number" and . == floor;
    def valid_hostname:
      type == "object" and (.Value | type == "string" and length > 0);
    def valid_zone:
      type == "object" and
      (.Id | integer and . > 0) and
      (.Name | type == "string" and length > 0) and
      (.OriginUrl | type == "string" and length > 0) and
      (.Hostnames | type == "array" and length > 0) and
      ([.Hostnames[] | valid_hostname] | all);

    (if type == "array" then .
     elif type == "object" and
          (.Items | type == "array") and
          (.CurrentPage | integer and . >= 0) and
          (.TotalItems | integer and . >= 0) and
          (.HasMoreItems | type == "boolean") and
          .HasMoreItems == false and
          .TotalItems == (.Items | length)
     then .Items
     else error("unexpected or incomplete pull-zone list envelope")
     end) as $zones
    | select(([$zones[] | valid_zone] | all) and
             (([$zones[].Id] | unique | length) == ($zones | length)))
    | $zones
  ' "$input" >"$output" 2>/dev/null \
    || die "pull zone listing has an unexpected or incomplete shape"
}

require_token_file() { # path variable-name human-name
  local token_file="$1" variable_name="$2" token_name="$3" token_mode
  [[ -n "$token_file" && -f "$token_file" ]] || die "${variable_name} must name a token file"
  # Keep the probes in separate assignments. GNU stat interprets BSD's `-f`
  # differently and may print filesystem data before returning failure; a
  # single command substitution around `bsd || gnu` would retain that partial
  # output and prepend it to the real mode.
  if token_mode="$(stat -f '%Lp' "$token_file" 2>/dev/null)"; then
    :
  elif token_mode="$(stat -c '%a' "$token_file" 2>/dev/null)"; then
    :
  else
    die "could not inspect ${token_name} file permissions"
  fi
  [[ "$token_mode" == 600 ]] || die "${token_name} file must have mode 0600"
  LC_ALL=C awk 'BEGIN{ok=0} NR==1 && $0 !~ /[[:space:]]/ {n=length($0); if (n >= 32 && n <= 512) ok=1} END{if (NR != 1 || !ok) exit 1}' "$token_file" \
    || die "${token_name} must be one 32..512 byte non-whitespace line"
}

if [[ "$MODE" == inventory ]]; then
  command -v bunny >/dev/null || die "bunny CLI is required"
  command -v curl >/dev/null || die "curl is required"
  command -v jq >/dev/null || die "jq is required"
  API_TOKEN_FILE="${OPENRUNG_API_TOKEN_FILE:-}"
  # Do not let either the raw broker variable or the token-file path leak into
  # unrelated child processes. The token itself is accepted only from the
  # protected file and is materialized solely in curl's protected header file.
  unset OPENRUNG_API_TOKEN OPENRUNG_API_TOKEN_FILE API_TOKEN
  require_token_file "$API_TOKEN_FILE" OPENRUNG_API_TOKEN_FILE "operational API token"

  BROKER_URL="${OPENRUNG_BROKER_URL:-https://broker-origin.openrung.org}"
  BROKER_URL="${BROKER_URL%/}"
  [[ "$BROKER_URL" == https://* ]] || die "OPENRUNG_BROKER_URL must be an HTTPS broker origin"
  BROKER_AUTHORITY="${BROKER_URL#https://}"
  [[ -n "$BROKER_AUTHORITY" && "$BROKER_AUTHORITY" != */* && "$BROKER_AUTHORITY" != *\?* \
    && "$BROKER_AUTHORITY" != *\#* && "$BROKER_AUTHORITY" != *@* && "$BROKER_AUTHORITY" != *%* \
    && "$BROKER_AUTHORITY" != *[[:space:]]* ]] \
    || die "OPENRUNG_BROKER_URL must contain only an unencoded HTTPS origin, with no path, query, credentials, or fragment"
  BROKER_AUTHORITY_LOWER="$(printf '%s' "$BROKER_AUTHORITY" | LC_ALL=C tr '[:upper:]' '[:lower:]')"
  if [[ "$BROKER_AUTHORITY_LOWER" == broker.openrung.org \
    || "$BROKER_AUTHORITY_LOWER" == broker.openrung.org. \
    || "$BROKER_AUTHORITY_LOWER" == broker.openrung.org:* \
    || "$BROKER_AUTHORITY_LOWER" == broker.openrung.org.:* ]]; then
    die "OPENRUNG_BROKER_URL must be the direct broker origin, not the public CDN front"
  fi

  umask 077
  INVENTORY_TMP="$(mktemp -d)"
  trap 'rm -rf "$INVENTORY_TMP"' EXIT
  bunny api GET "/pullzone?perPage=1000" -o json </dev/null >"${INVENTORY_TMP}/zones.json" \
    || die "could not list pull zones"
  normalize_pull_zone_list "${INVENTORY_TMP}/zones.json" "${INVENTORY_TMP}/zone-list.json"

  # curl's @file header form keeps the bearer out of its argv. API_TOKEN is a
  # non-exported shell variable and is discarded before curl starts, so the
  # bearer does not appear in curl's environment either.
  API_TOKEN="$(<"$API_TOKEN_FILE")"
  printf 'Authorization: Bearer %s\n' "$API_TOKEN" >"${INVENTORY_TMP}/authorization.header"
  unset API_TOKEN
  # --disable must be curl's first argument. Ignoring ~/.curlrc prevents a
  # caller-local `verbose`, `trace`, or `location-trusted` setting from printing
  # or forwarding the Authorization header behind the script's back.
  INVENTORY_STATUS="$(curl --disable --fail --silent --show-error --proto '=https' --tlsv1.2 --max-time 30 \
    --header "@${INVENTORY_TMP}/authorization.header" \
    --output "${INVENTORY_TMP}/inventory.json" --write-out '%{http_code}' \
    "${BROKER_URL}/admin/api/relays/inventory")" \
    || die "could not fetch the complete relay inventory"
  [[ "$INVENTORY_STATUS" == 200 ]] \
    || die "broker inventory returned HTTP ${INVENTORY_STATUS}, want 200"

  # Fail closed on the exact completeness contract. In particular, an API
  # channel or any response carrying a limit is a ranked client page, not a
  # fleet inventory, and must never be treated as authoritative merely because
  # it happens to contain valid relay descriptors.
  jq -e '
    type == "object" and
    .channel == "inventory" and
    (has("limit") | not) and
    (.count | type == "number" and . >= 0 and . == floor) and
    (.relays | type == "array") and
    .count == (.relays | length) and
    ([.relays[] | (.id | type == "string" and length > 0)] | all) and
    ([.relays[].id] == ([.relays[].id] | sort)) and
    (([.relays[].id] | unique | length) == (.relays | length))
  ' "${INVENTORY_TMP}/inventory.json" >/dev/null \
    || die "broker response is not a complete, stable relay inventory"

  # The inventory is the only authority on what a relay actually advertises;
  # a pull zone can exist while no relay offers it, which is the normal state
  # mid-rollout and after a rollback.
  jq -n \
    --slurpfile zones "${INVENTORY_TMP}/zone-list.json" \
    --slurpfile inventory "${INVENTORY_TMP}/inventory.json" '
    $zones[0] as $z
    | $inventory[0].relays as $r
    | [ $z[]?
        | . as $zone
        | ("wss://" + ($zone.Hostnames[0].Value // "") + "/api/v1/wss-bridge") as $url
        # Keep an unadvertised zone in the report. It may be an orphan left by
        # a rollback, or evidence that a relay stopped advertising its front.
        | ([$r[] | select(any(.wss_fronts[]?; .url == $url))] | first) as $relay
        | {
            front_host: ($zone.Hostnames[0].Value // null),
            pull_zone_id: $zone.Id,
            origin: $zone.OriginUrl,
            advertised_by: ($relay.label // null),
            front_id: ([$relay.wss_fronts[]? | select(.url == $url) | .id][0] // null),
            node_class: ($relay.node_class // null)
          } ]
    | sort_by(.advertised_by // "~")' "${INVENTORY_TMP}/zones.json"
  exit 0
fi

[[ "$MODE" == create || "$MODE" == audit ]] || die "usage: $0 create|audit RELAY_NAME ORIGIN_HOST FRONT_ID ZONE_NAME | inventory"
[[ "$RELAY_NAME" =~ ^[a-z0-9][a-z0-9-]{0,62}$ ]] || die "relay name is invalid"
[[ "$FRONT_ID" =~ ^[a-z0-9][a-z0-9._-]{0,63}$ ]] || die "front ID is invalid"
[[ "$ORIGIN_HOST" =~ ^[a-z0-9][a-z0-9.-]{0,252}[a-z0-9]$ ]] \
  || die "origin hostname must be a lowercase DNS hostname"
# The origin must be the relay itself.  A pull zone that fronts another pull
# zone would chain two fronts onto one ticket and break the one-relay-one-front
# binding the sidecar enforces.
[[ "$ORIGIN_HOST" != *.b-cdn.net ]] || die "origin must be the relay, not another pull zone"
[[ "$ORIGIN_HOST" != *.cloudfront.net ]] || die "origin must be the relay, not a CloudFront distribution"
# One label, and the exact shape wsscore's no-SNI path accepts: the client
# verifies bunny's default *.b-cdn.net certificate against this host.
[[ "$ZONE_NAME" =~ ^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$ ]] || die "zone name is invalid"
[[ "$ORIGIN_PORT" =~ ^[0-9]{1,5}$ ]] && ((ORIGIN_PORT >= 1024 && ORIGIN_PORT <= 65535)) || die "origin port is invalid"
[[ "$BANDWIDTH_LIMIT" =~ ^[0-9]+$ ]] && ((BANDWIDTH_LIMIT > 0)) \
  || die "monthly bandwidth limit must be a positive byte count"
case "$MAX_WS_CONNECTIONS" in
  500|1000|2500|5000|10000|25000) ;;
  *) die "max WebSocket connections must be a bunny tier: 500, 1000, 2500, 5000, 10000, or 25000" ;;
esac

# Capture the file path in a non-exported shell variable, then remove both the
# supported file variable and any tempting raw-token variable before the first
# child process is started.  The token is materialized only in protected JSON
# files that are piped to the CLI.
TOKEN_FILE=""
if [[ "$MODE" == create ]]; then
  TOKEN_FILE="${OPENRUNG_ORIGIN_TOKEN_FILE:-}"
fi
unset OPENRUNG_ORIGIN_TOKEN OPENRUNG_ORIGIN_TOKEN_FILE ORIGIN_TOKEN
if [[ "$MODE" == create ]]; then
  require_token_file "$TOKEN_FILE" OPENRUNG_ORIGIN_TOKEN_FILE "origin token"
fi
command -v bunny >/dev/null || die "bunny CLI is required"
command -v jq >/dev/null || die "jq is required"

FRONT_HOST="${ZONE_NAME}.b-cdn.net"
ORIGIN_URL="https://${ORIGIN_HOST}:${ORIGIN_PORT}"
# The edge sees the viewer's wss:// request as https://, so the trigger that
# scopes the origin token is written against the https form of the same URL.
BRIDGE_URL="https://${FRONT_HOST}/api/v1/wss-bridge"

TMP_DIR="$(mktemp -d)"
cleanup() { rm -rf "$TMP_DIR"; }
trap cleanup EXIT
umask 077

# `bunny api` prints raw response bodies, and pull-zone bodies can contain the
# origin token in EdgeRules. Never quote either stream in an error: a failed
# request is still allowed to echo the request or current resource.
bunny_api_fail() {
  local method="$1" path="$2"
  die "bunny api ${method} ${path} failed (response withheld)"
}

# stdin is closed explicitly: with no --body and no body piped in, the CLI
# blocks waiting for one, so a plain GET would hang forever whenever the caller
# happened to leave stdin open.
bunny_api() {
  local method="$1" path="$2" out="$3"
  shift 3
  bunny api "$method" "$path" -o json "$@" </dev/null >"$out" 2>"${TMP_DIR}/api.err" \
    || bunny_api_fail "$method" "$path"
  jq -e . "$out" >/dev/null 2>&1 || die "bunny api ${method} ${path} returned a non-JSON body"
}

# Same, with the request body piped in from a file.
#
# The CLI's --body takes the JSON as an argument, which would put a front's
# origin token in the bunny process's argv, readable from the process table by
# anything running as this user. Piping keeps it out: only the file path is ever
# an argument.
#
# It must be a pipe, not `<file`. The CLI reads a body only from a pipe; given a
# regular file on stdin it silently sends none, and the API answers 500 -- which
# looks like a transient server fault rather than a caller mistake. Verified
# both ways against the live API.
bunny_api_body() {
  local method="$1" path="$2" out="$3" body="$4"
  cat "$body" | bunny api "$method" "$path" -o json >"$out" 2>"${TMP_DIR}/api.err" \
    || bunny_api_fail "$method" "$path"
  jq -e . "$out" >/dev/null 2>&1 || die "bunny api ${method} ${path} returned a non-JSON body"
}

bunny_api GET "/pullzone?perPage=1000" "${TMP_DIR}/zones.json"
normalize_pull_zone_list "${TMP_DIR}/zones.json" "${TMP_DIR}/zone-list.json"

# Match on name AND origin together, never either alone. Matching on one was
# actively dangerous: given an existing zone name with a different origin, the
# script adopted that zone's ID, rewrote its origin-token rule, and only then
# failed the structural audit -- by which point the live front it had just
# retagged was already broken, because its relay's sidecar still holds the old
# token. Any partial match is now a conflict, refused before the first write.
LOOKUP="$(jq -r --arg name "$ZONE_NAME" --arg origin "$ORIGIN_URL" '
  . as $zones
  | [$zones[]? | select(.Name == $name)] as $byName
  | [$zones[]? | select(.OriginUrl == $origin)] as $byOrigin
  | if ($byName | length) > 1 or ($byOrigin | length) > 1 then
      "conflict\tmore than one pull zone claims this name or this relay origin"
    elif ($byName | length) == 1 and ($byOrigin | length) == 1 then
      (if ($byName[0].Id) == ($byOrigin[0].Id)
       then "match\t\($byName[0].Id)"
       else "conflict\tname \($name) and origin \($origin) belong to different pull zones" end)
    elif ($byName | length) == 1 then
      "conflict\ta pull zone named \($name) already exists, with origin \($byName[0].OriginUrl // "unset")"
    elif ($byOrigin | length) == 1 then
      "conflict\tpull zone \($byOrigin[0].Name) already fronts \($origin)"
    else
      "absent\t"
    end
' "${TMP_DIR}/zone-list.json")"
LOOKUP_STATUS="${LOOKUP%%	*}"
LOOKUP_DETAIL="${LOOKUP#*	}"
[[ "$LOOKUP_STATUS" != conflict ]] || die "$LOOKUP_DETAIL"
EXISTING_ID=""
[[ "$LOOKUP_STATUS" != match ]] || EXISTING_ID="$LOOKUP_DETAIL"

# This is the complete mutable contract. The same object is used for creation,
# convergence, and audit so a newly added setting cannot quietly be omitted
# from either of the latter paths.
jq -n \
  --arg originUrl "$ORIGIN_URL" \
  --arg originHost "$ORIGIN_HOST" \
  --argjson limit "$BANDWIDTH_LIMIT" \
  --argjson maxSockets "$MAX_WS_CONNECTIONS" '
  {
    OriginUrl:$originUrl,
    Type:0,
    EnableWebSockets:true,
    MaxWebSocketConnections:$maxSockets,
    EnableLogging:false,
    LoggingIPAnonymizationEnabled:true,
    VerifyOriginSSL:true,
    AddHostHeader:false,
    OriginHostHeader:$originHost,
    CacheControlMaxAgeOverride:0,
    CacheControlPublicMaxAgeOverride:0,
    CacheErrorResponses:false,
    DisableCookies:true,
    EnableGeoZoneUS:true, EnableGeoZoneEU:true, EnableGeoZoneASIA:true,
    EnableGeoZoneSA:true, EnableGeoZoneAF:true,
    MonthlyBandwidthLimit:$limit
  }' >"${TMP_DIR}/desired-settings.json"

# Verify everything that cannot be safely converged before the first write.
# ExtraActions are intentionally forbidden: they share a rule's triggers and
# can otherwise hide SetResponseHeader or OriginUrl actions behind a harmless
# top-level SetRequestHeader action.
preflight_zone() { # state-file
  local state="$1"
  jq -e \
    --argjson id "$ZONE_ID" \
    --arg name "$ZONE_NAME" \
    --arg origin "$ORIGIN_URL" \
    --arg front "$FRONT_HOST" \
    --arg originHeader "$ORIGIN_TOKEN_HEADER" \
    --arg viewerHeader "$VIEWER_ADDRESS_HEADER" \
    --argjson request "$ACTION_SET_REQUEST_HEADER" \
    --argjson response "$ACTION_SET_RESPONSE_HEADER" \
    --argjson override "$ACTION_OVERRIDE_ORIGIN" '
    type == "object" and
    .Id == $id and .Name == $name and .OriginUrl == $origin and
    .Enabled == true and .Suspended == false and
    (.Hostnames | type == "array" and length == 1) and
    .Hostnames[0].Value == $front and
    (.EdgeRules == null or (.EdgeRules | type == "array")) and
    ([.EdgeRules[]? | select(.ActionType == $response or .ActionType == $override)] | length) == 0 and
    ([.EdgeRules[]? | select(
      (.ExtraActions != null and
       ((.ExtraActions | type != "array") or (.ExtraActions | length) != 0))
    )] | length) == 0 and
    ([.EdgeRules[]? | select(.Enabled == true and
      ((.ActionType == $request and
        (.ActionParameter1 == $originHeader or .ActionParameter1 == $viewerHeader)) | not)
    )] | length) == 0 and
    ([.EdgeRules[]? | select(.ActionType == $request and .ActionParameter1 == $originHeader)] | length) <= 1 and
    ([.EdgeRules[]? | select(.ActionType == $request and .ActionParameter1 == $viewerHeader)] | length) <= 1 and
    ([.EdgeRules[]? | select(.ActionType == $request and
      (.ActionParameter1 == $originHeader or .ActionParameter1 == $viewerHeader) and
      ((.Guid | type != "string") or (.Guid | length) == 0 or .ReadOnly == true)
    )] | length) == 0
  ' "$state" >/dev/null 2>&1 || die "pull zone failed preflight safety checks; no changes were made"
}

settings_match() { # state-file
  jq -e --slurpfile desired "${TMP_DIR}/desired-settings.json" '
    . as $zone
    | all($desired[0] | to_entries[]; $zone[.key] == .value)
  ' "$1" >/dev/null 2>&1
}

if [[ "$MODE" == audit ]]; then
  [[ -n "$EXISTING_ID" ]] || die "no pull zone named ${ZONE_NAME} points to ${ORIGIN_URL}"
  ZONE_ID="$EXISTING_ID"
  bunny_api GET "/pullzone/${ZONE_ID}" "${TMP_DIR}/zone-before.json"
else
  if [[ -n "$EXISTING_ID" ]]; then
    ZONE_ID="$EXISTING_ID"
    bunny_api GET "/pullzone/${ZONE_ID}" "${TMP_DIR}/zone-before.json"
    preflight_zone "${TMP_DIR}/zone-before.json"
  else
    jq --arg name "$ZONE_NAME" '. + {Name:$name}' \
      "${TMP_DIR}/desired-settings.json" >"${TMP_DIR}/zone-create.json"
    bunny_api_body POST /pullzone "${TMP_DIR}/created.json" "${TMP_DIR}/zone-create.json"
    ZONE_ID="$(jq -r '
      if type == "object" and (.Id | type == "number" and . == floor and . > 0)
      then (.Id | tostring) else empty end
    ' "${TMP_DIR}/created.json")"
    [[ -n "$ZONE_ID" ]] || die "pull zone creation returned no valid ID"
    bunny_api GET "/pullzone/${ZONE_ID}" "${TMP_DIR}/zone-before.json"
    preflight_zone "${TMP_DIR}/zone-before.json"
  fi

  # Update only when the mutable contract drifted. A fresh GET then becomes the
  # sole source for rule GUIDs, avoiding stale state after the settings write.
  if ! settings_match "${TMP_DIR}/zone-before.json"; then
    bunny_api_body POST "/pullzone/${ZONE_ID}" \
      "${TMP_DIR}/zone-updated.json" "${TMP_DIR}/desired-settings.json"
    bunny_api GET "/pullzone/${ZONE_ID}" "${TMP_DIR}/zone-after-settings.json"
    preflight_zone "${TMP_DIR}/zone-after-settings.json"
    cp "${TMP_DIR}/zone-after-settings.json" "${TMP_DIR}/zone-before.json"
  fi

  # addOrUpdate appends unless the existing Guid is supplied. Generate the
  # desired rule from a file-backed value, compare it without putting that
  # value in argv, and write only when the rule actually differs.
  upsert_request_header_rule() { # header value-file description
    local header="$1" value_file="$2" description="$3" guid
    guid="$(jq -r --argjson action "$ACTION_SET_REQUEST_HEADER" --arg header "$header" '
      [.EdgeRules[]? | select(.ActionType == $action and .ActionParameter1 == $header) | .Guid]
      | if length == 0 then "" else .[0] end
    ' "${TMP_DIR}/zone-before.json")"
    jq -Rs 'rtrimstr("\n")' "$value_file" >"${TMP_DIR}/value.json"
    jq -n \
      --arg guid "$guid" \
      --argjson action "$ACTION_SET_REQUEST_HEADER" \
      --arg header "$header" \
      --arg description "$description" \
      --arg trigger "$BRIDGE_URL" \
      --slurpfile value "${TMP_DIR}/value.json" '
      {
        ActionType:$action,
        ActionParameter1:$header,
        ActionParameter2:$value[0],
        Triggers:[{Type:0,PatternMatches:[$trigger],PatternMatchingType:0,Parameter1:null}],
        ExtraActions:[],
        TriggerMatchingType:0,
        Description:$description,
        Enabled:true
      }
      | if $guid == "" then . else .Guid = $guid end' >"${TMP_DIR}/rule.json"

    if jq -e --slurpfile desired "${TMP_DIR}/rule.json" --arg header "$header" \
      --argjson action "$ACTION_SET_REQUEST_HEADER" '
      [.EdgeRules[]? | select(.ActionType == $action and .ActionParameter1 == $header)] as $found
      | $desired[0] as $want
      | ($found | length) == 1 and
        ($found[0] | .ActionType == $want.ActionType and
          .ActionParameter1 == $want.ActionParameter1 and
          .ActionParameter2 == $want.ActionParameter2 and
          .Enabled == $want.Enabled and
          .Description == $want.Description and
          .TriggerMatchingType == $want.TriggerMatchingType and
          (.ExtraActions == null or
            ((.ExtraActions | type == "array") and (.ExtraActions | length) == 0)) and
          (.Triggers | type == "array" and length == 1) and
          .Triggers[0].Type == $want.Triggers[0].Type and
          .Triggers[0].PatternMatches == $want.Triggers[0].PatternMatches and
          .Triggers[0].PatternMatchingType == $want.Triggers[0].PatternMatchingType and
          .Triggers[0].Parameter1 == $want.Triggers[0].Parameter1)
    ' "${TMP_DIR}/zone-before.json" >/dev/null 2>&1; then
      return
    fi

    bunny_api_body POST "/pullzone/${ZONE_ID}/edgerules/addOrUpdate" \
      "${TMP_DIR}/rule-created.json" "${TMP_DIR}/rule.json"
  }

  upsert_request_header_rule "$ORIGIN_TOKEN_HEADER" "$TOKEN_FILE" \
    "OpenRung origin token for front ${FRONT_ID}"
  printf '%s' "$VIEWER_ADDRESS_VALUE" >"${TMP_DIR}/viewer.txt"
  upsert_request_header_rule "$VIEWER_ADDRESS_HEADER" "${TMP_DIR}/viewer.txt" \
    "OpenRung viewer address for front ${FRONT_ID}"
fi

bunny_api GET "/pullzone/${ZONE_ID}" "${TMP_DIR}/zone-state.json"

# Audit the complete declared zone contract and every matching field of both
# exact-URL triggers. Disabled rules remain subject to the response/origin and
# ExtraActions bans so an unsafe dormant rule cannot be enabled later.
jq -e \
  --argjson id "$ZONE_ID" \
  --arg name "$ZONE_NAME" \
  --arg frontHost "$FRONT_HOST" \
  --arg bridge "$BRIDGE_URL" \
  --arg header "$ORIGIN_TOKEN_HEADER" \
  --arg viewerHeader "$VIEWER_ADDRESS_HEADER" \
  --arg viewerValue "$VIEWER_ADDRESS_VALUE" \
  --argjson request "$ACTION_SET_REQUEST_HEADER" \
  --argjson response "$ACTION_SET_RESPONSE_HEADER" \
  --argjson override "$ACTION_OVERRIDE_ORIGIN" \
  --slurpfile desired "${TMP_DIR}/desired-settings.json" '
  def no_extra_actions:
    .ExtraActions == null or
    ((.ExtraActions | type == "array") and (.ExtraActions | length) == 0);
  def exact_trigger:
    .TriggerMatchingType == 0 and
    (.Triggers | type == "array" and length == 1) and
    (.Triggers[0] | type == "object") and
    (.Triggers[0] | has("Type") and .Type == 0) and
    (.Triggers[0] | has("PatternMatches") and .PatternMatches == [$bridge]) and
    (.Triggers[0] | has("PatternMatchingType") and .PatternMatchingType == 0) and
    (.Triggers[0] | has("Parameter1") and .Parameter1 == null);
  def managed_rule($managedHeader):
    .Enabled == true and
    .ActionType == $request and .ActionParameter1 == $managedHeader and
    no_extra_actions and exact_trigger;

  . as $zone
  | type == "object" and
    .Id == $id and .Name == $name and
    .Enabled == true and .Suspended == false and
    all($desired[0] | to_entries[]; $zone[.key] == .value) and
    (.Hostnames | type == "array" and length == 1) and
    .Hostnames[0].Value == $frontHost and
    (.EdgeRules | type == "array") and
    all(.EdgeRules[]; no_extra_actions) and
    ([.EdgeRules[] | select(.ActionType == $response or .ActionType == $override)] | length) == 0 and
    ([.EdgeRules[] | select(.Enabled == true)] | length) == 2 and
    ([.EdgeRules[] | select(.ActionType == $request and .ActionParameter1 == $header)] | length) == 1 and
    ([.EdgeRules[] | select(.ActionType == $request and .ActionParameter1 == $viewerHeader)] | length) == 1 and
    ([.EdgeRules[] | select(managed_rule($header) and
      (.ActionParameter2 | type == "string" and length >= 32 and length <= 512))] | length) == 1 and
    ([.EdgeRules[] | select(managed_rule($viewerHeader) and
      .ActionParameter2 == $viewerValue)] | length) == 1
' "${TMP_DIR}/zone-state.json" >/dev/null 2>&1 || die "pull zone failed the structural audit"

# In create mode, length is not enough: rotation succeeds only when the live
# rule equals the requested file-backed value exactly. --slurpfile keeps the
# value out of jq's argv and environment.
if [[ "$MODE" == create ]]; then
  jq -Rs 'rtrimstr("\n")' "$TOKEN_FILE" >"${TMP_DIR}/expected-token.json"
  jq -e \
    --arg header "$ORIGIN_TOKEN_HEADER" \
    --argjson action "$ACTION_SET_REQUEST_HEADER" \
    --slurpfile expected "${TMP_DIR}/expected-token.json" '
    [.EdgeRules[] | select(.Enabled == true and .ActionType == $action and
      .ActionParameter1 == $header and .ActionParameter2 == $expected[0])]
    | length == 1
  ' "${TMP_DIR}/zone-state.json" >/dev/null 2>&1 \
    || die "origin token did not converge to the requested value"
fi

jq -n \
  --arg relay "$RELAY_NAME" --arg origin "$ORIGIN_HOST" --arg front "$FRONT_ID" \
  --arg zone "$ZONE_NAME" --arg id "$ZONE_ID" --arg host "$FRONT_HOST" \
  '{relay:$relay,origin_host:$origin,front_id:$front,provider:"bunny",
    zone_name:$zone,pull_zone_id:($id|tonumber),front_host:$host,
    url:("wss://"+$host+"/api/v1/wss-bridge")}' \
  >"${TMP_DIR}/public-state.json"
if [[ -n "$STATE_FILE" ]]; then
  install -m 0600 "${TMP_DIR}/public-state.json" "$STATE_FILE"
fi
jq . "${TMP_DIR}/public-state.json"
