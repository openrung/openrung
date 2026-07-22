#!/usr/bin/env bash
# Deploy the aggregate-only CloudFront origin-facing Lightsail firewall sync.
#
# Usage:
#   deploy-wss-origin-firewall-sync.sh check [targets.json]
#   deploy-wss-origin-firewall-sync.sh apply [targets.json]
#
# The target inventory is public configuration and carries only instance names
# and regions: the exact instance ARNs are resolved from the live account at
# apply time so no account or instance identifier is checked in.  IAM grants
# PutInstancePublicPorts only on those resolved ARNs.  GetInstancePortStates
# must remain Resource "*" because Lightsail does not support resource-level
# authorization for that read operation.
set -euo pipefail
{ set +x; } 2>/dev/null

MODE="${1:-check}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TARGETS_FILE="${2:-${HERE}/wss-origin-targets.json}"
AWS_PROFILE="${OPENRUNG_AWS_PROFILE:-default}"
CONTROL_REGION="${OPENRUNG_AWS_CONTROL_REGION:-us-east-1}"
FUNCTION_NAME="${OPENRUNG_WSS_FIREWALL_FUNCTION:-openrung-witty-salmon-cloudfront-origin-sync}"
ROLE_NAME="${OPENRUNG_WSS_FIREWALL_ROLE:-openrung-witty-salmon-cf-origin-sync-role}"
LOCK_TABLE="${OPENRUNG_WSS_FIREWALL_LOCK_TABLE:-openrung-wss-firewall-sync-lock}"
DAILY_RULE="${OPENRUNG_WSS_FIREWALL_DAILY_RULE:-openrung-witty-salmon-cf-origin-sync-daily}"
SNS_TOPIC="arn:aws:sns:us-east-1:806199016981:AmazonIpSpaceChanged"

die() { printf 'error: %s\n' "$*" >&2; exit 1; }
command -v aws >/dev/null || die "aws CLI is required"
command -v jq >/dev/null || die "jq is required"
command -v zip >/dev/null || die "zip is required"
[[ "$MODE" == check || "$MODE" == apply ]] || die "mode must be check or apply"
[[ -f "$TARGETS_FILE" ]] || die "target inventory not found: ${TARGETS_FILE}"

jq -e '
  type == "array" and length > 0 and length <= 100 and
  all(.[];
    (keys | sort) == ["instance","region"] and
    (.instance | test("^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$")) and
    (.region | test("^[a-z]{2}(-gov)?-[a-z]+-[0-9]$"))
  ) and
  ([.[] | (.region + "/" + .instance)] | unique | length) == length
' "$TARGETS_FILE" >/dev/null || die "target inventory is invalid or contains duplicates"

TARGETS_JSON="$(jq -c '[.[] | {region,instance}]' "$TARGETS_FILE")"
TARGET_COUNT="$(jq 'length' "$TARGETS_FILE")"
printf 'validated targets=%s function=%s control_region=%s\n' "$TARGET_COUNT" "$FUNCTION_NAME" "$CONTROL_REGION"
[[ "$MODE" == apply ]] || exit 0

ACCOUNT_ID="$(aws --profile "$AWS_PROFILE" sts get-caller-identity --query Account --output text)"
[[ "$ACCOUNT_ID" =~ ^[0-9]{12}$ ]] || die "AWS account ID is invalid"
ROLE_ARN="arn:aws:iam::${ACCOUNT_ID}:role/${ROLE_NAME}"
TABLE_ARN="arn:aws:dynamodb:${CONTROL_REGION}:${ACCOUNT_ID}:table/${LOCK_TABLE}"
LOG_ARN="arn:aws:logs:${CONTROL_REGION}:${ACCOUNT_ID}:log-group:/aws/lambda/${FUNCTION_NAME}:*"

TMP_DIR="$(mktemp -d)"
cleanup() { rm -rf "$TMP_DIR"; }
trap cleanup EXIT
umask 077

# Resolve each target's instance ARN from the live account so the checked-in
# inventory never contains the account ID or instance identifiers.
printf '[]\n' >"${TMP_DIR}/target-arns.json"
while IFS=$'\t' read -r region instance; do
  INSTANCE_ARN="$(aws --profile "$AWS_PROFILE" --region "$region" lightsail get-instance \
    --instance-name "$instance" --query 'instance.arn' --output text)" \
    || die "could not resolve the instance ARN for ${region}/${instance}"
  [[ "$INSTANCE_ARN" =~ ^arn:aws:lightsail:${region}:${ACCOUNT_ID}:Instance/[A-Za-z0-9-]+$ ]] \
    || die "resolved instance ARN for ${region}/${instance} is invalid"
  jq --arg arn "$INSTANCE_ARN" '. + [$arn]' "${TMP_DIR}/target-arns.json" >"${TMP_DIR}/target-arns.json.tmp"
  mv "${TMP_DIR}/target-arns.json.tmp" "${TMP_DIR}/target-arns.json"
