#!/usr/bin/env bash
#
# Provision an OpenRung relay hub on AWS EC2 with TWO public IPs, so the NAT
# hole-punch reflector can classify a peer's NAT (RFC 5780) from two distinct
# vantage points.
#
#   deploy/relayhub/ec2-up.sh [name]
#
# Why EC2 and not Lightsail: Lightsail gives only one public IPv4 per instance,
# and its public IP is 1:1-NAT'd (not on the NIC). EC2 lets us put a secondary
# private IP on the ENI and associate a second Elastic IP, giving two public IPs
# that both map to on-NIC private addresses the reflector can bind. The reflector
# BINDS the private IPs and ADVERTISES the EIPs (see the bind/advertise split in
# internal/relayhub + deploy/relayhub/README.md).
#
# It launches an instance in the default VPC, assigns a secondary private IP,
# allocates + associates two EIPs, self-signs a control-channel TLS cert (SANs =
# both EIPs), and runs the hub with punch enabled. A boot-time systemd unit keeps
# the secondary private IP configured on the interface across reboots.
#
# Prerequisites: authenticated `aws` CLI with EC2 permissions, and the GHCR image
# published + PUBLIC.
#
# Overridable via env: OPENRUNG_REGION, OPENRUNG_EC2_SUBNET, OPENRUNG_EC2_TYPE,
# OPENRUNG_IMAGE, OPENRUNG_BROKER_URL, OPENRUNG_HUB_CONTROL_PORT,
# OPENRUNG_HUB_HTTP_PORT, OPENRUNG_HUB_PORT_RANGE, OPENRUNG_HUB_REFLECTOR_PORT,
# OPENRUNG_KEY_NAME, OPENRUNG_KEY_FILE, OPENRUNG_SG_NAME, OPENRUNG_VOLUNTEER_TOKEN.
set -euo pipefail

REGION="${OPENRUNG_REGION:-ap-northeast-2}"          # Seoul
ITYPE="${OPENRUNG_EC2_TYPE:-t4g.micro}"              # ARM Graviton (cheapest)
IMAGE="${OPENRUNG_IMAGE:-ghcr.io/openrung/openrung-relayhub:main}"
BROKER_URL="${OPENRUNG_BROKER_URL:-http://54.238.185.205:8080}"
CONTROL_PORT="${OPENRUNG_HUB_CONTROL_PORT:-9443}"
HTTP_PORT="${OPENRUNG_HUB_HTTP_PORT:-9444}"
PORT_RANGE="${OPENRUNG_HUB_PORT_RANGE:-20000-20100}"
REFLECTOR_PORT="${OPENRUNG_HUB_REFLECTOR_PORT:-19302}"
KEY_NAME="${OPENRUNG_KEY_NAME:-openrung}"
# Local private key used to SSH into the instance and (below) imported as the EC2
# key pair when it does not exist yet, so hosts share the fleet-standard key.
KEY_FILE="${OPENRUNG_KEY_FILE:-$HOME/.ssh/id_ed25519_openrung}"
SG_NAME="${OPENRUNG_SG_NAME:-openrung-relayhub}"
TOKEN="${OPENRUNG_VOLUNTEER_TOKEN:-}"

RANGE_START="${PORT_RANGE%%-*}"
RANGE_END="${PORT_RANGE##*-}"
NAME="${1:-hub-ec2-$RANDOM}"

# ARM types (t4g/c7g/m7g/...) need the arm64 AMI; everything else amd64.
case "$ITYPE" in
  *g.*|*gd.*) ARCH="arm64" ;;
  *) ARCH="amd64" ;;
esac
AMI="$(aws ssm get-parameter --region "$REGION" \
  --name "/aws/service/canonical/ubuntu/server/24.04/stable/current/${ARCH}/hvm/ebs-gp3/ami-id" \
  --query 'Parameter.Value' --output text)"

VPC="$(aws ec2 describe-vpcs --region "$REGION" --filters Name=isDefault,Values=true --query 'Vpcs[0].VpcId' --output text)"
SUBNET="${OPENRUNG_EC2_SUBNET:-$(aws ec2 describe-subnets --region "$REGION" --filters Name=vpc-id,Values=$VPC --query 'Subnets[0].SubnetId' --output text)}"

