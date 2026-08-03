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
# This helper is idempotent: re-running it leaves an existing front alone.

set -euo pipefail

RG="${OPENRUNG_AZURE_RG:-openrung-broker-front}"
LOCATION="${OPENRUNG_AZURE_LOCATION:-japaneast}"
PROFILE="${OPENRUNG_AZURE_PROFILE:-openrung-broker-front}"
ENDPOINT="${OPENRUNG_AZURE_ENDPOINT:-openrung-broker}"
ORIGIN_HOST="${OPENRUNG_BROKER_ORIGIN:-broker-origin.openrung.org}"

ORIGIN_GROUP="broker-origin"
ORIGIN_NAME="typhoon-broker"
ROUTE="broker-api"

log() { printf '\n==> %s\n' "$*"; }

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

  log "Endpoint ${ENDPOINT}"
  if az afd endpoint show -g "$RG" --profile-name "$PROFILE" \
      --endpoint-name "$ENDPOINT" -o none 2>/dev/null; then
    echo "endpoint already exists"
  else
    az afd endpoint create -g "$RG" --profile-name "$PROFILE" \
      --endpoint-name "$ENDPOINT" --enabled-state Enabled -o none
  fi

  # Probe /healthz rather than the default "/" so a probe failure means the
  # broker is unhealthy, not merely that the root path has no handler.
  log "Origin group ${ORIGIN_GROUP}"
  if az afd origin-group show -g "$RG" --profile-name "$PROFILE" \
      --origin-group-name "$ORIGIN_GROUP" -o none 2>/dev/null; then
    echo "origin group already exists"
  else
    az afd origin-group create -g "$RG" --profile-name "$PROFILE" \
      --origin-group-name "$ORIGIN_GROUP" \
      --probe-request-type GET --probe-protocol Https --probe-path /healthz \
      --probe-interval-in-seconds 100 \
      --sample-size 4 --successful-samples-required 3 \
      --additional-latency-in-milliseconds 50 -o none
  fi

  # --origin-host-header must be the origin's own TLS name: the edge uses it as
  # SNI to the origin, and Caddy there serves a certificate for that name only.
  log "Origin ${ORIGIN_NAME} -> ${ORIGIN_HOST}:443"
  if az afd origin show -g "$RG" --profile-name "$PROFILE" \
      --origin-group-name "$ORIGIN_GROUP" --origin-name "$ORIGIN_NAME" -o none 2>/dev/null; then
    echo "origin already exists"
  else
    az afd origin create -g "$RG" --profile-name "$PROFILE" \
      --origin-group-name "$ORIGIN_GROUP" --origin-name "$ORIGIN_NAME" \
      --host-name "$ORIGIN_HOST" --origin-host-header "$ORIGIN_HOST" \
      --https-port 443 --priority 1 --weight 1000 \
      --enforce-certificate-name-check true \
      --enabled-state Enabled -o none
  fi

  # Caching is disabled by OMITTING --cache-configuration: a route with no cache
  # configuration does not cache. There is no --enable-caching flag to set false,
  # so the absence below is deliberate — do not "tidy" a cache configuration in.
  log "Route ${ROUTE} (caching disabled, HTTPS only)"
  if az afd route show -g "$RG" --profile-name "$PROFILE" \
      --endpoint-name "$ENDPOINT" --route-name "$ROUTE" -o none 2>/dev/null; then
    echo "route already exists"
  else
    az afd route create -g "$RG" --profile-name "$PROFILE" \
      --endpoint-name "$ENDPOINT" --route-name "$ROUTE" \
      --origin-group "$ORIGIN_GROUP" \
      --supported-protocols Https --patterns-to-match '/*' \
      --forwarding-protocol HttpsOnly --https-redirect Disabled \
      --link-to-default-domain Enabled -o none
  fi

  # Assert it rather than trust it: a cached relay list still passes signature
  # verification, so this is invisible to everything except frontcheck.
  cache="$(az afd route show -g "$RG" --profile-name "$PROFILE" \
    --endpoint-name "$ENDPOINT" --route-name "$ROUTE" \
    --query cacheConfiguration -o tsv 2>/dev/null | tr -d '\r')"
  if [ -n "$cache" ] && [ "$cache" != "None" ]; then
    echo "route ${ROUTE} has caching enabled (${cache}); the relay list must not be cached" >&2
    exit 1
  fi
  echo "caching confirmed off"

  hostname="$(az afd endpoint show -g "$RG" --profile-name "$PROFILE" \
    --endpoint-name "$ENDPOINT" --query hostName -o tsv | tr -d '\r')"

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

main "$@"
