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
# Installs /etc/openrung/egress-limit.nft and a boot unit that loads it, then
# applies it now. Re-running replaces the ruleset atomically; 'off' removes it.
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
# The leading "table" + "delete table" pair makes the load an atomic replace
# whether or not the table already exists.
cat > "$RULES" <<RULESET
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
  chain output {
    type filter hook output priority filter; policy accept;
    oif "lo" accept
    ct state != new accept
    meta l4proto != { tcp, udp } accept
    ip daddr @known_v4 update @known_v4 { ip daddr } accept
    ip6 daddr @known_v6 update @known_v6 { ip6 daddr } accept
    limit rate over ${RATE}/second burst ${BURST} packets counter drop
    meta nfproto ipv4 add @known_v4 { ip daddr }
    meta nfproto ipv6 add @known_v6 { ip6 daddr }
  }
}
RULESET

cat > "$UNIT" <<UNITFILE
[Unit]
Description=OpenRung relay new-destination rate limit (nftables)
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f $RULES
ExecStop=/usr/sbin/nft delete table inet openrung_egress

[Install]
WantedBy=multi-user.target
UNITFILE

systemctl daemon-reload
systemctl enable openrung-egress-limit.service
# restart, not start: a re-run must load the rewritten ruleset even when the
# oneshot unit is already active.
systemctl restart openrung-egress-limit.service
echo "openrung-egress-limit: ${RATE}/s new destinations, burst ${BURST}"
