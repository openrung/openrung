#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Exercises deploy/broker/Caddyfile's client-IP branches with a real Caddy:
# the file is served unchanged except for its site address, upstream and log
# path, in front of a local echo backend that reports the X-Forwarded-For value
# and any origin-auth secret header it receives.
#
# Needs caddy (or CADDY=/path/to/caddy) and python3. Without caddy the test is
# skipped locally and fails under CI.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CADDY="${CADDY:-$(command -v caddy || true)}"
if [ -z "$CADDY" ]; then
  if [ "${CI:-}" = true ]; then
    echo "caddy is required in CI" >&2
    exit 1
  fi
  echo "SKIP: caddy not found (set CADDY=/path/to/caddy)"
  exit 0
fi

TEST_TMP="$(mktemp -d)"
CADDY_PID=
BACKEND_PID=
cleanup() {
  [ -n "$CADDY_PID" ] && kill "$CADDY_PID" 2>/dev/null
  [ -n "$BACKEND_PID" ] && kill "$BACKEND_PID" 2>/dev/null
  wait 2>/dev/null
  rm -rf "$TEST_TMP"
}
trap cleanup EXIT

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); }
fail() {
  echo "FAIL: $*" >&2
  FAIL=$((FAIL + 1))
}

free_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])'
}

FRONT_PORT="$(free_port)"
BACKEND_PORT="$(free_port)"
CF_SECRET="cf-$(python3 -c 'import secrets; print(secrets.token_hex(24))')"
AZ_SECRET="az-$(python3 -c 'import secrets; print(secrets.token_hex(24))')"
WORKER_SECRET="wk-$(python3 -c 'import secrets; print(secrets.token_hex(24))')"

cat > "${TEST_TMP}/backend.py" <<'EOF'
import http.server, sys
SECRETS = ("X-OpenRung-Origin-Auth", "X-OpenRung-CloudFront-Auth", "X-OpenRung-Azure-Auth")
class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        leaked = ",".join(h for h in SECRETS if self.headers.get(h) is not None) or "-"
        body = ("%s %s" % (self.headers.get("X-Forwarded-For", "-"), leaked)).encode()
        self.send_response(200)
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *args):
        pass
http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
EOF
python3 "${TEST_TMP}/backend.py" "$BACKEND_PORT" &
BACKEND_PID=$!

sed -e "s|^broker-origin.openrung.org {|http://127.0.0.1:${FRONT_PORT} {|" \
  -e "s|127.0.0.1:8080|127.0.0.1:${BACKEND_PORT}|g" \
  -e "s|/var/log/caddy/broker-origin.access.log|${TEST_TMP}/access.log|" \
  "${HERE}/Caddyfile" > "${TEST_TMP}/Caddyfile"
if ! grep -q "127.0.0.1:${FRONT_PORT} {" "${TEST_TMP}/Caddyfile"; then
  echo "could not rewrite the site address; update this test with the Caddyfile" >&2
  exit 1
fi

start_caddy() { # with-secrets|without-secrets
  if [ -n "$CADDY_PID" ]; then
    kill "$CADDY_PID" 2>/dev/null
    wait "$CADDY_PID" 2>/dev/null
  fi
  (
    export OPENRUNG_WORKER_ORIGIN_AUTH="$WORKER_SECRET"
    if [ "$1" = with-secrets ]; then
      export OPENRUNG_CLOUDFRONT_ORIGIN_AUTH="$CF_SECRET"
      export OPENRUNG_AZURE_ORIGIN_AUTH="$AZ_SECRET"
    else
      unset OPENRUNG_CLOUDFRONT_ORIGIN_AUTH OPENRUNG_AZURE_ORIGIN_AUTH
    fi
    export HOME="$TEST_TMP" XDG_DATA_HOME="${TEST_TMP}/data" XDG_CONFIG_HOME="${TEST_TMP}/config"
    exec "$CADDY" run --config "${TEST_TMP}/Caddyfile" --adapter caddyfile
  ) > "${TEST_TMP}/caddy-$1.log" 2>&1 &
  CADDY_PID=$!
  for _ in $(seq 1 50); do
    curl -s -o /dev/null "http://127.0.0.1:${FRONT_PORT}/" && return 0
    sleep 0.1
  done
  echo "caddy did not start; log:" >&2
  cat "${TEST_TMP}/caddy-$1.log" >&2
  exit 1
}

expect() { # name, wanted X-Forwarded-For, curl header args...
  local name="$1" wanted="$2" got xff leaked
  shift 2
  got="$(curl -s "$@" "http://127.0.0.1:${FRONT_PORT}/api/v1/relays")"
  xff="${got% *}"
  leaked="${got##* }"
  if [ "$xff" = "$wanted" ]; then pass; else fail "${name}: X-Forwarded-For=${xff:-<empty>}; want ${wanted}"; fi
  if [ "$leaked" = "-" ]; then pass; else fail "${name}: secret header reached the broker: ${leaked}"; fi
}

PEER=127.0.0.1
WRONG=wrong-wrong-wrong-wrong-wrong-wrong-wrong

start_caddy with-secrets

