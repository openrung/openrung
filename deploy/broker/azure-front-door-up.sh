#!/usr/bin/env bash
#
# Provision the Azure Front Door Standard broker front (front #3).
#
#   deploy/broker/azure-front-door-up.sh
#
# Front Door is a CONTROL-PLANE front only: it proxies the broker's HTTP API
# (relay directory, registrations, telemetry ingest). Relay traffic must never
# pass through it — the sponsorship grant buys roughly 24 TB/year, about 29 days
# of fleet traffic, and Front Door egress is billed at a higher rate than VM
# egress on top of a fixed monthly base fee.
#
# Clients reach this front WITHOUT SNI, selecting the endpoint through the
# encrypted HTTP Host header. That is why two settings below are load-bearing
# rather than stylistic:
#
#   * No custom domain. Without SNI the edge serves its shared default
#     certificate whatever the Host, so a custom domain would be no better
#     authenticated while losing the ordinary verification it gets by keeping
#     SNI. See deploy/broker/azure-front-door.md for the full tradeoff.
#   * Caching disabled on the route. A cached relay list still passes signature
#     verification — the signature covers a 30-minute window — so a caching
#     front is invisible to every check except frontcheck's freshness bound. A
#     Cloudflare edge once served this deployment a stale list for four hours.
#
# The origin is the broker's own TLS name, NOT a CDN front and NOT a bare IP:
# the edge->origin leg must validate a real certificate, and broker-origin
# resolves DNS-only straight to the Lightsail box.
#
# Prerequisites: an authenticated `az` CLI (az login) on a subscription with
# Azure Front Door Standard profile quota. A sponsorship subscription may refuse
# profile creation with "The number of profiles created exceeds quota" even at
# zero profiles; that needs a free quota request in the portal first.
#
# Overridable via env: OPENRUNG_AZURE_RG, OPENRUNG_AZURE_LOCATION,
# OPENRUNG_AZURE_PROFILE, OPENRUNG_AZURE_ENDPOINT, OPENRUNG_BROKER_ORIGIN.
#
# This helper is idempotent: re-running it reconciles mutable settings and then
# reads every routing- and TLS-relevant property back. It fails closed on
# incompatible additions such as caching, custom domains, and rule sets.

set -euo pipefail

RG="${OPENRUNG_AZURE_RG:-openrung-broker-front}"
LOCATION="${OPENRUNG_AZURE_LOCATION:-japaneast}"
PROFILE="${OPENRUNG_AZURE_PROFILE:-openrung-broker-front}"
# Keep this prefix GENERIC and free of anything naming this project. Suppressing
# SNI keeps the endpoint name out of the ClientHello, but the client still
# resolves it over ordinary cleartext DNS, so the name is the one part of this
# front a passive observer sees. A prefix like "openrung-broker" turns that query
# into a keyword match — enough to blocklist the front by pattern, and enough to
# mark the user as running this software. Azure appends an unguessable suffix, so
# a boring prefix costs nothing. The resource group and profile names below are
# never on the wire and stay descriptive on purpose.
ENDPOINT="${OPENRUNG_AZURE_ENDPOINT:-cdn-edge}"
ORIGIN_HOST="${OPENRUNG_BROKER_ORIGIN:-broker-origin.openrung.org}"

ORIGIN_GROUP="broker-origin"
ORIGIN_NAME="typhoon-broker"
ROUTE="broker-api"

log() { printf '\n==> %s\n' "$*"; }

die() {
  echo "ERROR: $*" >&2
  exit 1
}

# Keep all configuration comparisons independent of the caller's Azure CLI
# output default. The joined queries below use "|" only between fields whose
# allowed values cannot contain it.
azure_tsv() {
  local value
  if ! value="$(az "$@" -o tsv | tr -d '\r')"; then
    die "Azure CLI query failed: az $*"
  fi
  printf '%s' "$value"
}

