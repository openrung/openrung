#!/usr/bin/env bash
#
# Provision an OpenRung relay hub on AWS Lightsail (1 GB instance).
#
#   deploy/relayhub/lightsail-up.sh [name]
#
# The relay hub is the public component that terminates reverse tunnels from
# CGNAT volunteer-run relays and forwards client traffic to them. This stands
# one up on a micro_3_0 instance (1 GB RAM / 2 vCPU / 40 GB / 2 TB transfer), gives it a
# static IP, generates a self-signed TLS cert for the control channel (with the
# static IP in the cert SAN), pulls the prebuilt image from GHCR, and opens the
# control port plus the public tunnel port range in the firewall.
#
# Prerequisites: an authenticated `aws` CLI (aws configure) with Lightsail
# permissions, and the GHCR image published and PUBLIC (see deploy/relayhub/README.md).
#
# Overridable via env: OPENRUNG_REGION, OPENRUNG_AZ, OPENRUNG_BUNDLE,
# OPENRUNG_BLUEPRINT, OPENRUNG_IMAGE, OPENRUNG_BROKER_URL, OPENRUNG_HUB_CONTROL_PORT,
# OPENRUNG_HUB_PORT_RANGE, OPENRUNG_VOLUNTEER_TOKEN.
set -euo pipefail

REGION="${OPENRUNG_REGION:-ap-northeast-2}"          # Seoul (South Korea)
AZ="${OPENRUNG_AZ:-${REGION}a}"
BUNDLE="${OPENRUNG_BUNDLE:-micro_3_0}"          # 1GB RAM / 2 vCPU / 40GB / 2TB
BLUEPRINT="${OPENRUNG_BLUEPRINT:-ubuntu_24_04}"
IMAGE="${OPENRUNG_IMAGE:-ghcr.io/openrung/openrung-relayhub:main}"
BROKER_URL="${OPENRUNG_BROKER_URL:-http://54.238.185.205:8080}"
CONTROL_PORT="${OPENRUNG_HUB_CONTROL_PORT:-9443}"
PORT_RANGE="${OPENRUNG_HUB_PORT_RANGE:-20000-20100}"
# HTTP API port for the reachability prober (enables the relay runtime's
# `-mode auto`).
# Served over TLS with the same self-signed control cert. Set to empty to disable.
HTTP_PORT="${OPENRUNG_HUB_HTTP_PORT:-9444}"
TOKEN="${OPENRUNG_VOLUNTEER_TOKEN:-}"
# NOTE: NAT hole-punch reflectors are intentionally NOT enabled here. On Lightsail
# the public IP is 1:1-NAT'd (not on the instance NIC), so the reflector must bind
# a wildcard address but advertise the public IP — a bind/advertise split not yet
# wired for this environment. Punch is also dormant until mobile clients support
# it. Enable later via OPENRUNG_HUB_REFLECTOR_ADDRS once that is in place.

RANGE_START="${PORT_RANGE%%-*}"
RANGE_END="${PORT_RANGE##*-}"

adjectives=(happy grumpy glorious sleepy brave clever gentle jolly mighty nimble plucky quiet rapid shiny snappy spry sturdy sunny swift witty zesty breezy cosmic dapper eager fuzzy golden hardy lucky merry noble proud quirky rustic silly valiant)
nouns=(hippo walrus castle otter falcon badger lantern comet maple harbor meadow beacon pebble willow cactus cobra ferret gecko heron ibex jaguar koala lemur marmot narwhal ocelot panther quokka raven salmon tapir urchin viper wombat yak zebra)

NAME="${1:-hub-${adjectives[RANDOM % ${#adjectives[@]}]}-${nouns[RANDOM % ${#nouns[@]}]}}"
IPNAME="${NAME}-ip"

echo "Provisioning Lightsail relay hub '${NAME}' in ${REGION} (${BUNDLE}, ${BLUEPRINT})"

