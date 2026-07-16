#!/usr/bin/env bash
#
# Provision an OpenRung volunteer-run relay on AWS Lightsail.
#
#   deploy/relay/lightsail-up.sh [name]
#
# If no name is given, a random "adjective-noun" name is generated and used as
# BOTH the Lightsail instance name and the relay label (OPENRUNG_LABEL), so the
# relay shows up in the broker dashboard under the same friendly name as the box.
#
# Overridable via env: OPENRUNG_REGION, OPENRUNG_AZ, OPENRUNG_BUNDLE,
# OPENRUNG_BLUEPRINT, OPENRUNG_IMAGE, OPENRUNG_BROKER_URL.
#
# This helper deliberately provisions only unauthenticated volunteer-class
# relays. Lightsail retains user-data and the bootstrap log, so registration
# tokens must not be interpolated here. For an authenticated or Foundation relay,
# provision the host first, then install a root-owned mode-0600 env file over SSH
# and recreate the container with --env-file. Foundation relays additionally
# require explicit direct mode and an HTTPS broker URL.
set -euo pipefail

REGION="${OPENRUNG_REGION:-ap-northeast-1}"
AZ="${OPENRUNG_AZ:-${REGION}a}"
BUNDLE="${OPENRUNG_BUNDLE:-micro_3_0}"          # 1GB RAM / 2 vCPU / 40GB / 2TB
BLUEPRINT="${OPENRUNG_BLUEPRINT:-ubuntu_24_04}"
IMAGE="${OPENRUNG_IMAGE:-ghcr.io/openrung/openrung-relay:main}"
BROKER_URL="${OPENRUNG_BROKER_URL:-http://54.238.185.205:8080}"

if [ "${OPENRUNG_VOLUNTEER_TOKEN+x}" = x ] || [ "${OPENRUNG_FOUNDATION_TOKEN+x}" = x ] || [ "${OPENRUNG_NODE_CLASS+x}" = x ]; then
  echo "error: this helper does not accept registration tokens (including the foundation token) or node-class overrides because Lightsail user-data persists; configure them post-boot in a root-owned env file" >&2
  exit 2
fi

adjectives=(happy grumpy glorious sleepy brave clever gentle jolly mighty nimble plucky quiet rapid shiny snappy spry sturdy sunny swift witty zesty breezy cosmic dapper eager fuzzy golden hardy lucky merry noble proud quirky rustic silly valiant)
nouns=(hippo walrus castle otter falcon badger lantern comet maple harbor meadow beacon pebble willow cactus cobra ferret gecko heron ibex jaguar koala lemur marmot narwhal ocelot panther quokka raven salmon tapir urchin viper wombat yak zebra)

NAME="${1:-${adjectives[RANDOM % ${#adjectives[@]}]}-${nouns[RANDOM % ${#nouns[@]}]}}"
IPNAME="${NAME}-ip"

echo "Provisioning Lightsail relay '${NAME}' in ${REGION} (${BUNDLE}, ${BLUEPRINT})"

# Static IP (free while attached; gives the relay a stable public host).
aws lightsail allocate-static-ip --static-ip-name "$IPNAME" --region "$REGION" >/dev/null 2>&1 || true
STATIC_IP="$(aws lightsail get-static-ip --static-ip-name "$IPNAME" --region "$REGION" --query 'staticIp.ipAddress' --output text)"

# Launch script: install Docker, pull the public image, run the relay. The IP is
# known up front (static IP), so OPENRUNG_PUBLIC_HOST and OPENRUNG_LABEL are baked in.
#
# Container hardening — this is the exact posture the production Lightsail and
# Hetzner fleets run (confirmed read-only via `docker inspect` on 2026-07-13:
# ReadonlyRootfs=true, CapDrop=[ALL], CapAdd=[NET_BIND_SERVICE], while serving
# 443). The flags on the `docker run` line:
#   --cap-drop ALL --cap-add NET_BIND_SERVICE
#       Drop every capability, re-add only the one needed to bind the privileged
#       public port (443). The binary carries a cap_net_bind_service file
#       capability (setcap in the Dockerfile); NET_BIND_SERVICE keeps it in the
#       container's bounding set so the non-root `openrung` user can use it.
#   (deliberately NO --security-opt no-new-privileges)
#       no-new-privileges makes the kernel ignore file capabilities on exec,
#       which would break the 443 bind. Do NOT add it here. To harden with
#       no-new-privileges instead, serve a port >= 1024 and drop NET_BIND_SERVICE.
#   --read-only --tmpfs /tmp
#       Read-only rootfs; the only writable path the relay needs is the generated
#       xray config under /tmp. Xray logs to stdout (loglevel warning, no log
#       files), so nothing else is written.
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
docker pull ${IMAGE}
docker rm -f openrung-relay 2>/dev/null || true
docker run -d --name openrung-relay --restart unless-stopped \\
  --network host --cap-drop ALL --cap-add NET_BIND_SERVICE --read-only --tmpfs /tmp \\
  -e OPENRUNG_BROKER_URL=${BROKER_URL} \\
  -e OPENRUNG_PUBLIC_HOST=${STATIC_IP} \\
  -e OPENRUNG_LISTEN_HOST=0.0.0.0 \\
  -e OPENRUNG_LABEL=${NAME} \\
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

# KEYPAIR_ARG deliberately expands unquoted: it is either empty or the two
# words "--key-pair-name <name>".
# shellcheck disable=SC2086
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
aws lightsail open-instance-public-ports --instance-name "$NAME" --region "$REGION" \
  --port-info "fromPort=443,toPort=443,protocol=TCP,cidrs=0.0.0.0/0,ipv6Cidrs=::/0" >/dev/null

echo "Done. '${NAME}' is at ${STATIC_IP}:443 and registers with ${BROKER_URL} after boot (~2-3 min)."
echo "  This helper launches an unauthenticated volunteer-class relay only."
echo "  Install credentials post-boot in a root-owned env file and recreate the container."
echo "OPENRUNG_RELAY name=${NAME} ip=${STATIC_IP}"