echo "Provisioning EC2 relay hub '${NAME}' in ${REGION} (${ITYPE}/${ARCH}, ami ${AMI})"

# --- security group (idempotent) ---
SG_ID="$(aws ec2 describe-security-groups --region "$REGION" \
  --filters "Name=group-name,Values=${SG_NAME}" "Name=vpc-id,Values=${VPC}" \
  --query 'SecurityGroups[0].GroupId' --output text 2>/dev/null || true)"
if [ -z "$SG_ID" ] || [ "$SG_ID" = "None" ]; then
  SG_ID="$(aws ec2 create-security-group --region "$REGION" --group-name "$SG_NAME" \
    --description "OpenRung relay hub" --vpc-id "$VPC" --query GroupId --output text)"
  aws ec2 authorize-security-group-ingress --region "$REGION" --group-id "$SG_ID" --ip-permissions \
    "IpProtocol=tcp,FromPort=22,ToPort=22,IpRanges=[{CidrIp=0.0.0.0/0}]" \
    "IpProtocol=tcp,FromPort=${CONTROL_PORT},ToPort=${CONTROL_PORT},IpRanges=[{CidrIp=0.0.0.0/0}]" \
    "IpProtocol=tcp,FromPort=${HTTP_PORT},ToPort=${HTTP_PORT},IpRanges=[{CidrIp=0.0.0.0/0}]" \
    "IpProtocol=tcp,FromPort=${RANGE_START},ToPort=${RANGE_END},IpRanges=[{CidrIp=0.0.0.0/0}]" \
    "IpProtocol=udp,FromPort=${REFLECTOR_PORT},ToPort=${REFLECTOR_PORT},IpRanges=[{CidrIp=0.0.0.0/0}]" >/dev/null
fi
echo "Security group: ${SG_ID}"

# --- key pair (idempotent) ---
# Prefer importing the fleet-standard public key (derived from $KEY_FILE) so every
# host is reachable with the same key. Only generate a fresh key pair as a last
# resort, and never overwrite an existing local private key.
if ! aws ec2 describe-key-pairs --region "$REGION" --key-names "$KEY_NAME" >/dev/null 2>&1; then
  if [ -f "$KEY_FILE" ] && PUBKEY="$(ssh-keygen -y -f "$KEY_FILE" 2>/dev/null)" && [ -n "$PUBKEY" ]; then
    aws ec2 import-key-pair --region "$REGION" --key-name "$KEY_NAME" \
      --public-key-material "$(printf '%s' "$PUBKEY" | base64)" >/dev/null
    echo "Imported key pair '${KEY_NAME}' from ${KEY_FILE}"
  elif [ -e "$KEY_FILE" ]; then
    echo "ERROR: ${KEY_FILE} exists but is not a usable SSH private key; set OPENRUNG_KEY_FILE" >&2
    exit 1
  else
    mkdir -p "$(dirname "$KEY_FILE")"
    aws ec2 create-key-pair --region "$REGION" --key-name "$KEY_NAME" --query KeyMaterial --output text > "$KEY_FILE"
    chmod 600 "$KEY_FILE"
    echo "Created key pair, private key at ${KEY_FILE}"
  fi
fi

# --- two Elastic IPs ---
read -r ALLOC1 EIP1 < <(aws ec2 allocate-address --region "$REGION" --domain vpc --query '[AllocationId,PublicIp]' --output text)
read -r ALLOC2 EIP2 < <(aws ec2 allocate-address --region "$REGION" --domain vpc --query '[AllocationId,PublicIp]' --output text)
echo "Elastic IPs: ${EIP1} (primary) + ${EIP2} (reflector-2)"

# --- cloud-init: config secondary IP, TLS cert, run hub with punch on ---
UD="$(mktemp)"
trap 'rm -f "$UD"' EXIT
cat > "$UD" <<'TMPL'
#!/bin/bash
set -eux
exec > /var/log/openrung-init.log 2>&1
export DEBIAN_FRONTEND=noninteractive

