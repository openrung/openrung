#!/usr/bin/env bash
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="${HERE}/azure-front-door-up.sh"
TEST_TMP="$(mktemp -d)"
trap 'rm -rf "$TEST_TMP"' EXIT

PASS=0
FAIL=0

pass() { PASS=$((PASS + 1)); }
fail() {
  echo "FAIL: $*" >&2
  FAIL=$((FAIL + 1))
}

assert_file_contains() { # file, needle, context
  local file="$1" needle="$2" context="$3"
  if grep -Fq -- "$needle" "$file"; then
    pass
  else
    fail "${context}: missing '${needle}'"
  fi
}

assert_file_not_contains() { # file, needle, context
  local file="$1" needle="$2" context="$3"
  if grep -Fq -- "$needle" "$file"; then
    fail "${context}: unexpectedly contained '${needle}'"
  else
    pass
  fi
}

mock_arg() { # option, argv...
  local wanted="$1"
  shift
  while [ "$#" -gt 0 ]; do
    if [ "$1" = "$wanted" ]; then
      [ "$#" -ge 2 ] || return 1
      printf '%s' "$2"
      return 0
    fi
    shift
  done
  return 0
}

run_scenario() ( # scenario, calls file
  local scenario="$1"
  MOCK_CALLS="$2"

  # Operator shells commonly have these overrides exported. Each scenario
  # starts from the script defaults so the matrix is hermetic; scenarios that
  # exercise an override set it explicitly below.
  unset OPENRUNG_AZURE_RG OPENRUNG_AZURE_LOCATION OPENRUNG_AZURE_PROFILE
  unset OPENRUNG_AZURE_ENDPOINT OPENRUNG_BROKER_ORIGIN

  MOCK_ENDPOINT_ENABLED=Enabled
  MOCK_HOSTNAME=cdn-edge-test.z02.azurefd.net
  MOCK_ENDPOINT_NAMES=cdn-edge
  MOCK_PROBE_REQUEST=GET
  MOCK_PROBE_PROTOCOL=Https
  MOCK_PROBE_PATH=/healthz
  MOCK_PROBE_INTERVAL=100
  MOCK_SAMPLE_SIZE=4
  MOCK_SUCCESSFUL_SAMPLES=3
  MOCK_ADDITIONAL_LATENCY=50
  MOCK_ORIGIN_GROUP_NAMES=broker-origin
  MOCK_ORIGIN_HOST=broker-origin.openrung.org
  MOCK_ORIGIN_HEADER=broker-origin.openrung.org
  MOCK_ORIGIN_HTTPS_PORT=443
  MOCK_ORIGIN_PRIORITY=1
  MOCK_ORIGIN_WEIGHT=1000
  MOCK_ORIGIN_CERT_CHECK=true
  MOCK_ORIGIN_ENABLED=Enabled
  MOCK_ORIGIN_NAMES=typhoon-broker
  MOCK_ROUTE_ENABLED=Enabled
  MOCK_ROUTE_ORIGIN_GROUP=/subscriptions/test/resourceGroups/test/providers/Microsoft.Cdn/profiles/test/originGroups/broker-origin
  MOCK_ROUTE_PROTOCOLS=Https
  MOCK_ROUTE_PATTERNS='/*'
  MOCK_ROUTE_FORWARDING=HttpsOnly
  MOCK_ROUTE_REDIRECT=Disabled
  MOCK_ROUTE_DEFAULT_DOMAIN=Enabled
  MOCK_ROUTE_CACHE=null
  MOCK_ROUTE_CUSTOM_DOMAINS='[]'
  MOCK_ROUTE_RULE_SETS='[]'
  MOCK_ROUTE_ORIGIN_PATH=null
  MOCK_ROUTE_NAMES=broker-api
  MOCK_PROFILE_CUSTOM_DOMAINS=
  MOCK_STICKY_CERT=0

  case "$scenario" in
    safe) ;;
    cert-drift)
      MOCK_ORIGIN_CERT_CHECK=false
      ;;
    extra-endpoint)
      MOCK_ENDPOINT_NAMES=cdn-edge,shadow-endpoint
      ;;
    replacement-endpoint)
      export OPENRUNG_AZURE_ENDPOINT=replacement-edge
      MOCK_ENDPOINT_NAMES=cdn-edge
      ;;
    extra-origin-group)
      MOCK_ORIGIN_GROUP_NAMES=broker-origin,shadow-origin
      ;;
    cert-update-does-not-stick)
      MOCK_ORIGIN_CERT_CHECK=false
      MOCK_STICKY_CERT=1
      ;;
    mutable-route-drift)
      MOCK_ROUTE_ENABLED=Disabled
      MOCK_ROUTE_ORIGIN_GROUP=/subscriptions/test/resourceGroups/test/providers/Microsoft.Cdn/profiles/test/originGroups/wrong-origin
      MOCK_ROUTE_PROTOCOLS=Http,Https
      MOCK_ROUTE_PATTERNS='/api/*'
      MOCK_ROUTE_FORWARDING=MatchRequest
      MOCK_ROUTE_REDIRECT=Enabled
      MOCK_ROUTE_DEFAULT_DOMAIN=Disabled
      ;;
    cached-route)
      MOCK_ROUTE_CACHE='{"queryStringCachingBehavior":"UseQueryString"}'
      ;;
    custom-domain-route)
      MOCK_ROUTE_CUSTOM_DOMAINS='[{"id":"/profiles/test/customDomains/unsafe"}]'
      ;;
    *)
      echo "unknown test scenario ${scenario}" >&2
      return 2
      ;;
  esac

  az() {
    local key query
    printf 'az %s\n' "$*" >> "$MOCK_CALLS"
    key="${1:-}:${2:-}:${3:-}"
    query="$(mock_arg --query "$@")"

    case "$key" in
      account:show:*|provider:register:*|group:create:*)
        return 0
        ;;
      afd:profile:show)
        if [ -n "$query" ]; then printf '%s' Standard_AzureFrontDoor; fi
        ;;
      afd:endpoint:show)
        if [ -n "$query" ]; then
          printf '%s|%s' "$MOCK_ENDPOINT_ENABLED" "$MOCK_HOSTNAME"
        fi
        ;;
      afd:endpoint:list)
        printf '%s' "$MOCK_ENDPOINT_NAMES"
        ;;
      afd:endpoint:update)
        MOCK_ENDPOINT_ENABLED=Enabled
        ;;
      afd:origin-group:show)
        if [ -n "$query" ]; then
          printf '%s|%s|%s|%s|%s|%s|%s' \
            "$MOCK_PROBE_REQUEST" "$MOCK_PROBE_PROTOCOL" "$MOCK_PROBE_PATH" \
            "$MOCK_PROBE_INTERVAL" "$MOCK_SAMPLE_SIZE" \
            "$MOCK_SUCCESSFUL_SAMPLES" "$MOCK_ADDITIONAL_LATENCY"
        fi
        ;;
      afd:origin-group:update)
        MOCK_PROBE_REQUEST=GET
        MOCK_PROBE_PROTOCOL=Https
        MOCK_PROBE_PATH=/healthz
        MOCK_PROBE_INTERVAL=100
        MOCK_SAMPLE_SIZE=4
        MOCK_SUCCESSFUL_SAMPLES=3
        MOCK_ADDITIONAL_LATENCY=50
        ;;
      afd:origin-group:list)
        printf '%s' "$MOCK_ORIGIN_GROUP_NAMES"
        ;;
      afd:origin:show)
        if [ -n "$query" ]; then
          printf '%s|%s|%s|%s|%s|%s|%s' \
            "$MOCK_ORIGIN_HOST" "$MOCK_ORIGIN_HEADER" \
            "$MOCK_ORIGIN_HTTPS_PORT" "$MOCK_ORIGIN_PRIORITY" \
            "$MOCK_ORIGIN_WEIGHT" "$MOCK_ORIGIN_CERT_CHECK" \
            "$MOCK_ORIGIN_ENABLED"
        fi
        ;;
      afd:origin:update)
        MOCK_ORIGIN_HOST=broker-origin.openrung.org
        MOCK_ORIGIN_HEADER=broker-origin.openrung.org
        MOCK_ORIGIN_HTTPS_PORT=443
        MOCK_ORIGIN_PRIORITY=1
        MOCK_ORIGIN_WEIGHT=1000
        if [ "$MOCK_STICKY_CERT" != 1 ]; then MOCK_ORIGIN_CERT_CHECK=true; fi
        MOCK_ORIGIN_ENABLED=Enabled
        ;;
      afd:origin:list)
        printf '%s' "$MOCK_ORIGIN_NAMES"
        ;;
      afd:route:show)
        if [ -z "$query" ]; then
          return 0
        elif [[ "$query" == *enabledState* ]]; then
          printf '%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s' \
            "$MOCK_ROUTE_ENABLED" "$MOCK_ROUTE_ORIGIN_GROUP" \
            "$MOCK_ROUTE_PROTOCOLS" "$MOCK_ROUTE_PATTERNS" \
            "$MOCK_ROUTE_FORWARDING" "$MOCK_ROUTE_REDIRECT" \
            "$MOCK_ROUTE_DEFAULT_DOMAIN" "$MOCK_ROUTE_CACHE" \
            "$MOCK_ROUTE_CUSTOM_DOMAINS" "$MOCK_ROUTE_RULE_SETS" \
            "$MOCK_ROUTE_ORIGIN_PATH"
        else
          printf '%s|%s|%s|%s' "$MOCK_ROUTE_CACHE" \
            "$MOCK_ROUTE_CUSTOM_DOMAINS" "$MOCK_ROUTE_RULE_SETS" \
            "$MOCK_ROUTE_ORIGIN_PATH"
        fi
        ;;
      afd:route:update)
        MOCK_ROUTE_ENABLED=Enabled
        MOCK_ROUTE_ORIGIN_GROUP=/subscriptions/test/resourceGroups/test/providers/Microsoft.Cdn/profiles/test/originGroups/broker-origin
        MOCK_ROUTE_PROTOCOLS=Https
        MOCK_ROUTE_PATTERNS='/*'
        MOCK_ROUTE_FORWARDING=HttpsOnly
        MOCK_ROUTE_REDIRECT=Disabled
        MOCK_ROUTE_DEFAULT_DOMAIN=Enabled
        ;;
      afd:route:list)
        printf '%s' "$MOCK_ROUTE_NAMES"
        ;;
      afd:custom-domain:list)
        printf '%s' "$MOCK_PROFILE_CUSTOM_DOMAINS"
        ;;
      afd:*:create)
        echo "unexpected create in existing-resource scenario: az $*" >&2
        return 97
        ;;
      *)
        echo "unhandled mock az invocation: az $*" >&2
        return 98
        ;;
    esac
  }

  curl() {
    printf 'curl %s\n' "$*" >> "$MOCK_CALLS"
    return 0
  }

  # shellcheck source=azure-front-door-up.sh
  source "$SCRIPT"
  main
)