done < <(jq -r '.[] | [.region, .instance] | @tsv' "$TARGETS_FILE")
jq -e --argjson count "$TARGET_COUNT" 'length == $count and (unique | length) == $count' \
  "${TMP_DIR}/target-arns.json" >/dev/null || die "resolved instance ARNs are incomplete or not unique"
printf 'resolved instance arns=%s\n' "$TARGET_COUNT"

jq -n '{Version:"2012-10-17",Statement:[{Effect:"Allow",Principal:{Service:"lambda.amazonaws.com"},Action:"sts:AssumeRole"}]}' >"${TMP_DIR}/trust.json"
jq -n \
  --arg table "$TABLE_ARN" \
  --arg logs "$LOG_ARN" \
  --slurpfile arns "${TMP_DIR}/target-arns.json" '
  {
    Version:"2012-10-17",
    Statement:[
      {Sid:"ReadFirewallState",Effect:"Allow",Action:"lightsail:GetInstancePortStates",Resource:"*"},
      {Sid:"WriteExactRelayFirewalls",Effect:"Allow",Action:"lightsail:PutInstancePublicPorts",Resource:$arns[0]},
      {Sid:"SerializeSync",Effect:"Allow",Action:["dynamodb:PutItem","dynamodb:DeleteItem"],Resource:$table},
      {Sid:"WriteAggregateLogs",Effect:"Allow",Action:["logs:CreateLogStream","logs:PutLogEvents"],Resource:$logs}
    ]
  }' >"${TMP_DIR}/role-policy.json"
jq -n --arg targets "$TARGETS_JSON" --arg table "$LOCK_TABLE" \
  '{Variables:{TARGETS_JSON:$targets,LOCK_TABLE:$table}}' >"${TMP_DIR}/environment.json"

if ! aws --profile "$AWS_PROFILE" --region "$CONTROL_REGION" dynamodb describe-table --table-name "$LOCK_TABLE" >/dev/null 2>&1; then
  aws --profile "$AWS_PROFILE" --region "$CONTROL_REGION" dynamodb create-table \
    --table-name "$LOCK_TABLE" --billing-mode PAY_PER_REQUEST \
    --attribute-definitions AttributeName=LockName,AttributeType=S \
    --key-schema AttributeName=LockName,KeyType=HASH >/dev/null
  aws --profile "$AWS_PROFILE" --region "$CONTROL_REGION" dynamodb wait table-exists --table-name "$LOCK_TABLE"
fi
TTL_STATUS="$(aws --profile "$AWS_PROFILE" --region "$CONTROL_REGION" dynamodb describe-time-to-live \
  --table-name "$LOCK_TABLE" --query TimeToLiveDescription.TimeToLiveStatus --output text)"
if [[ "$TTL_STATUS" == DISABLED ]]; then
  aws --profile "$AWS_PROFILE" --region "$CONTROL_REGION" dynamodb update-time-to-live \
    --table-name "$LOCK_TABLE" --time-to-live-specification Enabled=true,AttributeName=ExpiresAt >/dev/null
elif [[ "$TTL_STATUS" != ENABLED && "$TTL_STATUS" != ENABLING ]]; then
  die "unexpected DynamoDB TTL status: ${TTL_STATUS}"
fi

if ! aws --profile "$AWS_PROFILE" iam get-role --role-name "$ROLE_NAME" >/dev/null 2>&1; then
  aws --profile "$AWS_PROFILE" iam create-role --role-name "$ROLE_NAME" \
    --assume-role-policy-document "file://${TMP_DIR}/trust.json" >/dev/null
fi
aws --profile "$AWS_PROFILE" iam put-role-policy --role-name "$ROLE_NAME" \
  --policy-name openrung-wss-origin-firewall-sync \
  --policy-document "file://${TMP_DIR}/role-policy.json"

(cd "${HERE}/wss_origin_firewall_sync" && zip -q -j "${TMP_DIR}/function.zip" lambda_function.py)
if aws --profile "$AWS_PROFILE" --region "$CONTROL_REGION" lambda get-function --function-name "$FUNCTION_NAME" >/dev/null 2>&1; then
  aws --profile "$AWS_PROFILE" --region "$CONTROL_REGION" lambda update-function-code \
    --function-name "$FUNCTION_NAME" --zip-file "fileb://${TMP_DIR}/function.zip" >/dev/null
  aws --profile "$AWS_PROFILE" --region "$CONTROL_REGION" lambda wait function-updated-v2 --function-name "$FUNCTION_NAME"
  aws --profile "$AWS_PROFILE" --region "$CONTROL_REGION" lambda update-function-configuration \
    --function-name "$FUNCTION_NAME" --runtime python3.13 --handler lambda_function.lambda_handler \
    --timeout 120 --memory-size 128 \
    --environment "file://${TMP_DIR}/environment.json" >/dev/null
