#!/usr/bin/env bash
#
# Provision an OpenRung broker on AWS Lightsail.
#
#   deploy/broker/lightsail-up.sh [name]
#
# The broker is the control-plane HTTP API: it serves the relay directory to
# clients, accepts volunteer/hub registrations and heartbeats, and ingests client
# telemetry. It carries NO user traffic. This stands one up on a micro_3_0
# instance (1 GB RAM / 2 vCPU), gives it a static IP, pulls the prebuilt image
# from GHCR, runs it with host networking + a persistent telemetry volume, and
# opens the HTTP port.
#
# Front the broker with Cloudflare (TLS + DDoS). The raw origin port stays open
# as the app's direct-IP fallback; the broker only trusts forwarded client-IP
# headers from Cloudflare's ranges, so a direct hit cannot spoof its source. If
# you do not need the direct-IP fallback, restrict the origin port to
# Cloudflare's ranges at the firewall.
#
# Prerequisites: an authenticated `aws` CLI (aws configure) with Lightsail
# permissions, and the GHCR image published and PUBLIC (see deploy/broker/README.md).
#
# Overridable via env: OPENRUNG_REGION, OPENRUNG_AZ, OPENRUNG_BUNDLE,
# OPENRUNG_BLUEPRINT, OPENRUNG_IMAGE, OPENRUNG_BROKER_PORT, OPENRUNG_VOLUNTEER_TOKEN,
# OPENRUNG_DASHBOARD_TOKEN, OPENRUNG_RELAY_STORE, OPENRUNG_RELAY_DATABASE_URL,
# OPENRUNG_TELEMETRY_STORE, OPENRUNG_TELEMETRY_DATABASE_URL, OPENRUNG_GEOIP_ENDPOINT.
#
# This helper deliberately does NOT accept OPENRUNG_FOUNDATION_TOKEN. Lightsail
# retains user-data, and the bootstrap log is readable on the instance, so
# interpolating the privileged bearer here would leave durable copies. Provision
# the broker first, then transfer the token over SSH into the root-owned,
# mode-0600 /etc/openrung/broker.env and recreate the container with --env-file.
#
# OPENRUNG_RELAY_SIGNING_KEY is deliberately NOT overridable here: user-data
# persists world-readable under /var/lib/cloud, so the instance generates a
# fresh seed for itself instead. Clients pin the production public keys —
# replace the generated seed with the production active seed (transfer it into
# /etc/openrung/broker.env and recreate the container with --env-file) before pointing pinned
# clients at this broker.
set -euo pipefail

REGION="${OPENRUNG_REGION:-ap-northeast-1}"     # Tokyo
AZ="${OPENRUNG_AZ:-${REGION}a}"
BUNDLE="${OPENRUNG_BUNDLE:-micro_3_0}"          # 1GB RAM / 2 vCPU / 40GB / 2TB
BLUEPRINT="${OPENRUNG_BLUEPRINT:-ubuntu_24_04}"
IMAGE="${OPENRUNG_IMAGE:-ghcr.io/openrung/openrung-broker:main}"
PORT="${OPENRUNG_BROKER_PORT:-8080}"
TOKEN="${OPENRUNG_VOLUNTEER_TOKEN:-}"
DASHBOARD_TOKEN="${OPENRUNG_DASHBOARD_TOKEN:-}"
RELAY_STORE="${OPENRUNG_RELAY_STORE:-memory}"
RELAY_DATABASE_URL="${OPENRUNG_RELAY_DATABASE_URL:-}"
TELEMETRY_STORE="${OPENRUNG_TELEMETRY_STORE:-}"
TELEMETRY_DATABASE_URL="${OPENRUNG_TELEMETRY_DATABASE_URL:-}"
GEOIP_ENDPOINT="${OPENRUNG_GEOIP_ENDPOINT:-}"

if [ "${OPENRUNG_FOUNDATION_TOKEN+x}" = x ]; then
  echo "error: OPENRUNG_FOUNDATION_TOKEN is not accepted by this helper because Lightsail user-data persists; install it post-boot in /etc/openrung/broker.env" >&2
  exit 2