assert_setting() { # resource, setting, wanted, actual
  local resource="$1" setting="$2" wanted="$3" actual="$4"
  if [ "$actual" != "$wanted" ]; then
    die "${resource} has ${setting}=${actual:-<empty>}; want ${wanted}"
  fi
}

assert_nullish() { # resource, setting, actual
  local resource="$1" setting="$2" actual="$3"
  case "$actual" in
    ''|null|None) ;;
    *) die "${resource} has ${setting}=${actual}; it must be absent. Remove it deliberately, then rerun" ;;
  esac
}

assert_empty_list() { # resource, setting, actual
  local resource="$1" setting="$2" actual="$3"
  case "$actual" in
    ''|null|None|'[]') ;;
    *) die "${resource} has ${setting}=${actual}; it must be empty. Remove it deliberately, then rerun" ;;
  esac
}

require_cli() {
  if ! command -v az >/dev/null 2>&1; then
    echo "az CLI not found; install it and run 'az login'" >&2
    exit 1
  fi
  if ! az account show >/dev/null 2>&1; then
    echo "az CLI is not authenticated; run 'az login'" >&2
    exit 1
  fi
}

# The edge->origin leg validates the origin certificate, so a broken origin
# surfaces here as a clear message rather than as 502s through the front later.
check_origin() {
  log "Checking origin https://${ORIGIN_HOST}/healthz"
  if ! curl -fsS --max-time 15 "https://${ORIGIN_HOST}/healthz" >/dev/null; then
    echo "origin ${ORIGIN_HOST} did not serve /healthz over HTTPS." >&2
    echo "Front Door validates the origin certificate, so fix this first." >&2
    exit 1
  fi
  echo "origin healthy"
}

