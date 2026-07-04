#!/usr/bin/env bash
#
# Provision an OpenRung volunteer relay on AWS Lightsail.
#
#   deploy/volunteer/lightsail-up.sh [name]
#
# If no name is given, a random "adjective-noun" name is generated and used as
# BOTH the Lightsail instance name and the relay label (OPENRUNG_LABEL), so the
# relay shows up in the broker dashboard under the same friendly name as the box.
#
# Overridable via env: OPENRUNG_REGION, OPENRUNG_AZ, OPENRUNG_BUNDLE,
# OPENRUNG_BLUEPRINT, OPENRUNG_IMAGE, OPENRUNG_BROKER_URL.
set -euo pipefail

REGION="${OPENRUNG_REGION:-ap-northeast-1}"
AZ="${OPENRUNG_AZ:-${REGION}a}"
BUNDLE="${OPENRUNG_BUNDLE:-micro_3_0}"          # 1GB RAM / 2 vCPU / 40GB / 2TB
BLUEPRINT="${OPENRUNG_BLUEPRINT:-ubuntu_24_04}"
IMAGE="${OPENRUNG_IMAGE:-ghcr.io/openrung/openrung-volunteer:main}"
BROKER_URL="${OPENRUNG_BROKER_URL:-http://54.238.185.205:8080}"

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
docker rm -f openrung-volunteer 2>/dev/null || true
docker run -d --name openrung-volunteer --restart unless-stopped \\
  --network host --cap-drop ALL --cap-add NET_BIND_SERVICE --read-only --tmpfs /tmp \\
  -e OPENRUNG_BROKER_URL=${BROKER_URL} \\
  -e OPENRUNG_PUBLIC_HOST=${STATIC_IP} \\
  -e OPENRUNG_LISTEN_HOST=0.0.0.0 \\
  -e OPENRUNG_LABEL=${NAME} \\
  ${IMAGE}
EOF

aws lightsail create-instances \
  --instance-names "$NAME" \
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
echo "OPENRUNG_RELAY name=${NAME} ip=${STATIC_IP}"
