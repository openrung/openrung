#!/usr/bin/env bash
# Prepare one Foundation relay for an unadvertised, relay-local WSS front.
#
# Commands:
#   migrate RELAY HOST REALITY_PUBLIC_KEY IMAGE
#   sidecar RELAY HOST RELAY_ID FRONT_ID IMAGE
#   origin-tls RELAY HOST ORIGIN_HOST
#   matrix-limits RELAY HOST enable|restore
#   audit RELAY HOST IMAGE
#
# `migrate` is the one-time legacy-to-stable transition.  It preserves the
# currently active VLESS UUID, Reality private key, and short ID directly on
# the host, adds a new persistent Ed25519 identity seed, and transactionally
# starts the pinned image.  No private key or Foundation token is read back.
#
# `sidecar` requires these local mode-0600 files:
#   OPENRUNG_WSS_TICKET_PUBLIC_KEYS_FILE  public verification key ring
#   OPENRUNG_WSS_ORIGIN_TOKENS_FILE       JSON array with 1..2 unique tokens
# They are streamed over SSH stdin and are never accepted in argv or printed.
set -euo pipefail
{ set +x; } 2>/dev/null

COMMAND="${1:-}"
RELAY="${2:-}"
HOST="${3:-}"
SSH_KEY="${OPENRUNG_SSH_KEY:-$HOME/.ssh/id_ed25519_openrung}"
SSH_USER="${OPENRUNG_SSH_USER:-ubuntu}"
KNOWN_HOSTS="${OPENRUNG_KNOWN_HOSTS:-$HOME/.ssh/known_hosts}"
SSH_OPTS=(-i "$SSH_KEY" -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=yes -o "UserKnownHostsFile=${KNOWN_HOSTS}")

die() { printf 'error: %s\n' "$*" >&2; exit 1; }
[[ "$COMMAND" =~ ^(migrate|sidecar|origin-tls|matrix-limits|audit)$ ]] || die "unknown command"
[[ "$RELAY" =~ ^[a-z0-9][a-z0-9-]{0,62}$ ]] || die "relay name is invalid"
[[ "$HOST" =~ ^[A-Za-z0-9][A-Za-z0-9.:-]{0,254}$ ]] || die "host is invalid"
[[ -f "$SSH_KEY" && -f "$KNOWN_HOSTS" ]] || die "SSH key or known_hosts is missing"
ssh-keygen -F "$HOST" -f "$KNOWN_HOSTS" >/dev/null || die "host key is not pinned: ${HOST}"

ssh_run() { ssh "${SSH_OPTS[@]}" "${SSH_USER}@${HOST}" "$@"; }

case "$COMMAND" in
  migrate)
    REALITY_PUBLIC_KEY="${4:-}"
    IMAGE="${5:-}"
    [[ "$REALITY_PUBLIC_KEY" =~ ^[A-Za-z0-9_-]{40,64}$ ]] || die "Reality public key is invalid"
    [[ "$IMAGE" =~ ^[A-Za-z0-9][A-Za-z0-9:/@._-]*$ ]] || die "image is invalid"
    CHECKPOINT="openrung-relay-pre-wss-$(date -u +%Y%m%d%H%M%S)"
    REMOTE_SCRIPT="$(mktemp)"
    trap 'rm -f "$REMOTE_SCRIPT"' EXIT
    umask 077
    cat >"$REMOTE_SCRIPT" <<'REMOTE'
set -euo pipefail
relay="$1" public_key="$2" image="$3" checkpoint="$4"
canonical=/etc/openrung/relay.env
legacy=/etc/openrung/volunteer.env
candidate=openrung-relay
xray_snapshot="/run/openrung-${relay}-xray-config.json"

test "$(sudo docker inspect -f '{{.State.Running}}' "$candidate" 2>/dev/null)" = true
sudo test -f "$legacy"
! sudo test -e "$canonical"
! sudo docker ps -a --format '{{.Names}}' | grep -qx "$checkpoint"
sudo docker exec "$candidate" test -f /tmp/openrung-xray-config.json
sudo sh -c "umask 077; docker exec '$candidate' cat /tmp/openrung-xray-config.json > '$xray_snapshot'"
sudo chown root:root "$xray_snapshot"
sudo chmod 0600 "$xray_snapshot"
trap 'sudo rm -f "$xray_snapshot" "$canonical.tmp"' EXIT

