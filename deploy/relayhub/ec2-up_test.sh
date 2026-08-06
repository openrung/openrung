#!/usr/bin/env bash
# Regression checks for the cloud-init secret-handling contract in ec2-up.sh.
# A mocked AWS CLI captures the exact rendered user-data without touching AWS.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="${HERE}/ec2-up.sh"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

line_of() {
  local needle="$1" matches count
  matches="$(grep -nF -- "$needle" "$SCRIPT" || true)"
  count="$(printf '%s\n' "$matches" | sed '/^$/d' | wc -l | tr -d ' ')"
  [ "$count" -eq 1 ] || fail "expected one '${needle}' line, found ${count}"
  printf '%s' "${matches%%:*}"
}

TEMPLATE_LINE="$(line_of "cat > \"\$UD\" <<'TMPL'")"
STRICT_MODE_LINE="$(line_of 'set -euo pipefail')"
TOKEN_REJECT_LINE="$(line_of 'if [ "${OPENRUNG_VOLUNTEER_TOKEN+x}" = x ]; then')"
FIRST_AWS_LINE="$(line_of 'AMI="$(aws ssm get-parameter --region "$REGION" \')"
UMASK_LINE="$(awk -v start="$TEMPLATE_LINE" 'NR > start && $0 == "umask 077" { print NR; exit }' "$SCRIPT")"
[ -n "$UMASK_LINE" ] || fail "cloud-init does not set a restrictive umask"
OPENSSL_LINE="$(line_of 'openssl req -x509 -newkey rsa:2048 -nodes -days 3650')"
CERT_MODE_LINE="$(line_of 'chmod 0644 /etc/openrung/certs/hub.crt')"
ENV_WRITE_LINE="$(line_of 'cat > /etc/openrung/relayhub.env <<ENV')"
ENV_OWNER_LINE="$(line_of 'chown root:root /etc/openrung/relayhub.env')"
ENV_MODE_LINE="$(line_of 'chmod 0600 /etc/openrung/relayhub.env')"
PULL_LINE="$(line_of 'docker pull __IMAGE__')"
UID_LINE="$(line_of 'if ! HUB_UID=$(docker run --rm --entrypoint id __IMAGE__ -u); then')"
ROOT_UID_REJECT_LINE="$(line_of '0) echo "relayhub image must run as a non-root user" >&2; exit 1 ;;')"
KEY_OWNER_LINE="$(line_of 'chown "$HUB_UID" /etc/openrung/certs/hub.key')"

KEY_MODE_MATCHES="$(grep -nF -- 'chmod 0600 /etc/openrung/certs/hub.key' "$SCRIPT" || true)"
KEY_MODE_COUNT="$(printf '%s\n' "$KEY_MODE_MATCHES" | sed '/^$/d' | wc -l | tr -d ' ')"
[ "$KEY_MODE_COUNT" -eq 2 ] || fail "expected key mode to be enforced before and after chown"
KEY_MODE_FIRST="$(printf '%s\n' "$KEY_MODE_MATCHES" | sed -n '1s/:.*//p')"
KEY_MODE_SECOND="$(printf '%s\n' "$KEY_MODE_MATCHES" | sed -n '2s/:.*//p')"

FIRST_EXECUTABLE_AFTER_STRICT="$(awk -v start="$STRICT_MODE_LINE" 'NR > start && $0 !~ /^[[:space:]]*($|#)/ { print NR; exit }' "$SCRIPT")"
[ "$FIRST_EXECUTABLE_AFTER_STRICT" -eq "$TOKEN_REJECT_LINE" ] || fail "a command can run before token rejection"
[ "$TOKEN_REJECT_LINE" -lt "$FIRST_AWS_LINE" ] || fail "token input is not rejected before the first AWS call"
[ "$TEMPLATE_LINE" -lt "$UMASK_LINE" ] || fail "restrictive umask is outside cloud-init"
[ "$UMASK_LINE" -lt "$OPENSSL_LINE" ] || fail "private key is created before restrictive umask"
[ "$OPENSSL_LINE" -lt "$KEY_MODE_FIRST" ] || fail "key mode is not enforced after generation"
[ "$KEY_MODE_FIRST" -lt "$CERT_MODE_LINE" ] || fail "unexpected key/certificate mode sequence"
[ "$UMASK_LINE" -lt "$ENV_WRITE_LINE" ] || fail "environment file is created before restrictive umask"
[ "$ENV_WRITE_LINE" -lt "$ENV_OWNER_LINE" ] || fail "environment owner is set before the file is written"
[ "$ENV_OWNER_LINE" -lt "$ENV_MODE_LINE" ] || fail "environment mode is set before its owner"
[ "$ENV_MODE_LINE" -lt "$PULL_LINE" ] || fail "environment permissions are not finalized before Docker"
[ "$PULL_LINE" -lt "$UID_LINE" ] || fail "image UID is resolved before the image is pulled"
[ "$UID_LINE" -lt "$ROOT_UID_REJECT_LINE" ] || fail "root image UID is not rejected after UID resolution"
[ "$ROOT_UID_REJECT_LINE" -lt "$KEY_OWNER_LINE" ] || fail "key ownership is set before validating the image UID"
[ "$KEY_OWNER_LINE" -lt "$KEY_MODE_SECOND" ] || fail "key mode is not re-enforced after chown"

if grep -Eq 'chmod[[:space:]]+0?644[[:space:]].*hub\.key' "$SCRIPT"; then
  fail "TLS private key must never be made world-readable"
fi

for forbidden in '__TOKEN_ENV__' 'TOKEN_ENV=' 'TOKEN="${OPENRUNG_VOLUNTEER_TOKEN:-}"'; do
  if grep -Fq -- "$forbidden" "$SCRIPT"; then
    fail "relayhub bootstrap still contains obsolete token interpolation: $forbidden"
  fi
done

TEST_TMP="$(mktemp -d)"
trap 'rm -rf "$TEST_TMP"' EXIT
FAKE_BIN="$TEST_TMP/bin"
AWS_LOG="$TEST_TMP/aws.log"
AWS_CAPTURE="$TEST_TMP/user-data"
AWS_ALLOC_COUNTER="$TEST_TMP/allocate-count"
mkdir -p "$FAKE_BIN" "$TEST_TMP/home/.ssh"
: > "$TEST_TMP/home/.ssh/id_ed25519_openrung"

cat > "$FAKE_BIN/ssh-keygen" <<'FAKE_SSH_KEYGEN'
#!/bin/sh
printf '%s\n' "${OPENRUNG_TEST_LOCAL_PUBLIC_KEY:-ssh-ed25519 AAAAopenrungtestkey}"
exit 0
FAKE_SSH_KEYGEN
chmod 0700 "$FAKE_BIN/ssh-keygen"

cat > "$FAKE_BIN/docker" <<'FAKE_DOCKER'
#!/bin/sh
if [ -n "${OPENRUNG_TEST_CHILD_ENV_LOG:-}" ] && \
  { [ "${VOLUNTEER_TOKEN+x}" = x ] || [ "${OPENRUNG_VOLUNTEER_TOKEN+x}" = x ]; }; then
  printf 'docker inherited a volunteer token\n' >> "$OPENRUNG_TEST_CHILD_ENV_LOG"
fi
if [ "${1:-}" = container ] && [ "${2:-}" = inspect ]; then
  [ "${OPENRUNG_TEST_CONTAINER_EXISTS:-false}" = true ]
  exit
fi
echo "unexpected fake docker call: $*" >&2
exit 96
FAKE_DOCKER
chmod 0700 "$FAKE_BIN/docker"

cat > "$FAKE_BIN/mktemp" <<'FAKE_MKTEMP'
#!/bin/sh
if [ -n "${OPENRUNG_TEST_CHILD_ENV_LOG:-}" ] && \
  { [ "${VOLUNTEER_TOKEN+x}" = x ] || [ "${OPENRUNG_VOLUNTEER_TOKEN+x}" = x ]; }; then
  printf 'mktemp inherited a volunteer token\n' >> "$OPENRUNG_TEST_CHILD_ENV_LOG"
fi
exec /usr/bin/mktemp "$@"
FAKE_MKTEMP
chmod 0700 "$FAKE_BIN/mktemp"

cat > "$FAKE_BIN/aws" <<'FAKE_AWS'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$OPENRUNG_TEST_AWS_LOG"
service="${1:-}"
operation="${2:-}"
shift 2 || true

case "$service $operation" in
  'ssm get-parameter')
    printf 'ami-test\n'
    ;;
  'ec2 describe-vpcs')
    printf 'vpc-test\n'
    ;;
  'ec2 describe-subnets')
    printf 'subnet-test\n'
    ;;
  'ec2 describe-security-groups')
    printf 'sg-test\n'
    ;;
  'ec2 describe-key-pairs')
    if [ "${OPENRUNG_TEST_KEY_EXISTS:-true}" != true ]; then
      exit 255
    fi
    case " $* " in
      *' --include-public-key '*)
        printf '%s\n' "${OPENRUNG_TEST_EC2_PUBLIC_KEY:-ssh-ed25519 AAAAopenrungtestkey}"
        ;;
    esac
    ;;
  'ec2 import-key-pair')
    ;;
  'ec2 create-key-pair')
    printf '%s\n' 'fake-private-key-material'
    if [ "${OPENRUNG_TEST_CREATE_KEY_FAIL:-false}" = true ]; then
      exit 95
    fi
    ;;
  'ec2 allocate-address')
    count=0
    if [ -f "$OPENRUNG_TEST_ALLOC_COUNTER" ]; then
      count="$(cat "$OPENRUNG_TEST_ALLOC_COUNTER")"
    fi
    count=$((count + 1))
    printf '%s' "$count" > "$OPENRUNG_TEST_ALLOC_COUNTER"
    case "$count" in
      1) printf 'alloc-1\t203.0.113.10\n' ;;
      2) printf 'alloc-2\t203.0.113.11\n' ;;
      *) exit 91 ;;
    esac
    ;;
  'ec2 run-instances')
    user_data=''
    while [ "$#" -gt 0 ]; do
      if [ "$1" = '--user-data' ]; then
        shift
        user_data="${1:-}"
        break
      fi
      shift
    done
    case "$user_data" in
      file://*) cp "${user_data#file://}" "$OPENRUNG_TEST_AWS_CAPTURE" ;;
      *) exit 92 ;;
    esac
    printf 'i-test\n'
    ;;
  'ec2 wait')
    ;;
  'ec2 describe-instances')
    case "$*" in
      *NetworkInterfaceId*) printf 'eni-test\n' ;;
      *'Primary==`true`'*) printf '10.0.0.10\n' ;;
      *'Primary==`false`'*) printf '10.0.0.11\n' ;;
      *) exit 93 ;;
    esac
    ;;
  'ec2 associate-address')
    ;;
  *)
    printf 'unexpected fake aws call: %s %s %s\n' "$service" "$operation" "$*" >&2
    exit 94
    ;;
