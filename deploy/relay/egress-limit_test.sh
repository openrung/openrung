#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Exercises deploy/relay/egress-limit.sh against a real kernel. Needs root,
# nft, unshare and python3; everything runs in a private network and mount
# namespace, so the host's ruleset, /etc/openrung and systemd units are
# untouched:
#
#   sudo bash deploy/relay/egress-limit_test.sh
set -euo pipefail

SCRIPT="$(cd "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/egress-limit.sh"

if [ "${1:-}" != --in-namespace ]; then
  [ "$(id -u)" = 0 ] || { echo "egress-limit_test: must run as root" >&2; exit 1; }
  exec unshare --net --mount --propagation private bash "${BASH_SOURCE[0]}" --in-namespace
fi

fail() { echo "FAIL: $*" >&2; exit 1; }

mkdir -p /etc/openrung /etc/systemd/system
mount -t tmpfs none /etc/openrung
mount -t tmpfs none /etc/systemd/system
STUB="$(mktemp -d)"
mount -t tmpfs none "$STUB"
# systemd is the host's; the test loads the ruleset itself.
printf '#!/bin/sh\nexit 0\n' > "$STUB/systemctl"
chmod +x "$STUB/systemctl"
export PATH="$STUB:$PATH"

ip link set lo up
ip link add d0 type dummy
ip link set d0 up
ip route add 198.18.0.0/15 dev d0
ip -6 addr add 2001:db8:ffff::1/64 dev d0 nodad
ip -6 route add 2001:db8::/32 dev d0

sh "$SCRIPT" 10 >/dev/null
[ -f /etc/openrung/egress-limit.nft ] || fail "ruleset not written"
[ -f /etc/systemd/system/openrung-egress-limit.service ] || fail "unit not written"
nft -f /etc/openrung/egress-limit.nft
nft -f /etc/openrung/egress-limit.nft || fail "reloading the ruleset must replace it"

# sendto() fails with EPERM when the output hook drops the datagram.
blast() { # family prefix count
  python3 - "$@" <<'PY'
import errno, socket, sys
fam = socket.AF_INET6 if sys.argv[1] == "6" else socket.AF_INET
prefix, count = sys.argv[2], int(sys.argv[3])
sent = 0
for i in range(count):
    addr = prefix + (("%x" % (i + 1)) if fam == socket.AF_INET6 else "%d.%d" % (i // 250, i % 250 + 1))
    s = socket.socket(fam, socket.SOCK_DGRAM)
    try:
        s.sendto(b"x", (addr, 9))
        sent += 1
    except OSError as e:
        if e.errno != errno.EPERM:
            raise
    finally:
        s.close()
print(sent)
PY
}

sent="$(blast 4 198.18. 400)"
[ "$sent" -ge 300 ] && [ "$sent" -le 305 ] || fail "400 new IPv4 destinations: sent $sent, want the 300 burst (+refill)"
sent="$(blast 4 198.18. 300)"
[ "$sent" = 300 ] || fail "known IPv4 destinations must pass: sent $sent of 300"
sent="$(blast 4 127.0. 200)"
[ "$sent" = 200 ] || fail "loopback must be exempt: sent $sent of 200"
sent="$(blast 6 2001:db8:: 50)"
[ "$sent" -le 5 ] || fail "IPv6 shares the drained budget: sent $sent of 50"
sleep 2
sent="$(blast 6 2001:db8:1:: 15)"
[ "$sent" -ge 15 ] || fail "budget must refill at the rate: sent $sent of 15 after 2s"

for bad in 0 abc 010 -5; do
  if sh "$SCRIPT" "$bad" >/dev/null 2>&1; then fail "rate '$bad' must be rejected"; fi
done

sh "$SCRIPT" off >/dev/null
nft list tables | grep -q openrung_egress && fail "'off' must remove the table"
[ ! -e /etc/openrung/egress-limit.nft ] || fail "'off' must remove the ruleset file"
[ ! -e /etc/systemd/system/openrung-egress-limit.service ] || fail "'off' must remove the unit"

echo "egress-limit_test: ok"