sudo install -d -m 0700 -o root -g root /etc/openrung
sudo python3 - "$legacy" "$canonical.tmp" "$public_key" "$xray_snapshot" <<'PY'
import base64, json, os, re, secrets, sys
legacy, output, public_key, xray_snapshot = sys.argv[1:]
with open(legacy, encoding="utf-8") as handle:
    lines = handle.read().splitlines()
managed = {
    "OPENRUNG_IDENTITY_SEED", "OPENRUNG_CLIENT_ID", "OPENRUNG_REALITY_PRIVATE_KEY",
    "OPENRUNG_REALITY_PUBLIC_KEY", "OPENRUNG_SHORT_ID", "OPENRUNG_CONNECTION_LOG",
    "OPENRUNG_LISTEN_HOST", "OPENRUNG_WSS_FRONTS", "OPENRUNG_XRAY_PATH", "OPENRUNG_CONFIG_OUT",
}
kept = []
seen = set()
for line in lines:
    if not line or line.startswith("#"):
        continue
    if "=" not in line:
        raise SystemExit("legacy env contains an invalid line")
    key, value = line.split("=", 1)
    if not re.fullmatch(r"OPENRUNG_[A-Z0-9_]+", key) or key in seen:
        raise SystemExit("legacy env contains an invalid or duplicate key")
    seen.add(key)
    if key not in managed:
        kept.append((key, value))
required = {"OPENRUNG_FOUNDATION_TOKEN", "OPENRUNG_PUBLIC_HOST", "OPENRUNG_LABEL"}
values = dict(kept)
if not required.issubset(values) or any(not values[key] or any(ch.isspace() for ch in values[key]) for key in required):
    raise SystemExit("legacy env is missing a required value")
with open(xray_snapshot, encoding="utf-8") as handle:
    config = json.load(handle)
inbound = config["inbounds"][0]
client_id = inbound["settings"]["clients"][0]["id"]
reality = inbound["streamSettings"]["realitySettings"]
private_key = reality["privateKey"]
short_id = reality["shortIds"][0]
checks = {
    "OPENRUNG_CLIENT_ID": (client_id, r"[0-9a-fA-F-]{36}"),
    "OPENRUNG_REALITY_PRIVATE_KEY": (private_key, r"[A-Za-z0-9_-]{40,64}"),
    "OPENRUNG_REALITY_PUBLIC_KEY": (public_key, r"[A-Za-z0-9_-]{40,64}"),
    "OPENRUNG_SHORT_ID": (short_id, r"[0-9a-fA-F]{16}"),
}
for key, (value, pattern) in checks.items():
    if not re.fullmatch(pattern, value):
        raise SystemExit(f"generated Xray config contains invalid {key}")
kept.extend([
    ("OPENRUNG_LISTEN_HOST", "0.0.0.0"),
    ("OPENRUNG_CONNECTION_LOG", "false"),
    ("OPENRUNG_CLIENT_ID", client_id),
    ("OPENRUNG_REALITY_PRIVATE_KEY", private_key),
    ("OPENRUNG_REALITY_PUBLIC_KEY", public_key),
    ("OPENRUNG_SHORT_ID", short_id),
    ("OPENRUNG_IDENTITY_SEED", base64.b64encode(secrets.token_bytes(32)).decode("ascii")),
])
fd = os.open(output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, "w", encoding="utf-8") as handle:
    for key, value in kept:
        if "\n" in value or "\r" in value:
            raise SystemExit("env value contains a newline")
        handle.write(f"{key}={value}\n")
PY
sudo chown root:root "$canonical.tmp"
sudo chmod 0600 "$canonical.tmp"
sudo mv "$canonical.tmp" "$canonical"
sudo rm -f "$xray_snapshot"

rollback() {
  sudo docker rm -f "$candidate" >/dev/null 2>&1 || true
  if sudo docker ps -a --format '{{.Names}}' | grep -qx "$checkpoint"; then
    sudo docker rename "$checkpoint" "$candidate" || true
    sudo docker update --restart unless-stopped "$candidate" >/dev/null 2>&1 || true
    sudo docker start "$candidate" >/dev/null 2>&1 || true
  fi
  sudo rm -f "$canonical"
}
trap rollback ERR
sudo docker pull "$image" >/dev/null
sudo docker stop "$candidate" >/dev/null
sudo docker rename "$candidate" "$checkpoint"
sudo docker update --restart no "$checkpoint" >/dev/null
sudo docker run -d --name "$candidate" --restart unless-stopped \
  --network host --cap-drop ALL --cap-add NET_BIND_SERVICE --read-only --tmpfs /tmp \
  --env-file "$canonical" "$image" >/dev/null