fi

adjectives=(happy grumpy glorious sleepy brave clever gentle jolly mighty nimble plucky quiet rapid shiny snappy spry sturdy sunny swift witty zesty breezy cosmic dapper eager fuzzy golden hardy lucky merry noble proud quirky rustic silly valiant)
nouns=(hippo walrus castle otter falcon badger lantern comet maple harbor meadow beacon pebble willow cactus cobra ferret gecko heron ibex jaguar koala lemur marmot narwhal ocelot panther quokka raven salmon tapir urchin viper wombat yak zebra)

NAME="${1:-broker-${adjectives[RANDOM % ${#adjectives[@]}]}-${nouns[RANDOM % ${#nouns[@]}]}}"
IPNAME="${NAME}-ip"

echo "Provisioning Lightsail broker '${NAME}' in ${REGION} (${BUNDLE}, ${BLUEPRINT})"

# Static IP (free while attached; gives the broker a stable public host to point
# hubs/volunteers and the Cloudflare origin at).
aws lightsail allocate-static-ip --static-ip-name "$IPNAME" --region "$REGION" >/dev/null 2>&1 || true
STATIC_IP="$(aws lightsail get-static-ip --static-ip-name "$IPNAME" --region "$REGION" --query 'staticIp.ipAddress' --output text)"

# Auth: the broker fails closed without a registration token, so when none is
# provided we explicitly opt into an open, unauthenticated broker — set
# OPENRUNG_VOLUNTEER_TOKEN to require auth (a token supersedes the anonymous flag).
TOKEN_ENV=""
ANON_ENV=""
if [ -n "$TOKEN" ]; then
  TOKEN_ENV="OPENRUNG_VOLUNTEER_TOKEN=${TOKEN}"
else
  ANON_ENV="OPENRUNG_ALLOW_ANONYMOUS_REGISTRATION=true"
fi

# Optional env lines, included only when set so blank lines are harmless.
DASHBOARD_ENV=""; [ -n "$DASHBOARD_TOKEN" ] && DASHBOARD_ENV="OPENRUNG_DASHBOARD_TOKEN=${DASHBOARD_TOKEN}"
STORE_ENV=""; [ -n "$RELAY_STORE" ] && STORE_ENV="OPENRUNG_RELAY_STORE=${RELAY_STORE}"
DB_ENV=""; [ -n "$RELAY_DATABASE_URL" ] && DB_ENV="OPENRUNG_RELAY_DATABASE_URL=${RELAY_DATABASE_URL}"
TELEMETRY_STORE_ENV=""; [ -n "$TELEMETRY_STORE" ] && TELEMETRY_STORE_ENV="OPENRUNG_TELEMETRY_STORE=${TELEMETRY_STORE}"
TELEMETRY_DB_ENV=""; [ -n "$TELEMETRY_DATABASE_URL" ] && TELEMETRY_DB_ENV="OPENRUNG_TELEMETRY_DATABASE_URL=${TELEMETRY_DATABASE_URL}"
GEOIP_ENV=""; [ -n "$GEOIP_ENDPOINT" ] && GEOIP_ENV="OPENRUNG_GEOIP_ENDPOINT=${GEOIP_ENDPOINT}"

# Launch script: install Docker, write the env file, pull the public image, and
# run the broker. Host networking so the broker sees real peer IPs (its
# trusted-proxy client-IP logic needs them); a named volume persists telemetry
# across restarts; the rootfs is otherwise read-only. The image points
# OPENRUNG_TELEMETRY_FILE at the volume, so no telemetry path is set here.
read -r -d '' USERDATA <<EOF || true
#!/bin/sh
# NOTE: Lightsail prepends its own #!/bin/sh preamble, so this runs under dash.
# Keep it POSIX — no bashisms (e.g. no 'set -o pipefail').
set -eux
exec > /var/log/openrung-init.log 2>&1
export DEBIAN_FRONTEND=noninteractive
# DPkg::Lock::Timeout waits for cloud-init's own apt activity to release the lock.
apt-get -o DPkg::Lock::Timeout=300 update
apt-get -o DPkg::Lock::Timeout=300 install -y docker.io
systemctl enable --now docker