expect "CloudFront v4 viewer" 203.0.113.7 \
  -H "X-OpenRung-CloudFront-Auth: ${CF_SECRET}" -H "CloudFront-Viewer-Address: 203.0.113.7:51234"
expect "CloudFront v6 viewer" 2001:db8:abcd::42 \
  -H "X-OpenRung-CloudFront-Auth: ${CF_SECRET}" -H "CloudFront-Viewer-Address: 2001:db8:abcd::42:443"
expect "CloudFront ignores supplied X-Forwarded-For" 203.0.113.7 \
  -H "X-OpenRung-CloudFront-Auth: ${CF_SECRET}" -H "X-Forwarded-For: 198.51.100.66" \
  -H "CloudFront-Viewer-Address: 203.0.113.7:51234"
expect "CloudFront wrong secret" "$PEER" \
  -H "X-OpenRung-CloudFront-Auth: ${WRONG}" -H "CloudFront-Viewer-Address: 203.0.113.7:51234"
expect "CloudFront no secret" "$PEER" -H "CloudFront-Viewer-Address: 203.0.113.7:51234"
expect "CloudFront missing viewer" "$PEER" -H "X-OpenRung-CloudFront-Auth: ${CF_SECRET}"
expect "CloudFront malformed viewer" "$PEER" \
  -H "X-OpenRung-CloudFront-Auth: ${CF_SECRET}" -H "CloudFront-Viewer-Address: 203.0.113.7:1, 192.0.2.6:2"
expect "CloudFront viewer without port" "$PEER" \
  -H "X-OpenRung-CloudFront-Auth: ${CF_SECRET}" -H "CloudFront-Viewer-Address: 203.0.113.7"

expect "Azure v4 socket" 203.0.113.9 \
  -H "X-OpenRung-Azure-Auth: ${AZ_SECRET}" -H "X-Azure-SocketIP: 203.0.113.9"
expect "Azure v6 socket" 2001:db8::9 \
  -H "X-OpenRung-Azure-Auth: ${AZ_SECRET}" -H "X-Azure-SocketIP: 2001:db8::9"
expect "Azure prefers the socket over a conflicting client IP" 203.0.113.9 \
  -H "X-OpenRung-Azure-Auth: ${AZ_SECRET}" -H "X-Azure-ClientIP: 198.51.100.66" \
  -H "X-Azure-SocketIP: 203.0.113.9" -H "X-Forwarded-For: 198.51.100.66, 203.0.113.9"
expect "Azure client IP alone is not trusted" "$PEER" \
  -H "X-OpenRung-Azure-Auth: ${AZ_SECRET}" -H "X-Azure-ClientIP: 198.51.100.66"
expect "Azure wrong secret" "$PEER" \
  -H "X-OpenRung-Azure-Auth: ${WRONG}" -H "X-Azure-SocketIP: 203.0.113.9"
expect "Azure health probe without secret" "$PEER" \
  -H "X-FD-HealthProbe: 1" -H "X-Azure-SocketIP: 203.0.113.9"
expect "Azure malformed socket" "$PEER" \
  -H "X-OpenRung-Azure-Auth: ${AZ_SECRET}" -H "X-Azure-SocketIP: 203.0.113.9, 192.0.2.6"
expect "CloudFront secret in the Azure header" "$PEER" \
  -H "X-OpenRung-Azure-Auth: ${CF_SECRET}" -H "X-Azure-SocketIP: 203.0.113.9"
expect "Azure secret in the CloudFront header" "$PEER" \
  -H "X-OpenRung-CloudFront-Auth: ${AZ_SECRET}" -H "CloudFront-Viewer-Address: 203.0.113.7:51234"

# The Worker branch also requires a Cloudflare source address, which a local
# client never has, so a correct Worker secret must not be honoured here.
expect "Worker secret from a non-Cloudflare peer" "$PEER" \
  -H "X-OpenRung-Origin-Auth: ${WORKER_SECRET}" -H "X-Forwarded-For: 198.51.100.66"

start_caddy without-secrets

expect "unset CloudFront secret fails closed" "$PEER" \
  -H "X-OpenRung-CloudFront-Auth: ${CF_SECRET}" -H "CloudFront-Viewer-Address: 203.0.113.7:51234"
expect "unset Azure secret fails closed" "$PEER" \
  -H "X-OpenRung-Azure-Auth: ${AZ_SECRET}" -H "X-Azure-SocketIP: 203.0.113.9"
expect "empty secret header does not match an unset secret" "$PEER" \
  -H "X-OpenRung-Azure-Auth;" -H "X-Azure-SocketIP: 203.0.113.9"

kill "$CADDY_PID" 2>/dev/null
wait "$CADDY_PID" 2>/dev/null
CADDY_PID=
for secret in "$CF_SECRET" "$AZ_SECRET" "$WORKER_SECRET"; do
  if grep -Fq -- "$secret" "${TEST_TMP}/access.log"; then
    fail "an origin-auth secret was written to the access log"
  else
    pass
  fi
done

if [ "$FAIL" -ne 0 ]; then
  echo "${FAIL} assertion(s) failed; ${PASS} passed" >&2
  exit 1
fi
echo "${PASS} assertions passed"