for _ in $(seq 1 30); do
  state="$(sudo docker inspect -f '{{.State.Running}} {{.RestartCount}}' "$candidate" 2>/dev/null || true)"
  if [ "$state" = "true 0" ] && sudo docker logs "$candidate" 2>&1 | grep -q 'registered relay'; then
    break
  fi
  sleep 2
done
test "$(sudo docker inspect -f '{{.State.Running}} {{.RestartCount}}' "$candidate")" = "true 0"
line="$(sudo docker logs "$candidate" 2>&1 | grep 'registered relay' | tail -1)"
relay_id="$(printf '%s\n' "$line" | sed -n 's/.* id=\(relay_[0-9a-f]\{32\}\).*/\1/p')"
test -n "$relay_id"
sudo mv "$legacy" "${legacy}.pre-wss-$(date -u +%Y%m%d%H%M%S)"
trap - ERR
printf 'relay=%s relay_id=%s checkpoint=%s\n' "$relay" "$relay_id" "$checkpoint"
REMOTE
    ssh "${SSH_OPTS[@]}" "${SSH_USER}@${HOST}" \
      "sudo bash -s -- '$RELAY' '$REALITY_PUBLIC_KEY' '$IMAGE' '$CHECKPOINT'" <"$REMOTE_SCRIPT"
    ;;

  sidecar)
    RELAY_ID="${4:-}"
    FRONT_ID="${5:-}"
    IMAGE="${6:-}"
    TICKET_FILE="${OPENRUNG_WSS_TICKET_PUBLIC_KEYS_FILE:-}"
    TOKENS_FILE="${OPENRUNG_WSS_ORIGIN_TOKENS_FILE:-}"
    [[ "$RELAY_ID" =~ ^relay_[0-9a-f]{32}$ ]] || die "relay ID is invalid"
    [[ "$FRONT_ID" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$ ]] || die "front ID is invalid"
    [[ "$IMAGE" =~ ^[A-Za-z0-9][A-Za-z0-9:/@._-]*$ ]] || die "image is invalid"
    for file in "$TICKET_FILE" "$TOKENS_FILE"; do
      [[ -f "$file" ]] || die "required sidecar input file is missing"
      mode="$(stat -f '%Lp' "$file" 2>/dev/null || stat -c '%a' "$file")"
      [[ "$mode" == 600 ]] || die "sidecar input files must have mode 0600"
    done
    jq -e 'type == "array" and length >= 1 and length <= 2 and unique == . and all(.[]; type == "string" and length >= 32 and length <= 512 and test("^[^[:space:]]+$"))' "$TOKENS_FILE" >/dev/null \
      || die "origin token ring is invalid"
    grep -Eq '^[A-Za-z0-9+/=]+(,[A-Za-z0-9+/=]+)*$' "$TICKET_FILE" || die "ticket public-key file is invalid"
    TMP_ENV="$(mktemp)"
    trap 'rm -f "$TMP_ENV"' EXIT
    umask 077
    jq -n --arg front "$FRONT_ID" --slurpfile tokens "$TOKENS_FILE" '{($front):$tokens[0]}' >"${TMP_ENV}.tokens"
    {
      printf 'OPENRUNG_WSS_RELAY_ID=%s\n' "$RELAY_ID"
      printf 'OPENRUNG_WSS_TICKET_PUBLIC_KEYS='
      tr -d '\r\n' <"$TICKET_FILE"
      printf '\nOPENRUNG_WSS_FRONT_ORIGIN_TOKENS='
      jq -c . "${TMP_ENV}.tokens"
      printf 'OPENRUNG_WSS_MAX_SESSIONS_PER_SOURCE=512\n'
      printf 'OPENRUNG_WSS_MAX_STREAMS_PER_SOURCE=4096\n'
      printf 'OPENRUNG_WSS_MAX_PENDING_HANDSHAKES=4096\n'
      printf 'OPENRUNG_WSS_GLOBAL_HANDSHAKE_RATE=2000\n'
      printf 'OPENRUNG_WSS_GLOBAL_HANDSHAKE_BURST=10000\n'
    } >"$TMP_ENV"
    chmod 0600 "$TMP_ENV"
    ssh_run "sudo install -d -m 0700 -o root -g root /etc/openrung && sudo install -m 0600 -o root -g root /dev/stdin /etc/openrung/wss.env" <"$TMP_ENV"
    ssh_run "set -e
      sudo grep -q '^OPENRUNG_IDENTITY_SEED=' /etc/openrung/relay.env
      ! sudo grep -q '^OPENRUNG_WSS_FRONTS=' /etc/openrung/relay.env
      test \"\$(sudo docker inspect -f '{{.Config.Image}}' openrung-relay)\" = '$IMAGE'
      if sudo docker ps -a --format '{{.Names}}' | grep -qx openrung-wss-sidecar; then sudo docker rm -f openrung-wss-sidecar >/dev/null; fi
      sudo docker volume create openrung-wss-replay-${RELAY} >/dev/null
      sudo docker run -d --name openrung-wss-sidecar --restart unless-stopped \
        --network host --cap-drop ALL --security-opt no-new-privileges:true --read-only --tmpfs /tmp \
        --mount source=openrung-wss-replay-${RELAY},target=/var/lib/openrung \
        --env-file /etc/openrung/wss.env --entrypoint /usr/local/bin/wss-sidecar '$IMAGE' >/dev/null
      sleep 2
      test \"\$(sudo docker inspect -f '{{.State.Running}} {{.RestartCount}}' openrung-wss-sidecar)\" = 'true 0'
      sudo ss -ltn | grep -q '127.0.0.1:8081'
      ! sudo ss -ltn | grep -Eq '(^|[[:space:]])(0.0.0.0|\*|\[::\]):8081([[:space:]]|$)'"
    printf 'relay=%s relay_id=%s sidecar=ready advertised=false\n' "$RELAY" "$RELAY_ID"
    ;;

  origin-tls)
    ORIGIN_HOST="${4:-}"
    [[ "$ORIGIN_HOST" =~ ^[A-Za-z0-9][A-Za-z0-9.-]{0,252}[A-Za-z0-9]$ ]] || die "origin hostname is invalid"
    CADDYFILE="$(mktemp)"
    trap 'rm -f "$CADDYFILE"' EXIT
    cat >"$CADDYFILE" <<EOF
{
	admin 127.0.0.1:2019
}