main() {
  require_cli
  check_origin

  log "Registering Microsoft.Cdn"
  az provider register --namespace Microsoft.Cdn --wait

  log "Resource group ${RG} (${LOCATION})"
  az group create -n "$RG" -l "$LOCATION" -o none

  log "Front Door Standard profile ${PROFILE}"
  if az afd profile show -g "$RG" --profile-name "$PROFILE" -o none 2>/dev/null; then
    echo "profile already exists"
  else
    az afd profile create -g "$RG" --profile-name "$PROFILE" \
      --sku Standard_AzureFrontDoor -o none
  fi
  profile_sku="$(azure_tsv afd profile show -g "$RG" \
    --profile-name "$PROFILE" --query sku.name)"
  assert_setting "profile ${PROFILE}" sku Standard_AzureFrontDoor "$profile_sku"
  echo "profile configuration verified"

  log "Endpoint ${ENDPOINT}"
  # Endpoint names are immutable. Refuse a changed name before creating or
  # updating anything in the profile; otherwise a rotation attempt would leave
  # behind a new, unconfigured endpoint and only then discover the old one.
  # Use a fresh dedicated profile for a staged replacement (see the runbook).
  endpoint_names="$(azure_tsv afd endpoint list -g "$RG" --profile-name "$PROFILE" \
    --query "join(',', sort([*].name))")"
  if [ -n "$endpoint_names" ]; then
    assert_setting "profile ${PROFILE}" endpoints "$ENDPOINT" "$endpoint_names"
  fi
  if az afd endpoint show -g "$RG" --profile-name "$PROFILE" \
      --endpoint-name "$ENDPOINT" -o none 2>/dev/null; then
    echo "endpoint already exists; reconciling enabled state"
    az afd endpoint update -g "$RG" --profile-name "$PROFILE" \
      --endpoint-name "$ENDPOINT" --enabled-state Enabled -o none
  else
    az afd endpoint create -g "$RG" --profile-name "$PROFILE" \
      --endpoint-name "$ENDPOINT" --enabled-state Enabled -o none
  fi
  endpoint_state="$(azure_tsv afd endpoint show -g "$RG" \
    --profile-name "$PROFILE" --endpoint-name "$ENDPOINT" \
    --query "join('|', [to_string(enabledState), to_string(hostName)])")"
  IFS='|' read -r endpoint_enabled hostname <<< "$endpoint_state"
  assert_setting "endpoint ${ENDPOINT}" enabledState Enabled "$endpoint_enabled"
  case "$hostname" in
    *.azurefd.net) ;;
    *) die "endpoint ${ENDPOINT} has unexpected Azure hostname ${hostname:-<empty>}" ;;
  esac
  endpoint_names="$(azure_tsv afd endpoint list -g "$RG" --profile-name "$PROFILE" \
    --query "join(',', sort([*].name))")"
  assert_setting "profile ${PROFILE}" endpoints "$ENDPOINT" "$endpoint_names"
  echo "endpoint configuration verified"

  # Probe /healthz rather than the default "/" so a probe failure means the
  # broker is unhealthy, not merely that the root path has no handler.
  log "Origin group ${ORIGIN_GROUP}"
  if az afd origin-group show -g "$RG" --profile-name "$PROFILE" \
      --origin-group-name "$ORIGIN_GROUP" -o none 2>/dev/null; then
    echo "origin group already exists; reconciling probe and load balancing"
    az afd origin-group update -g "$RG" --profile-name "$PROFILE" \
      --origin-group-name "$ORIGIN_GROUP" \
      --probe-request-type GET --probe-protocol Https --probe-path /healthz \
      --probe-interval-in-seconds 100 \
      --sample-size 4 --successful-samples-required 3 \
      --additional-latency-in-milliseconds 50 -o none
  else
    az afd origin-group create -g "$RG" --profile-name "$PROFILE" \
      --origin-group-name "$ORIGIN_GROUP" \
      --probe-request-type GET --probe-protocol Https --probe-path /healthz \
      --probe-interval-in-seconds 100 \
      --sample-size 4 --successful-samples-required 3 \
      --additional-latency-in-milliseconds 50 -o none
  fi
  origin_group_state="$(azure_tsv afd origin-group show -g "$RG" \
    --profile-name "$PROFILE" --origin-group-name "$ORIGIN_GROUP" \
    --query "join('|', [to_string(healthProbeSettings.probeRequestType), to_string(healthProbeSettings.probeProtocol), to_string(healthProbeSettings.probePath), to_string(healthProbeSettings.probeIntervalInSeconds), to_string(loadBalancingSettings.sampleSize), to_string(loadBalancingSettings.successfulSamplesRequired), to_string(loadBalancingSettings.additionalLatencyInMilliseconds)])")"
  IFS='|' read -r probe_request probe_protocol probe_path probe_interval \
    sample_size successful_samples additional_latency <<< "$origin_group_state"
  assert_setting "origin group ${ORIGIN_GROUP}" probeRequestType GET "$probe_request"
  assert_setting "origin group ${ORIGIN_GROUP}" probeProtocol Https "$probe_protocol"
  assert_setting "origin group ${ORIGIN_GROUP}" probePath /healthz "$probe_path"
  assert_setting "origin group ${ORIGIN_GROUP}" probeIntervalInSeconds 100 "$probe_interval"
  assert_setting "origin group ${ORIGIN_GROUP}" sampleSize 4 "$sample_size"
  assert_setting "origin group ${ORIGIN_GROUP}" successfulSamplesRequired 3 "$successful_samples"
  assert_setting "origin group ${ORIGIN_GROUP}" additionalLatencyInMilliseconds 50 "$additional_latency"
  origin_group_names="$(azure_tsv afd origin-group list -g "$RG" \
    --profile-name "$PROFILE" --query "join(',', sort([*].name))")"
  assert_setting "profile ${PROFILE}" originGroups "$ORIGIN_GROUP" "$origin_group_names"
  echo "origin group configuration verified"

  # --origin-host-header must be the origin's own TLS name: the edge uses it as
  # SNI to the origin, and Caddy there serves a certificate for that name only.
  log "Origin ${ORIGIN_NAME} -> ${ORIGIN_HOST}:443"
  if az afd origin show -g "$RG" --profile-name "$PROFILE" \
      --origin-group-name "$ORIGIN_GROUP" --origin-name "$ORIGIN_NAME" -o none 2>/dev/null; then
    echo "origin already exists; reconciling TLS and routing settings"
    az afd origin update -g "$RG" --profile-name "$PROFILE" \
      --origin-group-name "$ORIGIN_GROUP" --origin-name "$ORIGIN_NAME" \
      --host-name "$ORIGIN_HOST" --origin-host-header "$ORIGIN_HOST" \
      --https-port 443 --priority 1 --weight 1000 \
      --enforce-certificate-name-check true \
      --enabled-state Enabled -o none
  else
    az afd origin create -g "$RG" --profile-name "$PROFILE" \
      --origin-group-name "$ORIGIN_GROUP" --origin-name "$ORIGIN_NAME" \
      --host-name "$ORIGIN_HOST" --origin-host-header "$ORIGIN_HOST" \
      --https-port 443 --priority 1 --weight 1000 \
      --enforce-certificate-name-check true \
      --enabled-state Enabled -o none
  fi
  origin_state="$(azure_tsv afd origin show -g "$RG" --profile-name "$PROFILE" \
    --origin-group-name "$ORIGIN_GROUP" --origin-name "$ORIGIN_NAME" \
    --query "join('|', [to_string(hostName), to_string(originHostHeader), to_string(httpsPort), to_string(priority), to_string(weight), to_string(enforceCertificateNameCheck), to_string(enabledState)])")"
  IFS='|' read -r origin_host origin_header origin_https_port origin_priority \
    origin_weight origin_cert_check origin_enabled <<< "$origin_state"
  assert_setting "origin ${ORIGIN_NAME}" hostName "$ORIGIN_HOST" "$origin_host"
  assert_setting "origin ${ORIGIN_NAME}" originHostHeader "$ORIGIN_HOST" "$origin_header"
  assert_setting "origin ${ORIGIN_NAME}" httpsPort 443 "$origin_https_port"
  assert_setting "origin ${ORIGIN_NAME}" priority 1 "$origin_priority"
  assert_setting "origin ${ORIGIN_NAME}" weight 1000 "$origin_weight"
  assert_setting "origin ${ORIGIN_NAME}" enforceCertificateNameCheck true "$origin_cert_check"
  assert_setting "origin ${ORIGIN_NAME}" enabledState Enabled "$origin_enabled"
  origin_names="$(azure_tsv afd origin list -g "$RG" --profile-name "$PROFILE" \
    --origin-group-name "$ORIGIN_GROUP" --query "join(',', sort([*].name))")"
  assert_setting "origin group ${ORIGIN_GROUP}" origins "$ORIGIN_NAME" "$origin_names"
  echo "origin configuration verified"

  # Caching is disabled by OMITTING --cache-configuration: a route with no cache
  # configuration does not cache. There is no --enable-caching flag to set false,
  # so the absence below is deliberate — do not "tidy" a cache configuration in.
  log "Route ${ROUTE} (caching disabled, HTTPS only)"
  if az afd route show -g "$RG" --profile-name "$PROFILE" \
      --endpoint-name "$ENDPOINT" --route-name "$ROUTE" -o none 2>/dev/null; then
    echo "route already exists; checking incompatible attachments"
    route_attachments="$(azure_tsv afd route show -g "$RG" \
      --profile-name "$PROFILE" --endpoint-name "$ENDPOINT" --route-name "$ROUTE" \
      --query "join('|', [to_string(cacheConfiguration), to_string(customDomains), to_string(ruleSets), to_string(originPath)])")"
    IFS='|' read -r route_cache route_custom_domains route_rule_sets route_origin_path \
      <<< "$route_attachments"
    assert_nullish "route ${ROUTE}" cacheConfiguration "$route_cache"
    assert_empty_list "route ${ROUTE}" customDomains "$route_custom_domains"
    assert_empty_list "route ${ROUTE}" ruleSets "$route_rule_sets"
    assert_nullish "route ${ROUTE}" originPath "$route_origin_path"

    echo "route attachments are safe; reconciling routing settings"
    az afd route update -g "$RG" --profile-name "$PROFILE" \
      --endpoint-name "$ENDPOINT" --route-name "$ROUTE" \
      --enabled-state Enabled --origin-group "$ORIGIN_GROUP" \
      --supported-protocols Https --patterns-to-match '/*' \
      --forwarding-protocol HttpsOnly --https-redirect Disabled \
      --link-to-default-domain Enabled -o none
  else
    az afd route create -g "$RG" --profile-name "$PROFILE" \
      --endpoint-name "$ENDPOINT" --route-name "$ROUTE" \
      --enabled-state Enabled --origin-group "$ORIGIN_GROUP" \
      --supported-protocols Https --patterns-to-match '/*' \
      --forwarding-protocol HttpsOnly --https-redirect Disabled \
      --link-to-default-domain Enabled -o none
  fi

  # Read the whole effective route back. A cached relay list still passes
  # signature verification, and a rule set can silently add caching or rewrite
  # requests, so absence is as important here as the positive settings.
  route_state="$(azure_tsv afd route show -g "$RG" --profile-name "$PROFILE" \
    --endpoint-name "$ENDPOINT" --route-name "$ROUTE" \
    --query "join('|', [to_string(enabledState), to_string(originGroup.id), join(',', sort(supportedProtocols)), join(',', sort(patternsToMatch)), to_string(forwardingProtocol), to_string(httpsRedirect), to_string(linkToDefaultDomain), to_string(cacheConfiguration), to_string(customDomains), to_string(ruleSets), to_string(originPath)])")"
  IFS='|' read -r route_enabled route_origin_group route_protocols route_patterns \
    route_forwarding route_redirect route_default_domain route_cache \
    route_custom_domains route_rule_sets route_origin_path <<< "$route_state"
  assert_setting "route ${ROUTE}" enabledState Enabled "$route_enabled"
  assert_setting "route ${ROUTE}" originGroup "$ORIGIN_GROUP" "${route_origin_group##*/}"
  assert_setting "route ${ROUTE}" supportedProtocols Https "$route_protocols"
  assert_setting "route ${ROUTE}" patternsToMatch '/*' "$route_patterns"
  assert_setting "route ${ROUTE}" forwardingProtocol HttpsOnly "$route_forwarding"
  assert_setting "route ${ROUTE}" httpsRedirect Disabled "$route_redirect"
  assert_setting "route ${ROUTE}" linkToDefaultDomain Enabled "$route_default_domain"
  assert_nullish "route ${ROUTE}" cacheConfiguration "$route_cache"
  assert_empty_list "route ${ROUTE}" customDomains "$route_custom_domains"
  assert_empty_list "route ${ROUTE}" ruleSets "$route_rule_sets"
  assert_nullish "route ${ROUTE}" originPath "$route_origin_path"
  route_names="$(azure_tsv afd route list -g "$RG" --profile-name "$PROFILE" \
    --endpoint-name "$ENDPOINT" --query "join(',', sort([*].name))")"
  assert_setting "endpoint ${ENDPOINT}" routes "$ROUTE" "$route_names"
  custom_domain_names="$(azure_tsv afd custom-domain list -g "$RG" \
    --profile-name "$PROFILE" --query "join(',', sort([*].name))")"
  assert_setting "profile ${PROFILE}" customDomains '<none>' "${custom_domain_names:-<none>}"
  echo "route configuration verified; caching and custom domains confirmed off"

  log "Provisioned"
  cat <<EOF
Endpoint: https://${hostname}/

Front Door takes a few minutes to propagate. Then run the acceptance check
from an UNPROXIED shell — it refuses to run behind a proxy, because Go tunnels
proxied HTTPS with CONNECT and never takes the no-SNI path:

  go run ./cmd/frontcheck -url https://${hostname}/

Every check must pass before this endpoint is added to DefaultBrokerURLs().
Record the printed certificate details in the pull request that advertises it.
EOF
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
