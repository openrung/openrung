#!/usr/bin/env bash
# Create or audit one dedicated CloudFront WSS distribution for one relay.
#
# The origin token must be supplied in a mode-0600 file through
# OPENRUNG_ORIGIN_TOKEN_FILE.  It is never accepted in argv or printed.  The
# resulting state file contains only the distribution ID/domain and public
# relay/front metadata.
set -euo pipefail
{ set +x; } 2>/dev/null

MODE="${1:-}"
RELAY_NAME="${2:-}"
ORIGIN_HOST="${3:-}"
FRONT_ID="${4:-}"
AWS_PROFILE="${OPENRUNG_AWS_PROFILE:-default}"
AWS_REGION="us-east-1"
ORIGIN_REQUEST_POLICY_ID="${OPENRUNG_WSS_ORIGIN_REQUEST_POLICY_ID:-380513fb-b7e5-4c85-8d9e-ec34cc2992dc}"
CACHE_POLICY_ID="4135ea2d-6df8-44a3-9df3-4b5a84be39ad"
STATE_FILE="${OPENRUNG_WSS_DISTRIBUTION_STATE_FILE:-}"

die() { printf 'error: %s\n' "$*" >&2; exit 1; }
[[ "$MODE" == create || "$MODE" == audit ]] || die "usage: $0 create|audit RELAY_NAME ORIGIN_HOST FRONT_ID"
[[ "$RELAY_NAME" =~ ^[a-z0-9][a-z0-9-]{0,62}$ ]] || die "relay name is invalid"
[[ "$FRONT_ID" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$ ]] || die "front ID is invalid"
[[ "$ORIGIN_HOST" =~ ^[A-Za-z0-9][A-Za-z0-9.-]{0,252}[A-Za-z0-9]$ ]] || die "origin hostname is invalid"
[[ "$ORIGIN_HOST" != *.cloudfront.net ]] || die "origin must be the relay, not another CloudFront distribution"
command -v aws >/dev/null || die "aws CLI is required"
command -v jq >/dev/null || die "jq is required"

TMP_DIR="$(mktemp -d)"
cleanup() { rm -rf "$TMP_DIR"; }
trap cleanup EXIT
umask 077

aws --profile "$AWS_PROFILE" --region "$AWS_REGION" cloudfront list-distributions --output json >"${TMP_DIR}/distributions.json"
EXISTING_ID="$(jq -r --arg host "$ORIGIN_HOST" '[.DistributionList.Items[]? | select(any(.Origins.Items[]?; .DomainName == $host)) | .Id] | if length == 0 then "" elif length == 1 then .[0] else error("multiple distributions use the same relay origin") end' "${TMP_DIR}/distributions.json")"

if [[ "$MODE" == audit ]]; then
  [[ -n "$EXISTING_ID" ]] || die "no distribution points to ${ORIGIN_HOST}"
  DIST_ID="$EXISTING_ID"
else
  TOKEN_FILE="${OPENRUNG_ORIGIN_TOKEN_FILE:-}"
  [[ -n "$TOKEN_FILE" && -f "$TOKEN_FILE" ]] || die "OPENRUNG_ORIGIN_TOKEN_FILE must name a token file"
  TOKEN_MODE="$(stat -f '%Lp' "$TOKEN_FILE" 2>/dev/null || stat -c '%a' "$TOKEN_FILE")"
  [[ "$TOKEN_MODE" == 600 ]] || die "origin token file must have mode 0600"
  TOKEN_LENGTH="$(LC_ALL=C awk 'BEGIN{ok=0} NR==1 && $0 !~ /[[:space:]]/ {n=length($0); if (n >= 32 && n <= 512) {print n; ok=1}} END{if (NR != 1 || !ok) exit 1}' "$TOKEN_FILE")" \
    || die "origin token must be one 32..512 byte non-whitespace line"
  [[ "$TOKEN_LENGTH" -ge 32 ]] || die "origin token is invalid"
  if [[ -n "$EXISTING_ID" ]]; then
    DIST_ID="$EXISTING_ID"
  else
    jq -Rs 'rtrimstr("\n")' "$TOKEN_FILE" >"${TMP_DIR}/token.json"
    jq -n \
      --arg relay "$RELAY_NAME" \
      --arg origin "$ORIGIN_HOST" \
      --arg policy "$ORIGIN_REQUEST_POLICY_ID" \
      --arg cache "$CACHE_POLICY_ID" \
      --slurpfile token "${TMP_DIR}/token.json" '
      {
        CallerReference:("openrung-" + $relay + "-wss-front-v1"),
        Aliases:{Quantity:0},
        DefaultRootObject:"",
        Origins:{Quantity:1,Items:[{
          Id:($relay + "-origin"),DomainName:$origin,OriginPath:"",
          CustomHeaders:{Quantity:1,Items:[{HeaderName:"X-OpenRung-Origin-Token",HeaderValue:$token[0]}]},
          CustomOriginConfig:{HTTPPort:80,HTTPSPort:8443,OriginProtocolPolicy:"https-only",OriginSslProtocols:{Quantity:1,Items:["TLSv1.2"]},OriginReadTimeout:10,OriginKeepaliveTimeout:5,IpAddressType:"ipv4"},
          ConnectionAttempts:1,ConnectionTimeout:5,OriginShield:{Enabled:false},OriginAccessControlId:""
        }]},
        OriginGroups:{Quantity:0},
        DefaultCacheBehavior:{
          TargetOriginId:($relay + "-origin"),
          TrustedSigners:{Enabled:false,Quantity:0},TrustedKeyGroups:{Enabled:false,Quantity:0},
          ViewerProtocolPolicy:"https-only",
          AllowedMethods:{Quantity:2,Items:["HEAD","GET"],CachedMethods:{Quantity:2,Items:["HEAD","GET"]}},
          SmoothStreaming:false,Compress:false,
          LambdaFunctionAssociations:{Quantity:0},FunctionAssociations:{Quantity:0},
          FieldLevelEncryptionId:"",CachePolicyId:$cache,OriginRequestPolicyId:$policy,
          GrpcConfig:{Enabled:false}
        },
        CacheBehaviors:{Quantity:0},CustomErrorResponses:{Quantity:0},
        Comment:("OpenRung per-relay WSS front: " + $relay + " only"),
        Logging:{Enabled:false,IncludeCookies:false,Bucket:"",Prefix:""},
        PriceClass:"PriceClass_All",Enabled:true,
        ViewerCertificate:{CloudFrontDefaultCertificate:true,MinimumProtocolVersion:"TLSv1",CertificateSource:"cloudfront"},
        Restrictions:{GeoRestriction:{RestrictionType:"none",Quantity:0}},
        WebACLId:"",HttpVersion:"http2and3",IsIPV6Enabled:true,
        ContinuousDeploymentPolicyId:"",Staging:false
      }' >"${TMP_DIR}/distribution.json"
    aws --profile "$AWS_PROFILE" --region "$AWS_REGION" cloudfront create-distribution \
      --distribution-config "file://${TMP_DIR}/distribution.json" >"${TMP_DIR}/created.json"
    DIST_ID="$(jq -r '.Distribution.Id' "${TMP_DIR}/created.json")"
  fi
  aws --profile "$AWS_PROFILE" --region "$AWS_REGION" cloudfront wait distribution-deployed --id "$DIST_ID"