https://${ORIGIN_HOST}:8443 {
	@bridge path /api/v1/wss-bridge

	handle @bridge {
		reverse_proxy 127.0.0.1:8081
	}

	handle {
		respond 404
	}
}
EOF
    ssh_run "sudo apt-get update -qq && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq caddy >/dev/null"
    ssh_run "sudo install -m 0644 -o root -g root /dev/stdin /etc/caddy/Caddyfile" <"$CADDYFILE"
    ssh_run "set -e
      sudo caddy validate --config /etc/caddy/Caddyfile >/dev/null
      sudo systemctl enable --now caddy >/dev/null
      sudo systemctl restart caddy
      for _ in \$(seq 1 30); do sudo ss -ltn | grep -q ':8443 ' && exit 0; sleep 2; done
      exit 1"
    printf 'relay=%s origin=%s tls=ready\n' "$RELAY" "$ORIGIN_HOST"
    ;;

  matrix-limits)
    ACTION="${4:-}"
    [[ "$ACTION" == enable || "$ACTION" == restore ]] || die "matrix-limits action must be enable or restore"
    ssh_run "set -e
      env=/etc/openrung/wss.env
      backup=/etc/openrung/wss.env.pre-matrix
      sudo test -f \"\$env\"
      ! sudo grep -q '^OPENRUNG_WSS_FRONTS=' /etc/openrung/relay.env
      if [ '$ACTION' = enable ]; then
        ! sudo test -e \"\$backup\"
        test \"\$(sudo grep -c '^OPENRUNG_WSS_MAX_SESSIONS_PER_SOURCE=512\$' \"\$env\")\" = 1
        ! sudo grep -q '^OPENRUNG_WSS_NO_STREAM_IDLE_TIMEOUT=' \"\$env\"
        sudo install -m 0600 -o root -g root \"\$env\" \"\$backup\"
        sudo sed -i \
          -e 's/^OPENRUNG_WSS_MAX_SESSIONS_PER_SOURCE=512\$/OPENRUNG_WSS_MAX_SESSIONS_PER_SOURCE=2/' \
          -e '\$aOPENRUNG_WSS_NO_STREAM_IDLE_TIMEOUT=3s' \"\$env\"
      else
        sudo test -f \"\$backup\"
        sudo install -m 0600 -o root -g root \"\$backup\" \"\$env\"
        sudo rm -f \"\$backup\"
      fi
      image=\"\$(sudo docker inspect -f '{{.Config.Image}}' openrung-wss-sidecar)\"
      sudo docker rm -f openrung-wss-sidecar >/dev/null
      sudo docker run -d --name openrung-wss-sidecar --restart unless-stopped \
        --network host --cap-drop ALL --security-opt no-new-privileges:true --read-only --tmpfs /tmp \
        --mount source=openrung-wss-replay-${RELAY},target=/var/lib/openrung \
        --env-file \"\$env\" --entrypoint /usr/local/bin/wss-sidecar \"\$image\" >/dev/null
      sleep 2
      test \"\$(sudo docker inspect -f '{{.State.Running}} {{.RestartCount}}' openrung-wss-sidecar)\" = 'true 0'"
    printf 'relay=%s matrix_limits=%s advertised=false\n' "$RELAY" "$ACTION"
    ;;

  audit)
    IMAGE="${4:-}"
    [[ "$IMAGE" =~ ^[A-Za-z0-9][A-Za-z0-9:/@._-]*$ ]] || die "image is invalid"
    ssh_run "set -e
      test \"\$(sudo docker inspect -f '{{.Config.Image}} {{.State.Running}} {{.RestartCount}} {{.HostConfig.NetworkMode}} {{.HostConfig.ReadonlyRootfs}}' openrung-relay)\" = '$IMAGE true 0 host true'
      test \"\$(sudo docker inspect -f '{{.Config.Image}} {{.State.Running}} {{.RestartCount}} {{.HostConfig.NetworkMode}} {{.HostConfig.ReadonlyRootfs}}' openrung-wss-sidecar)\" = '$IMAGE true 0 host true'
      test \"\$(sudo docker inspect -f '{{json .Config.Entrypoint}} {{json .Config.Cmd}} {{json .HostConfig.CapDrop}} {{json .HostConfig.SecurityOpt}}' openrung-wss-sidecar)\" = '[\"/usr/local/bin/wss-sidecar\"] null [\"ALL\"] [\"no-new-privileges:true\"]'
      test \"\$(sudo docker inspect -f '{{range .Mounts}}{{if eq .Destination \"/var/lib/openrung\"}}{{.Type}} {{.Name}} {{.RW}}{{end}}{{end}}' openrung-wss-sidecar)\" = 'volume openrung-wss-replay-${RELAY} true'
      sudo test -f /etc/openrung/relay.env
      sudo test -f /etc/openrung/wss.env
      ! sudo test -e /etc/openrung/wss.env.pre-matrix
      test \"\$(sudo stat -c '%a %U:%G' /etc/openrung/relay.env)\" = '600 root:root'
      test \"\$(sudo stat -c '%a %U:%G' /etc/openrung/wss.env)\" = '600 root:root'
      ! sudo grep -q '^OPENRUNG_WSS_FRONTS=' /etc/openrung/relay.env
      test \"\$(sudo grep -c '^OPENRUNG_WSS_MAX_SESSIONS_PER_SOURCE=512\$' /etc/openrung/wss.env)\" = 1
      ! sudo grep -q '^OPENRUNG_WSS_NO_STREAM_IDLE_TIMEOUT=' /etc/openrung/wss.env
      ! sudo grep -q '^OPENRUNG_WSS_FIXED_TARGET=' /etc/openrung/wss.env
      sudo ss -ltn | grep -q '127.0.0.1:8081'
      ! sudo ss -ltn | grep -Eq '(^|[[:space:]])(0.0.0.0|\*|\[::\]):8081([[:space:]]|\$)'
      sudo ss -ltn | grep -q ':8443 '
      test \"\$(sudo grep -c 'reverse_proxy 127.0.0.1:8081' /etc/caddy/Caddyfile)\" = 1
      ! sudo grep -Eq '^[[:space:]]*log([[:space:]]|\{)' /etc/caddy/Caddyfile
      sudo systemctl is-active --quiet caddy"
    printf 'relay=%s host_audit=ok advertised=false\n' "$RELAY"
    ;;
esac