# Static IP (free while attached; gives the hub a stable public host that the
# self-signed cert and OPENRUNG_HUB_PUBLIC_HOST are pinned to).
aws lightsail allocate-static-ip --static-ip-name "$IPNAME" --region "$REGION" >/dev/null 2>&1 || true
STATIC_IP="$(aws lightsail get-static-ip --static-ip-name "$IPNAME" --region "$REGION" --query 'staticIp.ipAddress' --output text)"

# Optional bearer token (must match the broker). Included in the env file only
# when set so a blank line is harmless. Without a token the hub fails closed, so
# when none is provided we explicitly allow anonymous relay connections (open,
# unauthenticated hub) instead — set OPENRUNG_VOLUNTEER_TOKEN to require auth.
TOKEN_ENV=""
ANON_ENV=""
if [ -n "$TOKEN" ]; then
  TOKEN_ENV="OPENRUNG_VOLUNTEER_TOKEN=${TOKEN}"
else
  ANON_ENV="OPENRUNG_ALLOW_ANONYMOUS_VOLUNTEERS=true"
fi

# HTTP API (reachability prober) env line, only when a port is configured.
HTTP_ENV=""
if [ -n "$HTTP_PORT" ]; then HTTP_ENV="OPENRUNG_HUB_HTTP_ADDR=:${HTTP_PORT}"; fi

# Launch script: install Docker, self-sign a control-channel cert for the static
# IP, pull the public image, and run the hub. The IP is known up front (static
# IP), so it is baked into the cert SAN and OPENRUNG_HUB_PUBLIC_HOST.
read -r -d '' USERDATA <<EOF || true
#!/bin/sh
# NOTE: Lightsail prepends its own #!/bin/sh preamble, so this runs under dash.
# Keep it POSIX — no bashisms (e.g. no 'set -o pipefail').
set -eux
exec > /var/log/openrung-init.log 2>&1
export DEBIAN_FRONTEND=noninteractive
# DPkg::Lock::Timeout waits for cloud-init's own apt activity to release the lock.
apt-get -o DPkg::Lock::Timeout=300 update
apt-get -o DPkg::Lock::Timeout=300 install -y docker.io openssl
systemctl enable --now docker

# Self-signed TLS for the control channel, valid for the static IP. The cert is
# world-readable so the container's non-root user can read it (the box is
# single-purpose). Volunteer-run relays connect with
# OPENRUNG_HUB_INSECURE=true.
mkdir -p /etc/openrung/certs
openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \\
  -keyout /etc/openrung/certs/hub.key \\
  -out /etc/openrung/certs/hub.crt \\
  -subj "/CN=${STATIC_IP}" \\
  -addext "subjectAltName=IP:${STATIC_IP}"
chmod 644 /etc/openrung/certs/hub.crt /etc/openrung/certs/hub.key

cat > /etc/openrung/relayhub.env <<ENVEOF
OPENRUNG_HUB_PUBLIC_HOST=${STATIC_IP}
OPENRUNG_BROKER_URL=${BROKER_URL}
OPENRUNG_HUB_CONTROL_ADDR=:${CONTROL_PORT}
OPENRUNG_HUB_PORT_RANGE=${PORT_RANGE}
OPENRUNG_HUB_TLS_CERT=/etc/openrung/certs/hub.crt
OPENRUNG_HUB_TLS_KEY=/etc/openrung/certs/hub.key
${HTTP_ENV}
${TOKEN_ENV}
${ANON_ENV}
ENVEOF

docker pull ${IMAGE}
docker rm -f openrung-relayhub 2>/dev/null || true
docker run -d --name openrung-relayhub --restart unless-stopped \\
  --network host --cap-drop ALL --read-only --tmpfs /tmp \\
  -v /etc/openrung/certs:/etc/openrung/certs:ro \\
  --env-file /etc/openrung/relayhub.env \\
  ${IMAGE}