expect_success() { # scenario
  local scenario="$1" output calls
  output="${TEST_TMP}/${scenario}.output"
  calls="${TEST_TMP}/${scenario}.calls"
  : > "$calls"
  if run_scenario "$scenario" "$calls" >"$output" 2>&1; then
    pass
  else
    fail "${scenario}: provisioning unexpectedly failed; output: $(<"$output")"
  fi
}

expect_failure() { # scenario, expected output
  local scenario="$1" wanted="$2" output calls
  output="${TEST_TMP}/${scenario}.output"
  calls="${TEST_TMP}/${scenario}.calls"
  : > "$calls"
  if run_scenario "$scenario" "$calls" >"$output" 2>&1; then
    fail "${scenario}: provisioning unexpectedly succeeded"
  else
    pass
  fi
  assert_file_contains "$output" "$wanted" "${scenario} diagnostic"
}

expect_success safe
assert_file_contains "${TEST_TMP}/safe.calls" "az afd endpoint update" "safe rerun reconciles endpoint"
assert_file_contains "${TEST_TMP}/safe.calls" "az afd origin-group update" "safe rerun reconciles origin group"
assert_file_contains "${TEST_TMP}/safe.calls" "az afd origin update" "safe rerun reconciles origin"
assert_file_contains "${TEST_TMP}/safe.calls" "az afd route update" "safe rerun reconciles route"
assert_file_not_contains "${TEST_TMP}/safe.calls" "az afd profile create" "safe rerun preserves profile"