fi

aws --profile "$AWS_PROFILE" --region "$AWS_REGION" cloudfront get-distribution --id "$DIST_ID" --output json >"${TMP_DIR}/distribution-state.json"
jq -e --arg origin "$ORIGIN_HOST" --arg policy "$ORIGIN_REQUEST_POLICY_ID" --arg cache "$CACHE_POLICY_ID" '
  .Distribution.Status == "Deployed" and .Distribution.DistributionConfig.Enabled == true and
  .Distribution.DistributionConfig.Origins.Quantity == 1 and
  .Distribution.DistributionConfig.Origins.Items[0].DomainName == $origin and
  .Distribution.DistributionConfig.Origins.Items[0].CustomOriginConfig.HTTPSPort == 8443 and
  .Distribution.DistributionConfig.Origins.Items[0].CustomOriginConfig.OriginProtocolPolicy == "https-only" and
  .Distribution.DistributionConfig.Origins.Items[0].CustomHeaders.Quantity == 1 and
  .Distribution.DistributionConfig.Origins.Items[0].CustomHeaders.Items[0].HeaderName == "X-OpenRung-Origin-Token" and
  (.Distribution.DistributionConfig.Origins.Items[0].CustomHeaders.Items[0].HeaderValue | length) >= 32 and
  (.Distribution.DistributionConfig.Origins.Items[0].CustomHeaders.Items[0].HeaderValue | length) <= 512 and
  .Distribution.DistributionConfig.Origins.Items[0].ConnectionAttempts == 1 and
  .Distribution.DistributionConfig.Origins.Items[0].ConnectionTimeout == 5 and
  .Distribution.DistributionConfig.DefaultCacheBehavior.ViewerProtocolPolicy == "https-only" and
  .Distribution.DistributionConfig.DefaultCacheBehavior.AllowedMethods.Items == ["HEAD","GET"] and
  .Distribution.DistributionConfig.DefaultCacheBehavior.CachePolicyId == $cache and
  .Distribution.DistributionConfig.DefaultCacheBehavior.OriginRequestPolicyId == $policy and
  .Distribution.DistributionConfig.CacheBehaviors.Quantity == 0 and
  .Distribution.DistributionConfig.Logging.Enabled == false
' "${TMP_DIR}/distribution-state.json" >/dev/null || die "distribution failed the structural audit"

jq -n \
  --arg relay "$RELAY_NAME" --arg origin "$ORIGIN_HOST" --arg front "$FRONT_ID" \
  --arg id "$(jq -r '.Distribution.Id' "${TMP_DIR}/distribution-state.json")" \
  --arg domain "$(jq -r '.Distribution.DomainName' "${TMP_DIR}/distribution-state.json")" \
  '{relay:$relay,origin_host:$origin,front_id:$front,distribution_id:$id,distribution_domain:$domain,url:("wss://"+$domain+"/api/v1/wss-bridge")}' \
  >"${TMP_DIR}/public-state.json"
if [[ -n "$STATE_FILE" ]]; then
  install -m 0600 "${TMP_DIR}/public-state.json" "$STATE_FILE"
fi
jq . "${TMP_DIR}/public-state.json"