esac
FAKE_AWS
chmod 0700 "$FAKE_BIN/aws"

test_token_rejection() {
  local value="$1" output status
  : > "$AWS_LOG"
  set +e
  output="$(env -i \
    PATH="$FAKE_BIN:/usr/bin:/bin:/usr/sbin:/sbin" \
    HOME="$TEST_TMP/home" \
    OPENRUNG_TEST_AWS_LOG="$AWS_LOG" \
    OPENRUNG_TEST_AWS_CAPTURE="$AWS_CAPTURE" \
    OPENRUNG_TEST_ALLOC_COUNTER="$AWS_ALLOC_COUNTER" \
    OPENRUNG_VOLUNTEER_TOKEN="$value" \
    bash "$SCRIPT" rejected-hub 2>&1)"
  status=$?
  set -e

  [ "$status" -eq 2 ] || fail "set token exited $status, want 2"
  [ ! -s "$AWS_LOG" ] || fail "set token reached AWS before rejection"
  case "$output" in
    *user-data*IMDS*post-boot*) ;;
    *) fail "token rejection does not explain persistent user-data and post-boot setup" ;;
  esac
  if [ -n "$value" ] && printf '%s' "$output" | grep -Fq -- "$value"; then
    fail "token rejection echoed the supplied credential"
  fi
}