expect_success cert-drift
assert_file_contains "${TEST_TMP}/cert-drift.calls" \
  "--enforce-certificate-name-check true" "certificate-name check is reconciled"

expect_failure extra-endpoint \
  "profile openrung-broker-front has endpoints=cdn-edge,shadow-endpoint; want cdn-edge"
assert_file_not_contains "${TEST_TMP}/extra-endpoint.calls" \
  "az afd endpoint update" "extra endpoint fails before endpoint mutation"

expect_failure replacement-endpoint \
  "profile openrung-broker-front has endpoints=cdn-edge; want replacement-edge"
assert_file_not_contains "${TEST_TMP}/replacement-endpoint.calls" \
  "az afd endpoint create" "replacement endpoint fails before leaving an orphan"

expect_failure extra-origin-group \
  "profile openrung-broker-front has originGroups=broker-origin,shadow-origin; want broker-origin"

expect_failure cert-update-does-not-stick \
  "origin typhoon-broker has enforceCertificateNameCheck=false; want true"

expect_success mutable-route-drift
assert_file_contains "${TEST_TMP}/mutable-route-drift.calls" \
  "--supported-protocols Https" "route protocols are reconciled"
assert_file_contains "${TEST_TMP}/mutable-route-drift.calls" \
  "--forwarding-protocol HttpsOnly" "route forwarding is reconciled"

expect_failure cached-route "route broker-api has cacheConfiguration="
assert_file_not_contains "${TEST_TMP}/cached-route.calls" \
  "az afd route update" "cached route fails before route mutation"

expect_failure custom-domain-route "route broker-api has customDomains="
assert_file_not_contains "${TEST_TMP}/custom-domain-route.calls" \
  "az afd route update" "custom-domain route fails before route mutation"

if [ "$FAIL" -ne 0 ]; then
  echo "${FAIL} assertion(s) failed; ${PASS} passed" >&2
  exit 1
fi
echo "${PASS} assertions passed"
