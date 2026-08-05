#!/usr/bin/env bash
# Static regression checks for the cloud-init file-permission contract in
# ec2-up.sh. Provisioning the script itself would require a live AWS account,
# so keep these checks narrowly focused on ordering and explicit modes.
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
UMASK_LINE="$(line_of 'umask 077')"
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

echo "PASS: relayhub bootstrap keeps generated secrets private"