test_token_rejection 'sentinel-volunteer-token-do-not-print'
test_token_rejection ''

reset_mock_aws() {
  : > "$AWS_LOG"
  rm -f "$AWS_CAPTURE" "$AWS_ALLOC_COUNTER"
}

assert_common_user_data_contract() {
  [ -f "$AWS_CAPTURE" ] || fail "mocked provisioning did not capture rendered user-data"
  if grep -Eq '^OPENRUNG_VOLUNTEER_TOKEN=' "$AWS_CAPTURE"; then
    fail "rendered EC2 user-data contains a volunteer-token assignment"
  fi
  if grep -Eq '__[A-Z0-9_]+__' "$AWS_CAPTURE"; then
    fail "rendered user-data contains an unresolved template placeholder"
  fi
  if grep -Eq '(^|[;[:space:]])set[[:space:]]+(-[^;[:space:]]*x|-o[[:space:]]+xtrace)([;[:space:]]|$)' "$AWS_CAPTURE"; then
    fail "cloud-init enables xtrace and can log metadata credentials"
  fi
}

reset_mock_aws
staged_output="$(env -i \
  PATH="$FAKE_BIN:/usr/bin:/bin:/usr/sbin:/sbin" \
  HOME="$TEST_TMP/home" \
  OPENRUNG_TEST_AWS_LOG="$AWS_LOG" \
  OPENRUNG_TEST_AWS_CAPTURE="$AWS_CAPTURE" \
  OPENRUNG_TEST_ALLOC_COUNTER="$AWS_ALLOC_COUNTER" \
  bash "$SCRIPT" staged-hub 2>&1)"