EOF

# SSH access: import the standard key pair (id_ed25519_openrung) into this region
# and launch the instance with it, so it is reachable with the fleet's standard
# key. Idempotent. Falls back to the Lightsail default key pair when no local key
# is found (override the key with OPENRUNG_SSH_PUBKEY / OPENRUNG_SSH_KEY_NAME).
SSH_KEY_NAME="${OPENRUNG_SSH_KEY_NAME:-openrung}"
SSH_PUBKEY="${OPENRUNG_SSH_PUBKEY:-}"
if [ -z "$SSH_PUBKEY" ] && [ -f "$HOME/.ssh/id_ed25519_openrung" ]; then
  SSH_PUBKEY="$(ssh-keygen -y -f "$HOME/.ssh/id_ed25519_openrung" 2>/dev/null || true)"
fi
KEYPAIR_ARG=""
if [ -n "$SSH_PUBKEY" ]; then
  aws lightsail import-key-pair --key-pair-name "$SSH_KEY_NAME" --public-key-base64 "$SSH_PUBKEY" --region "$REGION" >/dev/null 2>&1 || true
  KEYPAIR_ARG="--key-pair-name $SSH_KEY_NAME"
fi

aws lightsail create-instances \
  --instance-names "$NAME" \
  $KEYPAIR_ARG \
  --availability-zone "$AZ" \
  --blueprint-id "$BLUEPRINT" \
  --bundle-id "$BUNDLE" \
  --ip-address-type dualstack \
  --user-data "$USERDATA" \
  --region "$REGION" >/dev/null

echo "Waiting for instance to start..."
until [ "$(aws lightsail get-instance --instance-name "$NAME" --region "$REGION" --query 'instance.state.name' --output text 2>/dev/null)" = "running" ]; do
  sleep 5
done

aws lightsail attach-static-ip --static-ip-name "$IPNAME" --instance-name "$NAME" --region "$REGION" >/dev/null

# Open the control port and the public tunnel port range (IPv4 + IPv6). These
# calls are additive and leave the default SSH (22) rule in place.
aws lightsail open-instance-public-ports --instance-name "$NAME" --region "$REGION" \
  --port-info "fromPort=${CONTROL_PORT},toPort=${CONTROL_PORT},protocol=TCP,cidrs=0.0.0.0/0,ipv6Cidrs=::/0" >/dev/null
aws lightsail open-instance-public-ports --instance-name "$NAME" --region "$REGION" \
  --port-info "fromPort=${RANGE_START},toPort=${RANGE_END},protocol=TCP,cidrs=0.0.0.0/0,ipv6Cidrs=::/0" >/dev/null

# HTTP API (reachability prober) — TCP, TLS.
if [ -n "$HTTP_PORT" ]; then
  aws lightsail open-instance-public-ports --instance-name "$NAME" --region "$REGION" \
    --port-info "fromPort=${HTTP_PORT},toPort=${HTTP_PORT},protocol=TCP,cidrs=0.0.0.0/0,ipv6Cidrs=::/0" >/dev/null
fi

echo "Done. Hub '${NAME}' is at ${STATIC_IP} (control ${CONTROL_PORT}, tunnels ${PORT_RANGE})."
if [ -n "$HTTP_PORT" ]; then
  echo "Reachability prober (auto-detect) on https://${STATIC_IP}:${HTTP_PORT} (self-signed)."
fi
echo "It registers tunneled relays with ${BROKER_URL} after boot (~2-3 min)."
echo
echo "Point a CGNAT volunteer-run relay at it with:"
echo "  OPENRUNG_TUNNEL=true OPENRUNG_HUB_ADDR=${STATIC_IP}:${CONTROL_PORT} OPENRUNG_HUB_INSECURE=true"
echo "OPENRUNG_HUB name=${NAME} ip=${STATIC_IP} control_port=${CONTROL_PORT}"
