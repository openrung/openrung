#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Cap how fast a relay host opens connections to destinations it has not
# contacted recently. Run as root on the relay host:
#
#   egress-limit.sh [RATE|off]
#
# RATE is new destinations per second (default 10, or OPENRUNG_NEW_DEST_RATE);
# the burst allowance is 30 seconds' worth. An address contacted within the
# last 10 minutes is "known" and never limited, so ordinary browsing, which
# keeps returning to the same hosts, stays well inside the budget; what the
# cap bounds is fan-out to many previously unseen addresses. Over-budget
# connection attempts are dropped, so TCP retransmits them once the budget
# refills instead of failing outright. Loopback is exempt.
#
# New destinations in Cloudflare's published address ranges
# (https://www.cloudflare.com/ips/) also draw on a tighter budget of their
# own, 1 per second with a burst of 120, per address family: ordinary traffic
# reaches few new addresses there, so the cap stays out of its way.
#
# Applies the ruleset now, then installs it as /etc/openrung/egress-limit.nft
# with a boot unit that restores it. Re-running replaces the ruleset
# atomically (a failed load keeps the previous one); 'off' removes it.
# The bring-up helpers embed this script in cloud-init user-data, and an
# existing relay can be updated with:
#
#   ssh root@HOST 'sh -s 10' < deploy/relay/egress-limit.sh
set -eu

RATE="${1:-${OPENRUNG_NEW_DEST_RATE:-10}}"
RULES=/etc/openrung/egress-limit.nft
UNIT=/etc/systemd/system/openrung-egress-limit.service

if [ "$RATE" = off ] || [ "$RATE" = none ]; then
  systemctl disable --now openrung-egress-limit.service 2>/dev/null || true
  nft delete table inet openrung_egress 2>/dev/null || true
  rm -f "$RULES" "$UNIT"
  systemctl daemon-reload
  echo "openrung-egress-limit: removed"
  exit 0
fi
case "$RATE" in
  '' | *[!0-9]* | 0*) echo "openrung-egress-limit: rate must be a positive integer or 'off', got '$RATE'" >&2; exit 2 ;;
esac
BURST=$((RATE * 30))

if ! command -v nft >/dev/null 2>&1; then
  # </dev/null: when this script arrives on stdin (sh -s), apt must not
  # consume the rest of it.
  export DEBIAN_FRONTEND=noninteractive
  apt-get -o DPkg::Lock::Timeout=300 update </dev/null
  apt-get -o DPkg::Lock::Timeout=300 install -y nftables </dev/null
fi

mkdir -p /etc/openrung
NEXT="$RULES.new"
trap 'rm -f "$NEXT"' EXIT
# The leading "table" + "delete table" pair makes the load an atomic replace
# whether or not the table already exists. The replace also empties the
# destination history; live connections repopulate it on their next packet.
cat > "$NEXT" <<RULESET
table inet openrung_egress
delete table inet openrung_egress
table inet openrung_egress {
  set known_v4 {
    type ipv4_addr
    size 262144
    flags dynamic, timeout
    timeout 10m
  }
  set known_v6 {
    type ipv6_addr
    size 262144
    flags dynamic, timeout
    timeout 10m
  }
  set cloudflare_v4 {
    type ipv4_addr
    flags interval
    elements = { 173.245.48.0/20, 103.21.244.0/22, 103.22.200.0/22,
                 103.31.4.0/22, 141.101.64.0/18, 108.162.192.0/18,
                 190.93.240.0/20, 188.114.96.0/20, 197.234.240.0/22,
                 198.41.128.0/17, 162.158.0.0/15, 104.16.0.0/13,
                 104.24.0.0/14, 172.64.0.0/13, 131.0.72.0/22 }
  }
  set cloudflare_v6 {
    type ipv6_addr
    flags interval
    elements = { 2400:cb00::/32, 2606:4700::/32, 2803:f800::/32,
                 2405:b500::/32, 2405:8100::/32, 2a06:98c0::/29,
                 2c0f:f248::/32 }
  }
  chain output {
    type filter hook output priority filter; policy accept;
    oif "lo" accept
    # Replies on connections clients opened to the relay carry no new
    # destination; skipping them keeps the set work off the bulk path.
    ct direction reply accept
    meta l4proto != { tcp, udp } accept
    # Every packet the relay sends on an existing connection records its
    # destination as recently contacted, so an address in continuous use never
    # ages out. These packets are never limited; the plain accept covers a
    # full set, where update cannot add.
    ct state != new meta nfproto ipv4 update @known_v4 { ip daddr } accept
    ct state != new meta nfproto ipv6 update @known_v6 { ip6 daddr } accept
    ct state != new accept
    ip daddr @known_v4 update @known_v4 { ip daddr } accept
    ip6 daddr @known_v6 update @known_v6 { ip6 daddr } accept
    ip daddr @cloudflare_v4 limit rate over 1/second burst 120 packets counter drop
    ip6 daddr @cloudflare_v6 limit rate over 1/second burst 120 packets counter drop
    limit rate over ${RATE}/second burst ${BURST} packets counter drop
    meta nfproto ipv4 add @known_v4 { ip daddr }
    meta nfproto ipv6 add @known_v6 { ip6 daddr }
  }
}
RULESET

# Load before installing: nft -f applies the file as one transaction, so a
# failed load leaves the running ruleset and the installed file untouched.
nft -f "$NEXT"
mv "$NEXT" "$RULES"

# The unit only restores the ruleset at boot. Never restart it: restart runs
# ExecStop, which removes the limit before ExecStart reloads it.
# ExecReload is the in-place, atomic path for operators.
cat > "$UNIT" <<UNITFILE
[Unit]
Description=OpenRung relay new-destination rate limit (nftables)
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f $RULES
ExecReload=/usr/sbin/nft -f $RULES
ExecStop=/usr/sbin/nft delete table inet openrung_egress

[Install]
WantedBy=multi-user.target
UNITFILE

systemctl daemon-reload
# A no-op when the unit is already active; on first install it runs
# ExecStart, which reloads the file just loaded.
systemctl enable --now openrung-egress-limit.service
echo "openrung-egress-limit: ${RATE}/s new destinations, burst ${BURST}"