assert_common_user_data_contract
STAGED_ANON_COUNT="$(grep -Ec '^OPENRUNG_ALLOW_ANONYMOUS_VOLUNTEERS=true$' "$AWS_CAPTURE" || true)"
[ "$STAGED_ANON_COUNT" -eq 0 ] || fail "default staged user-data enables anonymous access"
grep -Fq "cat > /usr/local/sbin/openrung-relayhub-start <<'SCRIPT'" "$AWS_CAPTURE" \
  || fail "staged host does not install the root-only start helper"
grep -Fq 'chmod 0700 /usr/local/sbin/openrung-relayhub-start' "$AWS_CAPTURE" \
  || fail "relayhub start helper is not root-only"
grep -Fq 'chmod 0700 /usr/local/sbin/openrung-relayhub-install-token' "$AWS_CAPTURE" \
  || fail "relayhub token installer is not root-only"
if grep -Fq 'docker rm -f openrung-relayhub' "$AWS_CAPTURE"; then
  fail "start helper can remove a live relayhub before its replacement succeeds"
fi
STAGED_BOOT_ACTION="$(awk '$0 == "chmod 0700 /usr/local/sbin/openrung-relayhub-install-token" { getline; print; exit }' "$AWS_CAPTURE")"
[ "$STAGED_BOOT_ACTION" = 'echo "relayhub staged without authentication; install a token post-boot before starting it"' ] \
  || fail "default staged user-data has an unexpected post-bootstrap action"
case "$staged_output" in
  *'status: STAGED; relayhub is not running'*'StrictHostKeyChecking=yes'*'cloud-init status --wait'*'openrung-relayhub-install-token'*) ;;
  *) fail "staged provisioning output does not describe the authenticated post-boot path" ;;
esac

reset_mock_aws
anonymous_output="$(env -i \
  PATH="$FAKE_BIN:/usr/bin:/bin:/usr/sbin:/sbin" \
  HOME="$TEST_TMP/home" \
  OPENRUNG_TEST_AWS_LOG="$AWS_LOG" \
  OPENRUNG_TEST_AWS_CAPTURE="$AWS_CAPTURE" \
  OPENRUNG_TEST_ALLOC_COUNTER="$AWS_ALLOC_COUNTER" \
  OPENRUNG_ALLOW_ANONYMOUS_VOLUNTEERS=true \
  bash "$SCRIPT" anonymous-hub 2>&1)"
assert_common_user_data_contract
ANON_COUNT="$(grep -Ec '^OPENRUNG_ALLOW_ANONYMOUS_VOLUNTEERS=true$' "$AWS_CAPTURE" || true)"
[ "$ANON_COUNT" -eq 1 ] || fail "explicit anonymous user-data must contain one active opt-in"
ANON_BOOT_ACTION="$(awk '$0 == "chmod 0700 /usr/local/sbin/openrung-relayhub-install-token" { getline; print; exit }' "$AWS_CAPTURE")"
[ "$ANON_BOOT_ACTION" = '/usr/local/sbin/openrung-relayhub-start' ] \
  || fail "explicit anonymous user-data does not have exactly the intended start action"
