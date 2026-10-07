#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Cap how fast a relay host opens connections to destinations it has not
# contacted recently. Ships in the relay image as /usr/local/bin/egress-limit:
#
#   egress-limit apply [RATE]   load the ruleset and save it for boot
#   egress-limit off            remove the ruleset and the saved copy
#   egress-limit render [RATE]  print the ruleset
#   egress-limit unit           print the host boot unit
#
# RATE is new destinations per second (default 10, or OPENRUNG_NEW_DEST_RATE);
# the burst allowance is 30 seconds' worth. An address contacted within the
# last 10 minutes is "known" and never limited, so ordinary browsing, which
# keeps returning to the same hosts, stays well inside the budget; what the
# cap bounds is fan-out to many previously unseen addresses. Over-budget
# connection attempts are dropped, so TCP retransmits them once the budget
# refills instead of failing outright. Loopback is exempt.
#
# New destinations in Cloudflare's address ranges (those published at
# https://www.cloudflare.com/ips/, plus 104.28.0.0/14, which completes
# Cloudflare's 104.16.0.0/12 block) also draw on a tighter budget of their
# own, 1 per second with a burst of 120, per address family: ordinary traffic
# reaches few new addresses there, so the cap stays out of its way.
#
# apply and off change the host's firewall, so they run as a one-shot
# container that alone holds NET_ADMIN, on the host network with the host's
# /etc/openrung mounted; the long-running relay container keeps no
# capabilities. The host keeps only the boot unit printed by `unit`, which
# reloads the saved ruleset with the host's own nft. deploy/relay/README.md
# shows the invocation; foundation-up.sh and the bring-up helpers run it.
set -eu

RULES=/etc/openrung/egress-limit.nft

usage() {
  echo "usage: egress-limit apply [RATE] | off | render [RATE] | unit" >&2
  exit 2
}

valid_rate() { # rate
  case "$1" in
    '' | *[!0-9]* | 0*)
      echo "egress-limit: rate must be a positive integer or 'off', got '$1'" >&2
      exit 2
      ;;
  esac
}

render() { # rate
  burst=$(($1 * 30))
  # The leading "table" + "delete table" pair makes the load an atomic replace
  # whether or not the table already exists. The replace also empties the
  # destination history; live connections repopulate it on their next packet.
  cat <<RULESET
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
                 104.24.0.0/14, 104.28.0.0/14, 172.64.0.0/13,
                 131.0.72.0/22 }
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
    limit rate over $1/second burst $burst packets counter drop
    meta nfproto ipv4 add @known_v4 { ip daddr }
    meta nfproto ipv6 add @known_v6 { ip6 daddr }
  }
}
RULESET
}

# The boot unit only restores the saved ruleset. Never restart it: restart runs
# ExecStop, which removes the limit before ExecStart reloads it. ExecReload is
# the in-place, atomic path for operators.
unit() {
  cat <<UNIT
[Unit]
Description=OpenRung relay new-destination rate limit (nftables)
After=network-online.target
Wants=network-online.target
ConditionPathExists=$RULES

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f $RULES
ExecReload=/usr/sbin/nft -f $RULES
ExecStop=/usr/sbin/nft delete table inet openrung_egress

[Install]
WantedBy=multi-user.target
UNIT
}

remove() {
  nft delete table inet openrung_egress 2>/dev/null || true
  rm -f "$RULES"
  echo "egress-limit: removed"
}

command="${1:-}"
[ "$#" -gt 0 ] && shift
case "$command" in
  apply)
    rate="${1:-${OPENRUNG_NEW_DEST_RATE:-10}}"
    if [ "$rate" = off ]; then
      remove
      exit 0
    fi
    valid_rate "$rate"
    mkdir -p "${RULES%/*}"
    next="$RULES.new"
    trap 'rm -f "$next"' EXIT
    render "$rate" > "$next"
    # Load before saving: nft -f applies the file as one transaction, so a
    # failed load leaves the running ruleset and the saved copy untouched.
    nft -f "$next"
    mv "$next" "$RULES"
    echo "egress-limit: ${rate}/s new destinations, burst $((rate * 30))"
    ;;
  off)
    remove
    ;;
  render)
    rate="${1:-${OPENRUNG_NEW_DEST_RATE:-10}}"
    valid_rate "$rate"
    render "$rate"
    ;;
  unit)
    unit
    ;;
  *)
    usage
    ;;
esac