else
  aws --profile "$AWS_PROFILE" --region "$CONTROL_REGION" lambda create-function \
    --function-name "$FUNCTION_NAME" --runtime python3.13 --handler lambda_function.lambda_handler \
    --role "$ROLE_ARN" --timeout 120 --memory-size 128 \
    --environment "file://${TMP_DIR}/environment.json" \
    --zip-file "fileb://${TMP_DIR}/function.zip" >/dev/null
fi
aws --profile "$AWS_PROFILE" --region "$CONTROL_REGION" lambda wait function-updated-v2 --function-name "$FUNCTION_NAME"

aws --profile "$AWS_PROFILE" --region "$CONTROL_REGION" logs create-log-group \
  --log-group-name "/aws/lambda/${FUNCTION_NAME}" >/dev/null 2>&1 || true
aws --profile "$AWS_PROFILE" --region "$CONTROL_REGION" logs put-retention-policy \
  --log-group-name "/aws/lambda/${FUNCTION_NAME}" --retention-in-days 14

RULE_ARN="$(aws --profile "$AWS_PROFILE" --region "$CONTROL_REGION" events put-rule \
  --name "$DAILY_RULE" --schedule-expression 'rate(1 day)' --state ENABLED --query RuleArn --output text)"
FUNCTION_ARN="$(aws --profile "$AWS_PROFILE" --region "$CONTROL_REGION" lambda get-function-configuration \
  --function-name "$FUNCTION_NAME" --query FunctionArn --output text)"
aws --profile "$AWS_PROFILE" --region "$CONTROL_REGION" events put-targets \
  --rule "$DAILY_RULE" --targets "Id=firewall-sync,Arn=${FUNCTION_ARN}" >/dev/null
aws --profile "$AWS_PROFILE" --region "$CONTROL_REGION" lambda add-permission \
  --function-name "$FUNCTION_NAME" --statement-id AllowDailyFirewallSync \
  --action lambda:InvokeFunction --principal events.amazonaws.com --source-arn "$RULE_ARN" >/dev/null 2>&1 || true
aws --profile "$AWS_PROFILE" --region "$CONTROL_REGION" lambda add-permission \
  --function-name "$FUNCTION_NAME" --statement-id AllowAWSIPSpaceChanged \
  --action lambda:InvokeFunction --principal sns.amazonaws.com --source-arn "$SNS_TOPIC" >/dev/null 2>&1 || true

for metric in Errors Throttles; do
  aws --profile "$AWS_PROFILE" --region "$CONTROL_REGION" cloudwatch put-metric-alarm \
    --alarm-name "OpenRung-WSS-Origin-Sync-${metric}" \
    --namespace AWS/Lambda --metric-name "$metric" --dimensions "Name=FunctionName,Value=${FUNCTION_NAME}" \
    --statistic Sum --period 300 --evaluation-periods 1 --datapoints-to-alarm 1 \
    --threshold 0 --comparison-operator GreaterThanThreshold --treat-missing-data notBreaching
done

if [[ "${OPENRUNG_SUBSCRIBE_IP_SPACE:-0}" == 1 ]]; then
  aws --profile "$AWS_PROFILE" --region "$CONTROL_REGION" sns subscribe \
    --topic-arn "$SNS_TOPIC" --protocol lambda --notification-endpoint "$FUNCTION_ARN" >/dev/null
fi

SYNCED=0
for attempt in $(seq 1 6); do
  aws --profile "$AWS_PROFILE" --region "$CONTROL_REGION" lambda invoke \
    --function-name "$FUNCTION_NAME" --cli-read-timeout 150 "${TMP_DIR}/invoke.json" >/dev/null
  if jq -e '.ok == true and .target_count == $count' --argjson count "$TARGET_COUNT" "${TMP_DIR}/invoke.json" >/dev/null; then
    SYNCED=1
    break
  fi
  jq -e '.ok == true and .skipped == true' "${TMP_DIR}/invoke.json" >/dev/null \
    || die "post-deployment synchronization failed"
  [[ "$attempt" -lt 6 ]] || break
  sleep 10
done
[[ "$SYNCED" == 1 ]] || die "post-deployment synchronization remained lease-held"
printf 'deployed targets=%s function=%s\n' "$TARGET_COUNT" "$FUNCTION_NAME"