case "$anonymous_output" in
  *'authentication: OPEN / anonymous (explicit opt-in)'*) ;;
  *) fail "anonymous provisioning output does not disclose open access" ;;
esac

reset_mock_aws
set +e
invalid_output="$(env -i \
  PATH="$FAKE_BIN:/usr/bin:/bin:/usr/sbin:/sbin" \
  HOME="$TEST_TMP/home" \
  OPENRUNG_TEST_AWS_LOG="$AWS_LOG" \
  OPENRUNG_TEST_AWS_CAPTURE="$AWS_CAPTURE" \
  OPENRUNG_TEST_ALLOC_COUNTER="$AWS_ALLOC_COUNTER" \
  OPENRUNG_ALLOW_ANONYMOUS_VOLUNTEERS=1 \
  bash "$SCRIPT" invalid-anonymous-hub 2>&1)"
invalid_status=$?
set -e
[ "$invalid_status" -eq 2 ] || fail "invalid anonymous opt-in exited $invalid_status, want 2"
[ ! -s "$AWS_LOG" ] || fail "invalid anonymous opt-in reached AWS"
case "$invalid_output" in
  *'must be exactly true or false'*) ;;
  *) fail "invalid anonymous opt-in error is not actionable" ;;
esac

reset_mock_aws
set +e
broker_output="$(env -i \
  PATH="$FAKE_BIN:/usr/bin:/bin:/usr/sbin:/sbin" \
  HOME="$TEST_TMP/home" \
  OPENRUNG_TEST_AWS_LOG="$AWS_LOG" \
  OPENRUNG_TEST_AWS_CAPTURE="$AWS_CAPTURE" \
  OPENRUNG_TEST_ALLOC_COUNTER="$AWS_ALLOC_COUNTER" \
  OPENRUNG_BROKER_URL='https://user:sentinel-password@broker.example' \
  bash "$SCRIPT" credential-url-hub 2>&1)"
broker_status=$?
set -e
[ "$broker_status" -eq 2 ] || fail "credential-bearing broker URL exited $broker_status, want 2"
[ ! -s "$AWS_LOG" ] || fail "credential-bearing broker URL reached AWS"
if printf '%s' "$broker_output" | grep -Fq 'sentinel-password'; then
  fail "broker URL rejection echoed embedded credentials"
fi

reset_mock_aws
set +e
mismatch_output="$(env -i \
  PATH="$FAKE_BIN:/usr/bin:/bin:/usr/sbin:/sbin" \
  HOME="$TEST_TMP/home" \
  OPENRUNG_TEST_AWS_LOG="$AWS_LOG" \
  OPENRUNG_TEST_AWS_CAPTURE="$AWS_CAPTURE" \
  OPENRUNG_TEST_ALLOC_COUNTER="$AWS_ALLOC_COUNTER" \
  OPENRUNG_TEST_LOCAL_PUBLIC_KEY='ssh-ed25519 AAAAlocalkey' \
  OPENRUNG_TEST_EC2_PUBLIC_KEY='ssh-ed25519 AAAAremotekey' \
  bash "$SCRIPT" mismatched-key-hub 2>&1)"
mismatch_status=$?
set -e
[ "$mismatch_status" -eq 1 ] || fail "mismatched EC2 key exited $mismatch_status, want 1"
case "$mismatch_output" in
  *'does not match existing EC2 key pair'*'post-boot setup would be unreachable'*) ;;
  *) fail "mismatched EC2 key error is not actionable" ;;
esac
if grep -Eq '^ec2 (allocate-address|run-instances)' "$AWS_LOG"; then
  fail "mismatched EC2 key was detected only after allocating or launching resources"
fi

