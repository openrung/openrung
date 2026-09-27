#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Exercises deploy/relay/egress-limit.sh against a real kernel. Needs root,
# nft, unshare, ip and python3; everything runs in a private network and mount
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
# Match against captured output: under pipefail, "nft ... | grep -q" fails
# whenever grep exits before nft finishes writing.
chain_has() { [[ "$(nft list chain inet openrung_egress output 2>/dev/null)" == *"$1"* ]]; }
RULES=/etc/openrung/egress-limit.nft
NFT="$(command -v nft)"

mkdir -p /etc/openrung /etc/systemd/system /run/netns
mount -t tmpfs none /etc/openrung
mount -t tmpfs none /etc/systemd/system
mount -t tmpfs none /run/netns
STUB="$(mktemp -d)"
mount -t tmpfs none "$STUB"
# There is no systemd in the namespace. The stub records each call so the test
# can hold the script to the verbs that never run ExecStop on a live unit.
cat > "$STUB/systemctl" <<STUBEOF
#!/bin/sh
echo "\$*" >> "$STUB/systemctl.calls"
STUBEOF
chmod +x "$STUB/systemctl"
export PATH="$STUB:$PATH"
calls() { cat "$STUB/systemctl.calls" 2>/dev/null; : > "$STUB/systemctl.calls"; }
assert_no_stop_verbs() { # calls what
  if grep -Eq '(^| )(restart|reload-or-restart|try-restart|stop|reload)( |$)' <<<"$1"; then
    fail "$2 must not stop, restart or reload the unit; systemctl calls: $1"
  fi
}

ip link set lo up
ip link add d0 type dummy
ip link set d0 up
ip route add 198.18.0.0/16 dev d0
ip -6 addr add 2001:db8:ffff::1/64 dev d0 nodad
ip -6 route add 2001:db8::/32 dev d0

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

# --- install and basic budget -------------------------------------------------
sh "$SCRIPT" 10 >/dev/null
[ -f "$RULES" ] || fail "ruleset not written"
[ ! -e "$RULES.new" ] || fail "staging file left behind"
grep -q '^ExecReload=/usr/sbin/nft -f ' /etc/systemd/system/openrung-egress-limit.service || fail "unit lacks an atomic ExecReload"
assert_no_stop_verbs "$(calls)" "install"
chain_has 'limit rate over 10/second burst 300 packets' || fail "script did not load the ruleset"

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

# --- re-run replaces atomically; a failed load keeps the previous policy ------
sh "$SCRIPT" 20 >/dev/null
chain_has 'over 20/second burst 600' || fail "re-run did not replace the ruleset"
assert_no_stop_verbs "$(calls)" "a re-run"
# An nft that refuses the staged file stands in for any load failure.
cat > "$STUB/nft" <<WRAPEOF
#!/bin/sh
for a in "\$@"; do case "\$a" in *.nft.new) echo "injected load failure" >&2; exit 1 ;; esac; done
exec "$NFT" "\$@"
WRAPEOF
chmod +x "$STUB/nft"
if sh "$SCRIPT" 7 >/dev/null 2>&1; then fail "a failed load must fail the script"; fi
rm "$STUB/nft"
chain_has 'over 20/second burst 600' || fail "a failed load must keep the running ruleset"
grep -q 'over 20/second burst 600' "$RULES" || fail "a failed load must keep the installed ruleset file"
[ ! -e "$RULES.new" ] || fail "a failed load left its staging file behind"
calls >/dev/null

# --- destinations in continuous use never age out -----------------------------
# A peer namespace gives a real TCP connection to a non-loopback address. The
# ruleset is reloaded with a 3s history so expiry happens within the test.
ip netns add peer
ip link add v0 type veth peer name v1 netns peer
ip addr add 198.19.0.1/24 dev v0
ip link set v0 up
ip -n peer link set lo up
ip -n peer addr add 198.19.0.2/24 dev v1
ip -n peer link set v1 up
ip netns exec peer python3 -c '
import socket, threading
srv = socket.create_server(("198.19.0.2", 7000))
def echo(c):
    while (b := c.recv(64)):
        c.sendall(b)
while True:
    threading.Thread(target=echo, args=(srv.accept()[0],), daemon=True).start()
' &
PEER_PID=$!
trap 'kill $PEER_PID 2>/dev/null || true' EXIT

sh "$SCRIPT" 1 >/dev/null
export SHORT_RULES="$STUB/egress-short.nft"
sed 's/timeout 10m/timeout 3s/' "$RULES" > "$SHORT_RULES"
nft -f "$SHORT_RULES"
python3 - <<'PY' || fail "a destination in continuous use was throttled after its history timeout"
import errno, os, socket, subprocess, sys, time

def drain():
    # 60 new destinations exhaust the 30-packet burst at 1/s.
    for i in range(60):
        s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        try:
            s.sendto(b"x", ("198.18.200.%d" % (i + 1), 9))
        except OSError as e:
            if e.errno != errno.EPERM:
                raise
        finally:
            s.close()

def use(conn, seconds):
    end = time.time() + seconds
    while time.time() < end:
        conn.sendall(b"x")
        conn.recv(1)
        time.sleep(0.3)

def second_connection(when):
    drain()
    try:
        socket.create_connection(("198.19.0.2", 7000), timeout=0.8).close()
    except OSError as e:
        sys.exit("second connection %s: %s" % (when, e))

for _ in range(50):
    try:
        conn = socket.create_connection(("198.19.0.2", 7000), timeout=2)
        break
    except OSError:
        time.sleep(0.1)
else:
    sys.exit("peer echo server never came up")
use(conn, 5)  # well past the 3s history timeout
second_connection("after 5s of continuous use")
# A reload empties the history; the live connection's next packets repopulate it.
subprocess.run(["nft", "-f", os.environ["SHORT_RULES"]], check=True)
use(conn, 1)
second_connection("after a reload")
PY

# --- off -----------------------------------------------------------------------
sh "$SCRIPT" off >/dev/null
[[ "$(nft list tables)" != *openrung_egress* ]] || fail "'off' must remove the table"
[ ! -e "$RULES" ] || fail "'off' must remove the ruleset file"
[ ! -e /etc/systemd/system/openrung-egress-limit.service ] || fail "'off' must remove the unit"

echo "egress-limit_test: ok"