# Keep every ENI private IP configured on the interface across reboots (AWS does
# not auto-configure secondary private IPs on the OS; the reflector binds them).
cat > /usr/local/bin/openrung-secondary-ips.sh <<'SCRIPT'
#!/bin/bash
set -eu
TOKEN=$(curl -sS -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 600")
imds() { curl -sS -H "X-aws-ec2-metadata-token: $TOKEN" "http://169.254.169.254/latest/meta-data/$1"; }
MAC=$(imds network/interfaces/macs/ | head -1 | tr -d /)
PRIMARY=$(imds local-ipv4)
CIDR=$(imds "network/interfaces/macs/$MAC/subnet-ipv4-cidr-block")
PREFIX=${CIDR##*/}
IFACE=$(ip -o link show | awk -F': ' '/: (en|eth)/{print $2; exit}')
for ip in $(imds "network/interfaces/macs/$MAC/local-ipv4s"); do
  [ "$ip" = "$PRIMARY" ] && continue
  ip addr add "$ip/$PREFIX" dev "$IFACE" 2>/dev/null || true
done
SCRIPT
chmod +x /usr/local/bin/openrung-secondary-ips.sh
cat > /etc/systemd/system/openrung-secondary-ips.service <<'UNIT'
[Unit]
Description=Add EC2 secondary private IPs to the primary interface
After=network-online.target
Wants=network-online.target
Before=docker.service
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/bin/openrung-secondary-ips.sh
[Install]
WantedBy=multi-user.target
UNIT

apt-get -o DPkg::Lock::Timeout=300 update
apt-get -o DPkg::Lock::Timeout=300 install -y docker.io openssl
systemctl daemon-reload
systemctl enable --now openrung-secondary-ips.service
systemctl enable --now docker

TOKEN=$(curl -sS -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 600")
imds() { curl -sS -H "X-aws-ec2-metadata-token: $TOKEN" "http://169.254.169.254/latest/meta-data/$1"; }
MAC=$(imds network/interfaces/macs/ | head -1 | tr -d /)
PRIMARY=$(imds local-ipv4)
SECONDARY=$(imds "network/interfaces/macs/$MAC/local-ipv4s" | grep -v "^$PRIMARY$" | head -1)

mkdir -p /etc/openrung/certs
openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
  -keyout /etc/openrung/certs/hub.key -out /etc/openrung/certs/hub.crt \
  -subj "/CN=__EIP1__" -addext "subjectAltName=IP:__EIP1__,IP:__EIP2__"
chmod 644 /etc/openrung/certs/hub.crt /etc/openrung/certs/hub.key

cat > /etc/openrung/relayhub.env <<ENV
OPENRUNG_HUB_PUBLIC_HOST=__EIP1__
OPENRUNG_BROKER_URL=__BROKER_URL__
OPENRUNG_HUB_CONTROL_ADDR=:__CONTROL_PORT__
OPENRUNG_HUB_PORT_RANGE=__PORT_RANGE__
OPENRUNG_HUB_HTTP_ADDR=:__HTTP_PORT__
OPENRUNG_HUB_TLS_CERT=/etc/openrung/certs/hub.crt
OPENRUNG_HUB_TLS_KEY=/etc/openrung/certs/hub.key
OPENRUNG_HUB_REFLECTOR_ADDRS=$PRIMARY:__REFLECTOR_PORT__,$SECONDARY:__REFLECTOR_PORT__
OPENRUNG_HUB_REFLECTOR_ADVERTISE=__EIP1__:__REFLECTOR_PORT__,__EIP2__:__REFLECTOR_PORT__
__TOKEN_ENV__
__ANON_ENV__
ENV

docker pull __IMAGE__
docker rm -f openrung-relayhub 2>/dev/null || true
docker run -d --name openrung-relayhub --restart unless-stopped \
  --network host --cap-drop ALL --read-only --tmpfs /tmp \
  -v /etc/openrung/certs:/etc/openrung/certs:ro \
  --env-file /etc/openrung/relayhub.env \
  __IMAGE__
TMPL

# Without a token the hub fails closed, so when none is provided we explicitly
# allow anonymous relay connections (open, unauthenticated hub) instead — set
# OPENRUNG_VOLUNTEER_TOKEN to require auth.
TOKEN_ENV=""
ANON_ENV=""
if [ -n "$TOKEN" ]; then
  TOKEN_ENV="OPENRUNG_VOLUNTEER_TOKEN=${TOKEN}"
else
  ANON_ENV="OPENRUNG_ALLOW_ANONYMOUS_VOLUNTEERS=true"
fi
sed -i \
  -e "s#__IMAGE__#${IMAGE}#g" \
  -e "s#__BROKER_URL__#${BROKER_URL}#g" \
  -e "s/__EIP1__/${EIP1}/g" -e "s/__EIP2__/${EIP2}/g" \
  -e "s/__CONTROL_PORT__/${CONTROL_PORT}/g" -e "s/__HTTP_PORT__/${HTTP_PORT}/g" \
  -e "s/__PORT_RANGE__/${PORT_RANGE}/g" -e "s/__REFLECTOR_PORT__/${REFLECTOR_PORT}/g" \
  -e "s#__TOKEN_ENV__#${TOKEN_ENV}#g" \
  -e "s#__ANON_ENV__#${ANON_ENV}#g" \
  "$UD"

# --- launch (primary private IP + one secondary; auto public IP for boot egress) ---
IID="$(aws ec2 run-instances --region "$REGION" \
  --image-id "$AMI" --instance-type "$ITYPE" --key-name "$KEY_NAME" \
  --network-interfaces "DeviceIndex=0,SubnetId=${SUBNET},Groups=${SG_ID},AssociatePublicIpAddress=true,SecondaryPrivateIpAddressCount=1" \
  --user-data "file://${UD}" \
  --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=${NAME}}]" \
  --metadata-options 'HttpTokens=required,HttpEndpoint=enabled' \
  --query 'Instances[0].InstanceId' --output text)"
echo "Instance: ${IID}, waiting for running..."
aws ec2 wait instance-running --instance-ids "$IID" --region "$REGION"

ENI="$(aws ec2 describe-instances --instance-ids "$IID" --region "$REGION" --query 'Reservations[0].Instances[0].NetworkInterfaces[0].NetworkInterfaceId' --output text)"
PRIMARY_IP="$(aws ec2 describe-instances --instance-ids "$IID" --region "$REGION" --query 'Reservations[0].Instances[0].NetworkInterfaces[0].PrivateIpAddresses[?Primary==`true`].PrivateIpAddress | [0]' --output text)"
SECONDARY_IP="$(aws ec2 describe-instances --instance-ids "$IID" --region "$REGION" --query 'Reservations[0].Instances[0].NetworkInterfaces[0].PrivateIpAddresses[?Primary==`false`].PrivateIpAddress | [0]' --output text)"

# Associate EIP1 -> primary, EIP2 -> secondary (order matches the env's
# REFLECTOR_ADDRS[primary,secondary] / REFLECTOR_ADVERTISE[EIP1,EIP2]).
aws ec2 associate-address --region "$REGION" --allocation-id "$ALLOC1" --network-interface-id "$ENI" --private-ip-address "$PRIMARY_IP" >/dev/null
aws ec2 associate-address --region "$REGION" --allocation-id "$ALLOC2" --network-interface-id "$ENI" --private-ip-address "$SECONDARY_IP" >/dev/null

echo "Done. Hub '${NAME}' (${IID}):"
echo "  control  ${EIP1}:${CONTROL_PORT} (TLS, self-signed)"
echo "  http api ${EIP1}:${HTTP_PORT} (reachability prober + punch coordinator)"
echo "  reflector UDP ${EIP1}:${REFLECTOR_PORT} + ${EIP2}:${REFLECTOR_PORT}"
echo "  tunnels  ${EIP1}:${PORT_RANGE}"
echo "It registers tunneled relays with ${BROKER_URL} after boot (~2-3 min)."
echo "Verify: curl -k https://${EIP1}:${HTTP_PORT}/api/v1/punch/config"
echo "OPENRUNG_HUB name=${NAME} eip1=${EIP1} eip2=${EIP2} instance=${IID}"