CREATE_FAIL_HOME="$TEST_TMP/create-fail-home"
mkdir -p "$CREATE_FAIL_HOME"
reset_mock_aws
set +e
create_fail_output="$(env -i \
  PATH="$FAKE_BIN:/usr/bin:/bin:/usr/sbin:/sbin" \
  HOME="$CREATE_FAIL_HOME" \
  OPENRUNG_TEST_AWS_LOG="$AWS_LOG" \
  OPENRUNG_TEST_AWS_CAPTURE="$AWS_CAPTURE" \
  OPENRUNG_TEST_ALLOC_COUNTER="$AWS_ALLOC_COUNTER" \
  OPENRUNG_TEST_KEY_EXISTS=false \
  OPENRUNG_TEST_CREATE_KEY_FAIL=true \
  bash "$SCRIPT" failed-key-create-hub 2>&1)"
create_fail_status=$?
set -e
[ "$create_fail_status" -eq 1 ] || fail "failed key creation exited $create_fail_status, want 1"
[ ! -e "$CREATE_FAIL_HOME/.ssh/id_ed25519_openrung" ] \
  || fail "failed EC2 key creation left a private-key target behind"
if find "$CREATE_FAIL_HOME" -name 'id_ed25519_openrung.tmp.*' -print -quit | grep -q .; then
  fail "failed EC2 key creation left private-key temporary material behind"
fi
case "$create_fail_output" in
  *'no local private key was installed'*) ;;
  *) fail "failed EC2 key creation error is not actionable" ;;
esac

reset_mock_aws
ampersand_output="$(env -i \
  PATH="$FAKE_BIN:/usr/bin:/bin:/usr/sbin:/sbin" \
  HOME="$TEST_TMP/home" \
  OPENRUNG_TEST_AWS_LOG="$AWS_LOG" \
  OPENRUNG_TEST_AWS_CAPTURE="$AWS_CAPTURE" \
  OPENRUNG_TEST_ALLOC_COUNTER="$AWS_ALLOC_COUNTER" \
  OPENRUNG_BROKER_URL='https://broker.example/path&segment' \
  bash "$SCRIPT" ampersand-url-hub 2>&1)"
assert_common_user_data_contract
grep -Fqx 'OPENRUNG_BROKER_URL=https://broker.example/path&segment' "$AWS_CAPTURE" \
  || fail "valid broker URL replacement metacharacters corrupt rendered user-data"
case "$ampersand_output" in
  *'status: STAGED; relayhub is not running'*) ;;
  *) fail "valid broker URL did not complete staged provisioning" ;;
esac

# Execute the rendered stdin installer against an isolated env file. Replace
# only its absolute paths and root check; all parsing and atomic-update logic is
# the same script that cloud-init installs.
INSTALLER_RAW="$TEST_TMP/install-token-raw.sh"
INSTALLER_SCRIPT="$TEST_TMP/install-token.sh"
INSTALL_ENV="$TEST_TMP/relayhub.env"
INSTALL_START="$TEST_TMP/start-relayhub"
INSTALL_START_LOG="$TEST_TMP/start.log"
INSTALL_CHILD_ENV_LOG="$TEST_TMP/child-env.log"
awk '
  $0 == "cat > /usr/local/sbin/openrung-relayhub-install-token <<\047SCRIPT\047" { copying=1; next }
  copying && $0 == "SCRIPT" { exit }
  copying { print }
' "$AWS_CAPTURE" > "$INSTALLER_RAW"
awk -v env_file="$INSTALL_ENV" -v start_helper="$INSTALL_START" '
  $0 == "ENV_FILE=/etc/openrung/relayhub.env" { print "ENV_FILE=" env_file; next }
  $0 == "if [ \"$(id -u)\" -ne 0 ]; then" { print "if false; then"; next }
  $0 == "chown root:root \"$ENV_TMP\"" { print ":"; next }
  $0 == "exec /usr/local/sbin/openrung-relayhub-start" { print "exec " start_helper; next }
  { print }
' "$INSTALLER_RAW" > "$INSTALLER_SCRIPT"
chmod 0700 "$INSTALLER_SCRIPT"
cat > "$INSTALL_START" <<'FAKE_START'
#!/bin/sh
if [ -n "${OPENRUNG_TEST_CHILD_ENV_LOG:-}" ] && \
  { [ "${VOLUNTEER_TOKEN+x}" = x ] || [ "${OPENRUNG_VOLUNTEER_TOKEN+x}" = x ]; }; then
  printf 'start helper inherited a volunteer token\n' >> "$OPENRUNG_TEST_CHILD_ENV_LOG"