mkdir -p /etc/openrung
cat > /etc/openrung/broker.env <<ENVEOF
OPENRUNG_ADDR=:${PORT}
${TOKEN_ENV}
${ANON_ENV}
${DASHBOARD_ENV}
${STORE_ENV}
${DB_ENV}
${TELEMETRY_STORE_ENV}
${TELEMETRY_DB_ENV}
${GEOIP_ENV}
ENVEOF
chmod 600 /etc/openrung/broker.env

# Relay-list signing seed (the broker fails fast without it). Generated ON the
# instance — the \$(...) is escaped so it never expands into user-data, which
# persists on disk under /var/lib/cloud. Production: replace with the pinned
# active seed over an authenticated channel and recreate the container; every
# redeploy must keep using --env-file so the seed is never hand-typed inline.
# set +x for exactly this line: set -eux (above) would otherwise trace the
# EXPANDED command — i.e. the generated seed in cleartext — into the
# world-readable /var/log/openrung-init.log.
set +x
echo "OPENRUNG_RELAY_SIGNING_KEY=\$(openssl rand -base64 32)" >> /etc/openrung/broker.env
set -x

docker pull ${IMAGE}
docker rm -f openrung-broker 2>/dev/null || true
docker run -d --name openrung-broker --restart unless-stopped \\
  --network host --cap-drop ALL --security-opt no-new-privileges --read-only --tmpfs /tmp \\
  -v openrung-broker-state:/var/lib/openrung \\
  --env-file /etc/openrung/broker.env \\
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

# Open the HTTP port (IPv4 + IPv6). Additive — leaves the default SSH (22) rule
# in place. The raw origin stays world-reachable as the direct-IP fallback;
# restrict cidrs to Cloudflare's ranges here if you do not want that.
aws lightsail open-instance-public-ports --instance-name "$NAME" --region "$REGION" \
  --port-info "fromPort=${PORT},toPort=${PORT},protocol=TCP,cidrs=0.0.0.0/0,ipv6Cidrs=::/0" >/dev/null

echo "Done. Broker '${NAME}' is at ${STATIC_IP}:${PORT} (ready ~2-3 min after boot)."
echo "  Health:  curl http://${STATIC_IP}:${PORT}/healthz"
echo "  Relays:  curl http://${STATIC_IP}:${PORT}/api/v1/relays"
if [ -n "$DASHBOARD_TOKEN" ]; then
  echo "  Dashboard: http://${STATIC_IP}:${PORT}/admin/telemetry"
fi
echo
echo "Point hubs and volunteers at it with:"
echo "  OPENRUNG_BROKER_URL=http://${STATIC_IP}:${PORT}"
echo
echo "Front it with Cloudflare for TLS, and set the client apps' HTTPS broker URL"
echo "to the Cloudflare hostname. Telemetry persists in the 'openrung-broker-state'"
echo "docker volume across restarts."
echo
echo "Relay-list signing: the instance generated a FRESH seed in"
echo "/etc/openrung/broker.env. Clients pin the production public keys, so scp the"
echo "production active seed over it (and recreate the container) before pointing"
echo "pinned clients at this broker. Confirm with: curl .../healthz (signing_key_id)."
echo
echo "Foundation registration is disabled until OPENRUNG_FOUNDATION_TOKEN is added"
echo "post-boot to the root-owned /etc/openrung/broker.env, then recreate the container"
echo "with --env-file (docker restart does not reload a changed env file)."
echo "Never pass that token through Lightsail user-data or an inline docker -e argument."
echo "OPENRUNG_BROKER name=${NAME} ip=${STATIC_IP} port=${PORT}"