fi
printf 'started\n' >> "$OPENRUNG_TEST_START_LOG"
FAKE_START
chmod 0700 "$INSTALL_START"

reset_install_fixture() {
  printf '%s\n' \
    'OPENRUNG_HUB_PUBLIC_HOST=203.0.113.10' \
    'OPENRUNG_ALLOW_ANONYMOUS_VOLUNTEERS=true' > "$INSTALL_ENV"
  : > "$INSTALL_START_LOG"
  : > "$INSTALL_CHILD_ENV_LOG"
}

run_installer() {
  env \
    PATH="$FAKE_BIN:/usr/bin:/bin:/usr/sbin:/sbin" \
    OPENRUNG_TEST_START_LOG="$INSTALL_START_LOG" \
    OPENRUNG_TEST_CHILD_ENV_LOG="$INSTALL_CHILD_ENV_LOG" \
    OPENRUNG_VOLUNTEER_TOKEN=inherited-openrung-sentinel \
    VOLUNTEER_TOKEN=inherited-sentinel \
    "$INSTALLER_SCRIPT"
}

reset_install_fixture
printf 'valid-token_123=\n' | run_installer
grep -Fqx 'OPENRUNG_VOLUNTEER_TOKEN=valid-token_123=' "$INSTALL_ENV" \
  || fail "stdin installer did not write the valid token"
if grep -Fq 'OPENRUNG_ALLOW_ANONYMOUS_VOLUNTEERS=' "$INSTALL_ENV"; then
  fail "stdin installer retained the anonymous opt-in"
fi
[ "$(grep -c '^started$' "$INSTALL_START_LOG")" -eq 1 ] \
  || fail "stdin installer did not perform exactly one first start"
[ ! -s "$INSTALL_CHILD_ENV_LOG" ] \
  || fail "stdin installer exposed the bearer through a child-process environment"

assert_installer_rejects() {
  local label="$1" input_kind="$2" status before
  reset_install_fixture
  before="$(cat "$INSTALL_ENV")"
  set +e
  case "$input_kind" in
    blank) printf '\n' | run_installer >/dev/null 2>&1 ;;
    invalid) printf 'invalid:token\n' | run_installer >/dev/null 2>&1 ;;
    extra_terminated) printf 'valid-token\nextra\n' | run_installer >/dev/null 2>&1 ;;
    extra_unterminated) printf 'valid-token\nextra' | run_installer >/dev/null 2>&1 ;;
    *) fail "unknown installer test input: $input_kind" ;;
  esac
  status=$?
  set -e
  [ "$status" -eq 2 ] || fail "$label installer input exited $status, want 2"
  [ "$(cat "$INSTALL_ENV")" = "$before" ] || fail "$label installer input changed the env file"
  [ ! -s "$INSTALL_START_LOG" ] || fail "$label installer input started relayhub"
  [ ! -s "$INSTALL_CHILD_ENV_LOG" ] || fail "$label installer input leaked through a child environment"
}

assert_installer_rejects "blank" blank
assert_installer_rejects "invalid-character" invalid
assert_installer_rejects "newline-terminated extra-line" extra_terminated
assert_installer_rejects "unterminated extra-line" extra_unterminated

reset_install_fixture
before_existing="$(cat "$INSTALL_ENV")"
set +e
printf 'valid-token\n' | OPENRUNG_TEST_CONTAINER_EXISTS=true run_installer >/dev/null 2>&1
existing_status=$?
set -e
[ "$existing_status" -eq 1 ] || fail "existing-container install exited $existing_status, want 1"
[ "$(cat "$INSTALL_ENV")" = "$before_existing" ] \
  || fail "existing-container refusal changed the env file"
[ ! -s "$INSTALL_START_LOG" ] || fail "existing-container refusal started relayhub"
[ ! -s "$INSTALL_CHILD_ENV_LOG" ] \
  || fail "existing-container refusal leaked through a child environment"

echo "PASS: relayhub bootstrap keeps volunteer bearers out of persistent EC2 user-data"
