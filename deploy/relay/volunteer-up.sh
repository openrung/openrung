#!/bin/sh
#
# One-line bring-up for a volunteer-run OpenRung relay on any Linux VPS:
#
#   curl -fsSL https://raw.githubusercontent.com/openrung/openrung/main/deploy/relay/volunteer-up.sh | sudo sh
#
# Runs the public relay image with the exact container posture of the
# Foundation fleet (see foundation-up.sh / lightsail-up.sh): host networking,
# --cap-drop ALL --cap-add NET_BIND_SERVICE, read-only rootfs with a /tmp
# tmpfs, configuration in a root-owned mode-0600 /etc/openrung/relay.env —
# except that no OPENRUNG_FOUNDATION_TOKEN is installed, so the broker attests
# the relay volunteer-class.
#
# Re-running the same line is the update path: it preserves the old container
# until a separately named candidate registers and stays running, then promotes
# the candidate and removes the backup. A failed candidate automatically rolls
# back. The env file is preserved, so the stable relay identity
# (OPENRUNG_IDENTITY_SEED, minted once on first run) survives. To change other
# settings, edit /etc/openrung/relay.env and re-run.
#
# Overridable via env — pass through sudo, e.g. to name your relay:
#   curl -fsSL .../volunteer-up.sh | sudo env OPENRUNG_LABEL=my-relay sh
#   OPENRUNG_IMAGE        image to run (default ghcr.io/openrung/openrung-relay:main)
#   OPENRUNG_BROKER_URL   broker to register with (default the broker's direct
#                         TLS origin — the CDN front challenges datacenter IPs)
#   OPENRUNG_PUBLIC_HOST  public IP/DNS clients use to reach this relay
#                         (skips auto-detection; required on the first run when
#                         detection is unavailable)
#   OPENRUNG_LABEL        relay name shown in the public relay directory
#                         (letters, digits, '.', '_', '-'; at most 63
#                         characters — the relay's own label rules); when
#                         unset, the first run generates a random
#                         adjective-noun name in the fleet style. A re-run
#                         against an env file that has no name pins one the
#                         same way, so relays set up before naming existed
#                         stop changing their dashboard name on every restart.
#   OPENRUNG_OPERATOR_CONTACT
#                         contact email for this relay, sent privately to the
#                         broker so the project can reach you if the relay has
#                         an issue; never published in the relay directory.
#                         Optional. Set it (or set it empty to skip) to avoid
#                         the first-run question below.
#   OPENRUNG_REACHABILITY_WAIT
#                         seconds to keep checking whether the broker can
#                         reach this relay's port 443 after the relay is up
#                         (default 600 on a terminal, 0 when output is not a
#                         terminal, e.g. under cloud-init). 0 reports the
#                         verdict once and moves on.
# Overrides configure the FIRST run; once /etc/openrung/relay.env exists it is
# authoritative and overrides are ignored (edit the file instead).
#
# The one question: on a first run with a terminal attached, the script asks
# for an optional contact email (Enter skips; a suggestion found on the host,
# such as the sudo user's git email, is shown and only used when confirmed).
# Standard input is this script under curl|sh, so the question is asked on
# the terminal device. Without a terminal (cloud-init, pipes) or with
# OPENRUNG_OPERATOR_CONTACT set, nothing is asked; later runs never ask.
#
# Reachability: the relay listens on TCP 443, and the broker connects to that
# port from the internet before it accepts the registration; a closed port is
# refused with the reason, and the relay retries on its own until it opens.
# This script opens 443 in the host firewall it can see (ufw, firewalld), then
# reads the broker's answer from the relay log: accepted means serving; refused
# means the install is complete but blocked, so it prints the fix for the
# detected cloud provider and keeps checking while the volunteer opens the
# port. A provider firewall (security group, cloud firewall) lives outside the
# VM: nothing inside it can open that, so the answer is reported, never
# bypassed.
#
# POSIX sh on purpose: this must run on whatever /bin/sh the volunteer's
# distro ships (dash, busybox ash, bash).
#
# Everything except helper definitions lives in main(), invoked on the last
# line: if the curl|sh stream is truncated mid-download, the shell hits EOF
# inside an unterminated function and parses nothing — a partial download can
# never execute a destructive prefix (like removing the serving container).
set -eu

# A private key (the identity seed) transits this script's variables: keep
# xtrace off even when a volunteer debugs with 'sudo sh -x', so the seed can
# never appear in trace output — same guard as foundation-up.sh and the fleet
# bootstrap user-data.
{ set +x; } 2>/dev/null

die() { echo "volunteer-up: error: $*" >&2; exit 1; }
log() { echo "volunteer-up: $*"; }

# Default relay names: the same adjective-noun style the fleet provisioning
# helpers use (lightsail-up.sh / hetzner-up.sh), so volunteer relays show up
# in the dashboard alongside fleet names instead of as raw relay-ID hex. The
# label is published in the relay directory: a generated name is also the
# privacy-safe default — never derive one from the hostname or username.
#
# The vocabulary itself lives in the relay binary (internal/relayruntime/
# label_*.txt, compiled in via go:embed), so this script asks the image it has
# already pulled instead of carrying its own copy of the word lists. A second
# copy here would silently drift from the canonical one, which is exactly how
# the fleet ended up naming relays out of a 1,296-name space.
#
# Requires that the pull has already run — see the docker pull before the
# env-file branch below.
random_label() {
    # The image ENTRYPOINT builds its own argument list and discards anything
    # passed to `docker run`, so invoke the binary directly. It only prints a
    # name and exits: no network, no config, no privileged port.
    rl_label="$(docker run --rm --entrypoint /usr/local/bin/relay "$IMAGE" -print-label 2>/dev/null)" || rl_label=""
    rl_label="$(printf '%s' "$rl_label" | tr -d '\r\n')"
    if validate_label "$rl_label"; then
        printf '%s' "$rl_label"
        return 0
    fi
    # An image pinned via OPENRUNG_IMAGE may predate -print-label. Fall back to
    # random hex rather than reintroducing a word list: it needs no vocabulary
    # to stay in sync, and four random bytes collide far less often than an
    # adjective-noun pair does.
    printf 'relay-%s' "$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')"
}

# Mirrors the relay binary's own label rules (relay.NormalizeLabel,
# internal/relay/types.go): slug charset, at most 63 characters. The label is
# ASCII-only after the charset check, so ${#...} counts characters exactly.
# Enforcing the limit here matters because the relay exits at startup on an
# invalid label: a bad value persisted into relay.env would crash-loop every
# later run until someone edits the file by hand.
validate_label() { # label
    case "$1" in '' | *[!A-Za-z0-9._-]*) return 1 ;; esac
    [ "${#1}" -le 63 ]
}

# Mirrors the broker's rule (relay.NormalizeOperatorContact,
# internal/relay/types.go): printable ASCII without spaces, at most 128
# characters, exactly one '@' after a non-empty local part, and a domain with a
# dot that neither starts nor ends it. A value the broker refuses would make the
# relay exit at startup, so it must never reach relay.env. Whitespace and
# control characters matter twice: the value lands in the env file, where a
# newline would inject a variable.
validate_contact() { # contact
    # The case pattern sees the whole value, embedded newlines included; the
    # grep below works line by line and cannot.
    case "$1" in '' | @* | *@*@* | *[![:graph:]]*) return 1 ;; esac
    # Byte-wise under the C locale: [:graph:] alone would admit non-ASCII
    # letters under bash in a UTF-8 locale.
    if printf '%s' "$1" | LC_ALL=C grep -q '[^!-~]'; then return 1; fi
    vc_domain=${1#*@}
    [ "$vc_domain" != "$1" ] || return 1
    case "$vc_domain" in .* | *.) return 1 ;; *.*) ;; *) return 1 ;; esac
    [ "${#1}" -le 128 ]
}

# A contact suggestion for the question: the git user.email of the user who
# ran sudo (or of root's own home when run as root). Only ever shown in the
# question and used when the operator confirms it — never stored silently.
suggest_contact() {
    command -v git >/dev/null 2>&1 || return 0
    if [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != root ]; then
        sc_home="$(awk -F: -v u="$SUDO_USER" '$1 == u { print $6; exit }' "$PASSWD_FILE" 2>/dev/null)" || sc_home=""
    else
        sc_home="${HOME:-}"
    fi
    [ -n "$sc_home" ] || return 0
    for sc_file in "$sc_home/.gitconfig" "$sc_home/.config/git/config"; do
        [ -f "$sc_file" ] || continue
        sc_email="$(git config --file "$sc_file" --get user.email 2>/dev/null)" || continue
        if validate_contact "$sc_email"; then
            printf '%s' "$sc_email"
            return 0
        fi
    done
}

# Ask for the optional operator contact on the terminal device and print the
# answer (empty when skipped). Enter confirms a shown suggestion; a typed value
# replaces it; Enter without a suggestion, end of input, or three invalid
# answers skip. The caller only asks when a terminal can actually be opened.
ask_operator_contact() {
    ac_suggestion="$(suggest_contact)" || ac_suggestion=""
    ac_tries=0
    exec 3<"$TTY_DEVICE"
    while :; do
        ac_tries=$((ac_tries + 1))
        if [ -n "$ac_suggestion" ]; then
            printf '%s [%s]: ' "$CONTACT_QUESTION" "$ac_suggestion" >>"$TTY_DEVICE"
        else
            printf '%s: ' "$CONTACT_QUESTION" >>"$TTY_DEVICE"
        fi
        ac_answer=""
        if ! read -r ac_answer <&3; then
            printf '\n' >>"$TTY_DEVICE"
            ac_answer=""
            break
        fi
        if [ -z "$ac_answer" ]; then ac_answer="$ac_suggestion"; fi
        if [ -z "$ac_answer" ] || validate_contact "$ac_answer"; then break; fi
        if [ "$ac_tries" -ge 3 ]; then
            printf 'volunteer-up: skipping the contact; set OPENRUNG_OPERATOR_CONTACT in %s later if you want one\n' "$ENV_FILE" >>"$TTY_DEVICE"
            ac_answer=""
            break
        fi
        printf 'volunteer-up: that does not look like an email address (no spaces, at most 128 characters); press Enter to skip\n' >>"$TTY_DEVICE"
    done
    exec 3<&-
    printf '%s' "$ac_answer"
}

# A terminal is attached when the terminal device opens; /dev/tty exists even
# without a controlling terminal, so test by opening it.
have_terminal() {
    (: <"$TTY_DEVICE" >>"$TTY_DEVICE") 2>/dev/null
}

# Strict dotted-quad, publicly-routable IPv4. The detection services' responses
# are untrusted input headed for the env file and the public relay directory: a
# glitched or truncated body must not register garbage, loopback, or private
# space as this relay's public address.
is_public_ipv4() {
    case "$1" in *[!0-9.]* | '' | .* | *. | *.*.*.*.* | *..*) return 1 ;; esac
    old_ifs=$IFS
    IFS=.
    # shellcheck disable=SC2086
    set -- $1
    IFS=$old_ifs
    [ "$#" -eq 4 ] || return 1
    for octet in "$@"; do
        # Reject leading zeros: some client IP parsers refuse (or read as
        # octal) addresses like 1.2.3.04, and no detection service emits them.
        case "$octet" in 0) ;; 0*) return 1 ;; esac
        [ "${#octet}" -le 3 ] || return 1
        [ "$octet" -le 255 ] || return 1
    done
    if [ "$1" -eq 0 ] || [ "$1" -eq 10 ] || [ "$1" -eq 127 ] || [ "$1" -ge 224 ]; then return 1; fi
    if [ "$1" -eq 100 ] && [ "$2" -ge 64 ] && [ "$2" -le 127 ]; then return 1; fi   # CGNAT
    if [ "$1" -eq 169 ] && [ "$2" -eq 254 ]; then return 1; fi
    if [ "$1" -eq 172 ] && [ "$2" -ge 16 ] && [ "$2" -le 31 ]; then return 1; fi
    # Special-purpose ranges that are not usable as a unique, globally
    # reachable VPS endpoint: IETF protocol assignments, TEST-NET-1/2/3,
    # deprecated 6to4 relay anycast, RFC1918, and benchmarking space.
    case "$1.$2.$3" in
        192.0.0 | 192.0.2 | 192.88.99 | 192.168.* \
            | 198.18.* | 198.19.* | 198.51.100 | 203.0.113) return 1 ;;
    esac
    return 0
}

detect_public_ipv4() {
    # The relay's public address is published in the relay directory anyway, so
    # a detection service learns nothing new. IPv4 on purpose: it matches the
    # rest of the fleet, and an IPv4 relay is reachable from v6-only clients
    # via their carrier's 464XLAT while the reverse is not true.
    #
    # Each attempt is captured and validated separately — a partial body from
    # a failed attempt must never concatenate with a later one. Command
    # substitution strips the trailing newline; is_public_ipv4 rejects any
    # other stray byte. busybox wget has no -4 flag, hence the plain-wget
    # retry; a dual-stack IPv6 answer from it fails validation harmlessly.
    for url in https://api.ipify.org https://checkip.amazonaws.com; do
        if command -v curl >/dev/null 2>&1; then
            ip="$(curl -fsS4 --max-time 10 "$url" 2>/dev/null)" || ip=""
        elif command -v wget >/dev/null 2>&1; then
            ip="$(wget -4 -q -T 10 -O - "$url" 2>/dev/null)" \
                || ip="$(wget -q -T 10 -O - "$url" 2>/dev/null)" \
                || ip=""
        else
            return 1
        fi
        if is_public_ipv4 "$ip"; then
            printf '%s' "$ip"
            return 0
        fi
    done
    return 1
}

container_inspect() {
    docker inspect --type container "$@"
}

container_exists() {
    container_inspect "$1" >/dev/null 2>&1
}

container_id() {
    container_inspect -f '{{.Id}}' "$1" 2>/dev/null
}

assert_volunteer_env() {
    if [ ! -e "$ENV_FILE" ] && [ ! -L "$ENV_FILE" ]; then return 0; fi
    [ -f "$ENV_FILE" ] || die "$ENV_FILE exists but is not a regular file"
    if grep -Eq '^OPENRUNG_FOUNDATION_TOKEN(=|[[:space:]]|$)' "$ENV_FILE"; then
        die "$ENV_FILE contains an OPENRUNG_FOUNDATION_TOKEN assignment; this host is Foundation-managed — use deploy/relay/foundation-up.sh update"
    else
        grep_status=$?
        [ "$grep_status" -eq 1 ] || die "could not inspect $ENV_FILE for Foundation ownership"
    fi
}

container_foundation_marker() {
    container_inspect -f '{{range .Config.Env}}{{if eq (index (split . "=") 0) "OPENRUNG_FOUNDATION_TOKEN"}}present{{end}}{{end}}' \
        "$1" 2>/dev/null
}

# Best-effort rollback for a transaction that has already moved the previous
# relay aside. This function deliberately does not call die(): it runs from the
# exit trap, where recursive exits could strand the host in a worse state.
rollback_transaction() {
    set +e
    rollback_cleanup_ok=1

    if container_exists "$CONTAINER"; then
        rollback_current_id="$(container_id "$CONTAINER")"
        if [ -n "$OLD_ID" ] && [ "$rollback_current_id" = "$OLD_ID" ]; then
            rollback_running="$(container_inspect -f '{{.State.Running}}' "$CONTAINER" 2>/dev/null)"
            if [ "$rollback_running" != true ]; then docker start "$CONTAINER" >/dev/null; fi
            [ "$(container_inspect -f '{{.State.Running}}' "$CONTAINER" 2>/dev/null)" = true ]
            return
        fi
        # A signal can arrive after the verified candidate was renamed but
        # before the transaction flag was cleared. Never overwrite it.
        if [ -n "$CANDIDATE_ID" ] && [ "$rollback_current_id" = "$CANDIDATE_ID" ]; then
            return 0
        fi
        echo "volunteer-up: error: rollback found an unexpected '$CONTAINER' container; refusing to overwrite it" >&2
        return 1
    fi

    if [ -n "$CANDIDATE_ID" ] && container_exists "$CANDIDATE_ID"; then
        if [ "$(container_id "$CANDIDATE_ID")" != "$CANDIDATE_ID" ] \
            || ! docker rm -f "$CANDIDATE_ID" >/dev/null 2>&1; then
            rollback_cleanup_ok=0
        fi
    elif container_exists "$NEW_CONTAINER"; then
        echo "volunteer-up: error: rollback does not own '$NEW_CONTAINER'; refusing to remove that ambiguous container" >&2
        rollback_cleanup_ok=0
    fi

    if [ "$HAD_PREVIOUS" = 0 ]; then
        [ "$rollback_cleanup_ok" = 1 ]
        return
    fi
    container_exists "$OLD_CONTAINER" || {
        echo "volunteer-up: error: rollback could not find the previous '$OLD_CONTAINER' container" >&2
        return 1
    }
    rollback_old_id="$(container_id "$OLD_CONTAINER")"
    if [ -z "$OLD_ID" ] || [ "$rollback_old_id" != "$OLD_ID" ]; then
        echo "volunteer-up: error: rollback found that '$OLD_CONTAINER' changed identity; refusing to rename it" >&2
        return 1
    fi
    docker rename "$OLD_CONTAINER" "$CONTAINER" >/dev/null || return 1
    rollback_running="$(container_inspect -f '{{.State.Running}}' "$CONTAINER" 2>/dev/null)"
    if [ "$rollback_running" != true ]; then docker start "$CONTAINER" >/dev/null || return 1; fi
    if [ "$(container_inspect -f '{{.State.Running}}' "$CONTAINER" 2>/dev/null)" != true ]; then
        return 1
    fi
    log "restored the previous relay after the failed update"
    [ "$rollback_cleanup_ok" = 1 ]
    return
}

transaction_exit() {
    transaction_status=$1
    trap - 0 HUP INT TERM
    if [ "$TRANSACTION_ACTIVE" = 1 ]; then
        if ! rollback_transaction; then
            echo "volunteer-up: error: automatic rollback was incomplete; inspect '$CONTAINER', '$OLD_CONTAINER', and '$NEW_CONTAINER' before making changes" >&2
            transaction_status=1
        fi
    fi
    exit "$transaction_status"
}

# --- reachability ---------------------------------------------------------
# The relay serves on this port; it is the only port the script sets up.
RELAY_PORT=443

# Allows inbound TCP 443 in the host firewall when the host runs one of the
# two front-ends that own the whole ruleset: ufw (when active) or firewalld
# (when running). Both calls are idempotent. A hand-written nftables or
# iptables policy is left alone — inserting into somebody's ordered ruleset
# blind can break it — and is covered by the verdict below instead. Docker
# never opens this port: the container runs on the host network, which has
# no published-port rules. Never fatal; a failure here is reported and the
# broker's verdict says whether it mattered.
open_host_firewall() {
    if command -v ufw >/dev/null 2>&1; then
        ufw_state="$(ufw status 2>/dev/null | head -n 1)" || ufw_state=""
        case "$ufw_state" in
            "Status: active"*)
                if ufw allow "${RELAY_PORT}/tcp" >/dev/null 2>&1; then
                    log "host firewall: ufw allows ${RELAY_PORT}/tcp"
                else
                    log "warning: could not allow ${RELAY_PORT}/tcp in ufw; run: ufw allow ${RELAY_PORT}/tcp"
                fi
                ;;
        esac
    fi
    if command -v firewall-cmd >/dev/null 2>&1 \
        && [ "$(firewall-cmd --state 2>/dev/null)" = running ]; then
        # Runtime and permanent separately, rather than --reload, which would
        # drop runtime-only rules the operator added by hand.
        if firewall-cmd --add-port="${RELAY_PORT}/tcp" >/dev/null 2>&1 \
            && firewall-cmd --permanent --add-port="${RELAY_PORT}/tcp" >/dev/null 2>&1; then
            log "host firewall: firewalld allows ${RELAY_PORT}/tcp"
        else
            log "warning: could not allow ${RELAY_PORT}/tcp in firewalld; run: firewall-cmd --permanent --add-port=${RELAY_PORT}/tcp && firewall-cmd --reload"
        fi
    fi
}

# Prints a provider token (aws, hetzner, gcp, azure, oracle, digitalocean,
# vultr, alibaba, tencent) from the DMI tables the hypervisor fills, or
# nothing when unknown. DMI needs no network and no metadata-service
# credentials; it only decides which instructions to print.
detect_provider() {
    dp_vendor="$(cat "$DMI_DIR/sys_vendor" 2>/dev/null)" || dp_vendor=""
    dp_product="$(cat "$DMI_DIR/product_name" 2>/dev/null)" || dp_product=""
    dp_asset="$(cat "$DMI_DIR/chassis_asset_tag" 2>/dev/null)" || dp_asset=""
    dp_bios="$(cat "$DMI_DIR/bios_version" 2>/dev/null)" || dp_bios=""
    case "$dp_asset" in
        OracleCloud.com) echo oracle; return 0 ;;
        7783-7084-3265-9085-8269-3286-77) echo azure; return 0 ;;
    esac
    case "$dp_vendor $dp_product $dp_bios" in
        "Amazon EC2"* | *amazon* | *"HVM domU"*) echo aws ;;
        Hetzner*) echo hetzner ;;
        Google*) echo gcp ;;
        DigitalOcean*) echo digitalocean ;;
        Vultr*) echo vultr ;;
        "Alibaba Cloud"*) echo alibaba ;;
        "Tencent Cloud"*) echo tencent ;;
        *) ;;
    esac
}

# Prints, for the detected provider, where the firewall outside the VM is
# opened. Every hint names the console path and the CLI equivalent; the
# generic fallback covers providers DMI does not identify.
provider_firewall_hint() { # provider
    case "$1" in
        aws)
            echo "  AWS: allow inbound TCP ${RELAY_PORT} from 0.0.0.0/0 AND ::/0."
            echo "    EC2: Security Groups -> the instance's group -> Inbound rules -> Add rule."
            echo "         aws ec2 authorize-security-group-ingress --group-id <sg-id> --protocol tcp --port ${RELAY_PORT} --cidr 0.0.0.0/0"
            echo "    Lightsail: the instance -> Networking -> IPv4 firewall -> Add rule (TCP ${RELAY_PORT}); repeat under IPv6."
            echo "         aws lightsail open-instance-public-ports --instance-name <name> --port-info fromPort=${RELAY_PORT},toPort=${RELAY_PORT},protocol=tcp"
            ;;
        hetzner)
            echo "  Hetzner Cloud: Firewalls -> the firewall attached to this server -> Inbound -> add TCP ${RELAY_PORT} from 0.0.0.0/0 and ::/0."
            echo "    hcloud firewall add-rule <firewall> --direction in --protocol tcp --port ${RELAY_PORT} --source-ips 0.0.0.0/0 --source-ips ::/0"
            ;;
        gcp)
            echo "  Google Cloud: VPC network -> Firewall -> Create rule: ingress, all instances (or this one's tag), TCP ${RELAY_PORT} from 0.0.0.0/0."
            echo "    gcloud compute firewall-rules create openrung-relay --direction INGRESS --allow tcp:${RELAY_PORT} --source-ranges 0.0.0.0/0"
            ;;
        azure)
            echo "  Azure: the VM -> Networking -> Network security group -> Inbound security rules -> Add: TCP ${RELAY_PORT}, Allow."
            echo "    az network nsg rule create -g <resource-group> --nsg-name <nsg> -n openrung-relay --priority 1000 --protocol Tcp --destination-port-ranges ${RELAY_PORT} --access Allow"
            ;;
        oracle)
            echo "  Oracle Cloud: Networking -> the VCN -> Security Lists (or the NSG) -> Ingress rule: TCP ${RELAY_PORT} from 0.0.0.0/0."
            echo "  Oracle images also ship an iptables REJECT rule inside the VM:"
            echo "    iptables -I INPUT 6 -p tcp --dport ${RELAY_PORT} -j ACCEPT && netfilter-persistent save"
            ;;
        digitalocean)
            echo "  DigitalOcean: Networking -> Firewalls -> the firewall on this Droplet -> Inbound Rules -> add TCP ${RELAY_PORT}."
            echo "    doctl compute firewall add-rules <firewall-id> --inbound-rules 'protocol:tcp,ports:${RELAY_PORT},address:0.0.0.0/0,address:::/0'"
            ;;
        vultr)
            echo "  Vultr: Products -> Firewall -> the group linked to this instance -> add inbound TCP ${RELAY_PORT} (or unlink the group)."
            ;;
        alibaba)
            echo "  Alibaba Cloud: ECS -> Security Groups -> the instance's group -> Inbound -> Add rule: TCP ${RELAY_PORT} from 0.0.0.0/0."
            ;;
        tencent)
            echo "  Tencent Cloud: CVM -> Security Groups -> the instance's group -> Inbound rules -> add TCP ${RELAY_PORT} from 0.0.0.0/0."
            ;;
        *)
            echo "  Your provider's console: the VM's firewall / security group -> inbound rule allowing TCP ${RELAY_PORT} from anywhere (0.0.0.0/0 and ::/0)."
            ;;
    esac
}

# The broker's latest answer from the relay log: 'reachable' once the broker
# accepted a registration ("registered relay"; it connects to the port before
# accepting), 'unreachable' while it refuses one ("public endpoint
# unreachable", the refusal the relay logs verbatim), empty before either.
# Newest line wins, so a refusal followed by a registration is reachable. Both
# phrases are deployment contracts: cmd/relay/main.go for the first,
# internal/broker/reachability.go for the second.
reachability_verdict() { # container
    rv_last="$(docker logs "$1" 2>&1 | grep -oE 'public endpoint unreachable|registered relay' | tail -n 1)"
    case "$rv_last" in
        'registered relay') echo reachable ;;
        'public endpoint unreachable') echo unreachable ;;
    esac
}

# Reports whether the broker accepted the relay and, while the port is closed,
# prints the provider's fix and keeps re-reading the answer for up to
# REACHABILITY_WAIT seconds: the relay re-registers on its own (rounds of
# attempts backed off to a minute apart) and the broker probes the port each
# time, so a port opened in another window turns the answer within about a
# minute. The relay stays installed
# either way — it registers by itself once the port opens — so this never
# fails the run.
report_reachability() { # container
    rr_container=$1
    rr_verdict="$(reachability_verdict "$rr_container")"
    case "$rr_verdict" in
        reachable)
            log "done — this host is now serving as a volunteer-class OpenRung relay (the broker connected to its public port ${RELAY_PORT} before accepting it)"
            return 0
            ;;
        '')
            log "note: the relay has neither been accepted nor refused by the broker yet; check later with: docker logs $CONTAINER 2>&1 | grep -E 'registered relay|public endpoint'"
            return 0
            ;;
    esac
    rr_provider="$(detect_provider)"
    echo "volunteer-up: WARNING: the broker could not connect to this relay on TCP ${RELAY_PORT} and refused its registration, so no client is being handed it yet." >&2
    echo "volunteer-up: the relay itself is installed and running, retrying by itself; something outside it blocks the port. Open it here:" >&2
    provider_firewall_hint "$rr_provider" >&2
    echo "volunteer-up: (host firewalls: ufw and firewalld were handled by this script; a hand-written nftables/iptables policy needs its own accept rule for TCP ${RELAY_PORT})" >&2
    if [ "$REACHABILITY_WAIT" -le 0 ]; then
        echo "volunteer-up: the relay registers by itself once the port opens; check with: docker logs $CONTAINER 2>&1 | grep -E 'registered relay|public endpoint'" >&2
        return 0
    fi
    log "waiting up to ${REACHABILITY_WAIT}s for the port to open (Ctrl-C stops waiting; the relay keeps running)"
    rr_waited=0
    while [ "$rr_waited" -lt "$REACHABILITY_WAIT" ]; do
        sleep 5
        rr_waited=$((rr_waited + 5))
        if [ "$(reachability_verdict "$rr_container")" = reachable ]; then
            log "done — the broker accepted this relay now: this host is serving as a volunteer-class OpenRung relay"
            return 0
        fi
        if [ $((rr_waited % 60)) -eq 0 ]; then
            log "still unreachable after ${rr_waited}s; waiting for inbound TCP ${RELAY_PORT} to open"
        fi
    done
    echo "volunteer-up: WARNING: still unreachable after ${REACHABILITY_WAIT}s. The relay keeps retrying and registers by itself once the port opens; check with: docker logs $CONTAINER 2>&1 | grep -E 'registered relay|public endpoint'" >&2
    return 0
}

# Returns 0 once the candidate has registered, 2 when it is running but the
# broker refused its registration because the port is unreachable (the
# candidate is healthy — the relay keeps retrying by itself — so an update
# must still promote it rather than roll back to a version with the same
# closed port), 1 when it stopped, restarted or did neither within 120s. The
# window covers the relay's three startup registration attempts (each a
# bounded broker probe, seconds apart) before it logs a refusal.
verify_candidate() {
    verify_container=$1
    log "waiting for the relay candidate to register with the broker"
    verify_i=0
    while :; do
        verify_state="$(container_inspect -f '{{.State.Running}} {{.RestartCount}}' "$verify_container" 2>/dev/null)" \
            || verify_state=""
        verify_logs="$(docker logs "$verify_container" 2>&1)" || verify_logs=""
        if [ "$verify_state" = "true 0" ] \
            && printf '%s\n' "$verify_logs" | grep -q 'registered relay'; then
            log "$(printf '%s\n' "$verify_logs" | grep 'registered relay' | tail -1)"
            return 0
        fi
        if [ "$verify_state" = "true 0" ] \
            && printf '%s\n' "$verify_logs" | grep -q 'public endpoint unreachable'; then
            log "the relay candidate is running; the broker refused its registration because its public port is unreachable"
            return 2
        fi

        case "$verify_state" in
            true\ 0) ;;
            *)
                echo "volunteer-up: error: the relay candidate stopped or restarted (state: ${verify_state:-unavailable}); recent logs:" >&2
                docker logs --tail 25 "$verify_container" >&2 || true
                return 1
                ;;
        esac

        verify_i=$((verify_i + 1))
        if [ "$verify_i" -ge 60 ]; then
            echo "volunteer-up: error: the relay candidate did not register within 120s; recent logs:" >&2
            docker logs --tail 25 "$verify_container" >&2 || true
            return 1
        fi
        sleep 2
    done
}

main() {
    IMAGE="${OPENRUNG_IMAGE:-ghcr.io/openrung/openrung-relay:main}"
    BROKER_URL="${OPENRUNG_BROKER_URL:-https://broker-origin.openrung.org}"
    ENV_FILE=/etc/openrung/relay.env
    DMI_DIR=/sys/class/dmi/id
    TTY_DEVICE=/dev/tty
    PASSWD_FILE=/etc/passwd
    CONTACT_QUESTION="volunteer-up: contact email for this relay (Optional, we will only contact you if there is an issue with your relay)"
    # A terminal has someone to open the port while we wait; automation
    # (cloud-init, a non-interactive shell) gets the verdict once and moves on.
    if [ -t 1 ]; then REACHABILITY_WAIT_DEFAULT=600; else REACHABILITY_WAIT_DEFAULT=0; fi
    REACHABILITY_WAIT="${OPENRUNG_REACHABILITY_WAIT:-$REACHABILITY_WAIT_DEFAULT}"
    case "$REACHABILITY_WAIT" in *[!0-9]* | '') die "OPENRUNG_REACHABILITY_WAIT must be a whole number of seconds" ;; esac
    CONTAINER=openrung-relay
    OLD_CONTAINER=openrung-relay-old
    NEW_CONTAINER=openrung-relay-new
    TRANSACTION_ACTIVE=0
    HAD_PREVIOUS=0
    OLD_ID=""
    CANDIDATE_ID=""

    [ "$(id -u)" = 0 ] \
        || die "must run as root — rerun as:  curl -fsSL .../volunteer-up.sh | sudo sh"

    # Operator-set values end up in an env file and in docker arguments; refuse
    # whitespace and control characters (an embedded newline would inject extra
    # variables into the file). case patterns match the whole string, so a
    # value containing a newline cannot sneak past a per-line check.
    case "$BROKER_URL" in *[![:graph:]]* | '') die "OPENRUNG_BROKER_URL is empty or contains whitespace/control characters" ;; esac
    case "$IMAGE" in *[!A-Za-z0-9:/@._-]* | '') die "OPENRUNG_IMAGE contains unexpected characters" ;; esac
    if [ -n "${OPENRUNG_LABEL:-}" ]; then
        validate_label "$OPENRUNG_LABEL" \
            || die "OPENRUNG_LABEL may use only letters, digits, '.', '_', '-', and at most 63 characters (the relay refuses longer labels at startup)"
    fi
    if [ -n "${OPENRUNG_OPERATOR_CONTACT:-}" ]; then
        validate_contact "$OPENRUNG_OPERATOR_CONTACT" \
            || die "OPENRUNG_OPERATOR_CONTACT must be an email address without spaces, at most 128 characters"
    fi

    # The Foundation conversion workflow deliberately uses this same canonical
    # env path. A Foundation token is therefore an ownership boundary, not an
    # override this volunteer-only helper may preserve. Reject the key itself
    # (even empty or duplicated) before installing Docker, pulling an image, or
    # touching a container; Docker's last-duplicate-wins semantics must not let
    # an ambiguous credential file be mistaken for volunteer state.
    assert_volunteer_env
    if [ -e /etc/openrung/volunteer.env ] || [ -L /etc/openrung/volunteer.env ]; then
        die "legacy /etc/openrung/volunteer.env found; migrate its settings into $ENV_FILE (see deploy/relay/README.md) and remove it, then re-run"
    fi

    # Ask first, while the operator who just ran the command is still at the
    # terminal: a fresh host spends the next minutes installing and pulling.
    # Only a first run asks (relay.env is authoritative afterwards), and a set
    # OPENRUNG_OPERATOR_CONTACT — even an empty one — answers the question.
    OPERATOR_CONTACT="${OPENRUNG_OPERATOR_CONTACT:-}"
    if [ ! -f "$ENV_FILE" ] && [ -z "${OPENRUNG_OPERATOR_CONTACT+set}" ] && have_terminal; then
        OPERATOR_CONTACT="$(ask_operator_contact)" || OPERATOR_CONTACT=""
    fi

    # Same package the Foundation fleet bootstrap installs (lightsail-up.sh
    # user-data). On non-apt distros, installing Docker is the volunteer's step.
    if ! command -v docker >/dev/null 2>&1; then
        if command -v apt-get >/dev/null 2>&1; then
            log "installing docker.io"
            export DEBIAN_FRONTEND=noninteractive
            apt-get -o DPkg::Lock::Timeout=300 update </dev/null
            apt-get -o DPkg::Lock::Timeout=300 install -y docker.io </dev/null
            if command -v systemctl >/dev/null 2>&1; then systemctl enable --now docker; fi
        else
            die "docker is not installed and this is not an apt-based system; install Docker (https://docs.docker.com/engine/install/) and re-run"
        fi
    fi

    docker_names="$(docker ps -a --format '{{.Names}}' 2>/dev/null)" \
        || die "could not inspect Docker containers; is the Docker daemon running?"

    # Legacy artifacts mean an old deploy/volunteer setup is (or was) serving
    # here; both would contend for port 443 on the host network, and
    # foundation-up.sh refuses hosts carrying them. Refuse rather than guess —
    # the migration steps are in deploy/relay/README.md.
    if printf '%s\n' "$docker_names" | grep -qx openrung-volunteer; then
        die "legacy 'openrung-volunteer' container found; migrate it first (see deploy/relay/README.md), or remove it with: docker rm -f openrung-volunteer"
    fi
    if printf '%s\n' "$docker_names" | grep -qx "$OLD_CONTAINER" \
        || printf '%s\n' "$docker_names" | grep -qx "$NEW_CONTAINER"; then
        die "an interrupted relay update is present; refusing to guess between '$OLD_CONTAINER' and '$NEW_CONTAINER'. Inspect them, restore the known-good container as '$CONTAINER', and re-run"
    fi

    # An existing openrung-relay container this script did not set up — a
    # Compose project (docker-compose.yml pins the same container name) or a
    # hand-run container with its settings in docker -e arguments — must not be
    # silently destroyed: recreating it from $ENV_FILE would rotate its relay
    # identity and drop its settings.
    LIVE_EXISTS=0
    if printf '%s\n' "$docker_names" | grep -qx "$CONTAINER"; then
        LIVE_EXISTS=1
        compose_project="$(container_inspect -f '{{index .Config.Labels "com.docker.compose.project"}}' "$CONTAINER" 2>/dev/null)" \
            || die "could not inspect existing '$CONTAINER' container"
        if [ -n "$compose_project" ]; then
            die "existing '$CONTAINER' container is managed by docker compose (project '$compose_project'); update it with compose (see deploy/relay/README.md) instead of this script"
        fi
        if [ ! -f "$ENV_FILE" ]; then
            die "existing '$CONTAINER' container was not set up by this script (no $ENV_FILE); refusing to replace it — its identity and settings live only in that container. To hand management to this script, remove it first: docker rm -f $CONTAINER"
        fi
        foundation_container_marker="$(container_foundation_marker "$CONTAINER")" \
            || die "could not inspect '$CONTAINER' for Foundation ownership"
        [ -z "$foundation_container_marker" ] \
            || die "existing '$CONTAINER' carries OPENRUNG_FOUNDATION_TOKEN; this host is Foundation-managed — use deploy/relay/foundation-up.sh update"
        live_running="$(container_inspect -f '{{.State.Running}}' "$CONTAINER" 2>/dev/null)" \
            || die "could not inspect whether '$CONTAINER' is running"
        [ "$live_running" = true ] \
            || die "existing '$CONTAINER' is not running; inspect and recover it before attempting an update"
    fi

    # Before the relay registers: the broker probes the port in the
    # registration request, and a host firewall opened first passes it.
    open_host_firewall

    # Pull before naming: random_label asks this image for the name, and the
    # env file written below must record whatever it returns.
    log "pulling $IMAGE"
    docker pull "$IMAGE" >/dev/null

    if [ -f "$ENV_FILE" ]; then
        log "reusing existing $ENV_FILE (stable relay identity preserved)"
        # Env files written before naming existed carry no label, and the
        # relay binary then generates a fresh random name on every restart
        # (cmd/relay/main.go) — the dashboard name churns. Pin one: the
        # OPENRUNG_LABEL override when given, else a generated name. Appending
        # is authoritative under docker's last-duplicate-wins --env-file
        # semantics and never rewrites the identity lines above it.
        effective_label="$(sed -n 's/^OPENRUNG_LABEL=//p' "$ENV_FILE" | tail -1)" || effective_label=""
        if [ -z "$effective_label" ]; then
            LABEL="${OPENRUNG_LABEL:-}"
            if [ -z "$LABEL" ]; then
                LABEL="$(random_label)"
                [ -n "$LABEL" ] || die "could not generate a relay name"
            fi
            umask 077
            { cat "$ENV_FILE" && printf 'OPENRUNG_LABEL=%s\n' "$LABEL"; } >"$ENV_FILE.tmp" \
                || die "could not stage the relay name into $ENV_FILE.tmp"
            mv "$ENV_FILE.tmp" "$ENV_FILE"
            log "pinned relay name '${LABEL}' into $ENV_FILE (this relay was unnamed; its dashboard name changed on every restart)"
        elif [ -n "${OPENRUNG_LABEL:-}" ]; then
            log "note: OPENRUNG_LABEL does not change an existing name — edit $ENV_FILE and re-run instead"
        fi
        # Same rule as the name: a relay set up before contacts existed takes
        # OPENRUNG_OPERATOR_CONTACT once; an existing contact is never changed
        # and the question is never asked again.
        effective_contact="$(sed -n 's/^OPENRUNG_OPERATOR_CONTACT=//p' "$ENV_FILE" | tail -1)" || effective_contact=""
        if [ -z "$effective_contact" ] && [ -n "$OPERATOR_CONTACT" ]; then
            umask 077
            { cat "$ENV_FILE" && printf 'OPENRUNG_OPERATOR_CONTACT=%s\n' "$OPERATOR_CONTACT"; } >"$ENV_FILE.tmp" \
                || die "could not stage the operator contact into $ENV_FILE.tmp"
            mv "$ENV_FILE.tmp" "$ENV_FILE"
            log "saved operator contact '${OPERATOR_CONTACT}' in $ENV_FILE (sent privately to the broker; never published)"
        elif [ -n "$OPERATOR_CONTACT" ] && [ "$OPERATOR_CONTACT" != "$effective_contact" ]; then
            log "note: OPENRUNG_OPERATOR_CONTACT does not change an existing contact — edit $ENV_FILE and re-run instead"
        fi
        if [ -n "${OPENRUNG_BROKER_URL:-}" ] || [ -n "${OPENRUNG_PUBLIC_HOST:-}" ]; then
            log "note: OPENRUNG_* overrides do not change an existing $ENV_FILE — edit that file and re-run instead"
        fi
        # Diagnostics below must name the broker the relay actually uses: the
        # env file is authoritative, last occurrence wins (docker --env-file
        # semantics).
        effective_broker="$(sed -n 's/^OPENRUNG_BROKER_URL=//p' "$ENV_FILE" | tail -1)" || effective_broker=""
        if [ -n "$effective_broker" ]; then BROKER_URL="$effective_broker"; fi
    else
        PUBLIC_HOST="${OPENRUNG_PUBLIC_HOST:-}"
        if [ -z "$PUBLIC_HOST" ]; then
            PUBLIC_HOST="$(detect_public_ipv4)" || PUBLIC_HOST=""
            [ -n "$PUBLIC_HOST" ] \
                || die "could not auto-detect a public IPv4 address; re-run with OPENRUNG_PUBLIC_HOST=<public IP or DNS name>"
        else
            case "$PUBLIC_HOST" in *[!A-Za-z0-9.:-]* | '') die "OPENRUNG_PUBLIC_HOST contains unexpected characters" ;; esac
        fi

        LABEL="${OPENRUNG_LABEL:-}"
        if [ -z "$LABEL" ]; then
            LABEL="$(random_label)"
            [ -n "$LABEL" ] || die "could not generate a relay name"
        fi

        umask 077
        install -d -m 700 "${ENV_FILE%/*}"
        # Minted once per host: the broker derives the stable relay ID from
        # this seed (spec openrung-relay-identity-v1), so the relay keeps one
        # identity across restarts and updates. The seed IS the relay's Ed25519
        # private key: it lives only in the root-owned 0600 env file and is
        # never echoed or traced.
        SEED="$(head -c 32 /dev/urandom | base64 | tr -d '\n')"
        [ -n "$SEED" ] || die "could not generate an identity seed"
        {
            printf 'OPENRUNG_BROKER_URL=%s\n' "$BROKER_URL"
            printf 'OPENRUNG_PUBLIC_HOST=%s\n' "$PUBLIC_HOST"
            printf 'OPENRUNG_IDENTITY_SEED=%s\n' "$SEED"
            printf 'OPENRUNG_LABEL=%s\n' "$LABEL"
            if [ -n "$OPERATOR_CONTACT" ]; then printf 'OPENRUNG_OPERATOR_CONTACT=%s\n' "$OPERATOR_CONTACT"; fi
            # The binary's default listen host is '::' (dual-stack, the fleet
            # posture). Pin the IPv4 wildcard only when the kernel has no IPv6
            # support at all, where a '::' bind cannot work.
            if [ ! -e /proc/net/if_inet6 ]; then printf 'OPENRUNG_LISTEN_HOST=0.0.0.0\n'; fi
        } >"$ENV_FILE.tmp"
        mv "$ENV_FILE.tmp" "$ENV_FILE"
        log "wrote $ENV_FILE (root-owned, mode 0600) with public host ${PUBLIC_HOST} and relay name '${LABEL}'"
        if [ -n "$OPERATOR_CONTACT" ]; then
            log "operator contact '${OPERATOR_CONTACT}' is sent privately to the broker and never published"
        fi
    fi

    # Updates are a recoverable transaction. Host networking prevents the old
    # and new relay from listening on 443 simultaneously, so retain the exact
    # previous container under OLD_CONTAINER, stop it, and verify a separately
    # named candidate. The exit/signal trap restores the old container on every
    # ordinary failure; only a verified candidate is promoted to the canonical
    # name, and the backup is removed last.
    assert_volunteer_env
    HAD_PREVIOUS=$LIVE_EXISTS
    if [ "$HAD_PREVIOUS" = 1 ]; then
        foundation_container_marker="$(container_foundation_marker "$CONTAINER")" \
            || die "could not recheck '$CONTAINER' for Foundation ownership"
        [ -z "$foundation_container_marker" ] \
            || die "existing '$CONTAINER' became Foundation-managed during preflight; use deploy/relay/foundation-up.sh update"
        OLD_ID="$(container_id "$CONTAINER")" \
            || die "could not capture the identity of the existing '$CONTAINER' container"
        [ -n "$OLD_ID" ] || die "existing '$CONTAINER' returned an empty container ID"
    fi
    TRANSACTION_ACTIVE=1
    trap 'transaction_exit "$?"' 0
    trap 'exit 1' HUP INT TERM

    if [ "$HAD_PREVIOUS" = 1 ]; then
        docker rename "$CONTAINER" "$OLD_CONTAINER" >/dev/null \
            || die "could not preserve the previous relay as '$OLD_CONTAINER'"
        [ "$(container_id "$OLD_CONTAINER")" = "$OLD_ID" ] \
            || die "previous relay changed identity while it was being preserved"
        docker stop "$OLD_CONTAINER" >/dev/null \
            || die "could not stop the previous relay; automatic rollback will restore it"
    fi

    # Exact Foundation fleet container posture (foundation-up.sh run_candidate /
    # lightsail-up.sh user-data): drop every capability, re-add only
    # NET_BIND_SERVICE so the binary's cap_net_bind_service file capability can
    # bind the privileged public port 443 — deliberately NO
    # --security-opt no-new-privileges, which would make the kernel ignore file
    # capabilities and break that bind. Read-only rootfs; the only writable
    # path the relay needs is the generated xray config under the /tmp tmpfs.
    # Create and capture the candidate ID before starting it. Unlike inferring
    # ownership from a name after `docker run` fails, this cannot claim or
    # remove a container concurrently created by another process.
    if ! CANDIDATE_ID="$(docker create --name "$NEW_CONTAINER" --restart unless-stopped \
        --network host --cap-drop ALL --cap-add NET_BIND_SERVICE --read-only --tmpfs /tmp \
        --label org.openrung.managed-by=volunteer-up \
        --env-file "$ENV_FILE" \
        "$IMAGE")"; then
        CANDIDATE_ID=""
        die "the relay candidate could not be created; automatic rollback will restore the previous relay"
    fi
    [ -n "$CANDIDATE_ID" ] || die "relay candidate returned an empty container ID"
    [ "$(container_id "$NEW_CONTAINER")" = "$CANDIDATE_ID" ] \
        || die "relay candidate name does not resolve to the container this transaction created"
    docker start "$CANDIDATE_ID" >/dev/null \
        || die "the relay candidate failed to start; automatic rollback will restore the previous relay"

    CANDIDATE_BLOCKED=0
    if verify_candidate "$NEW_CONTAINER"; then
        :
    else
        [ "$?" -eq 2 ] && CANDIDATE_BLOCKED=1 \
            || die "the relay candidate failed verification; automatic rollback will restore the previous relay"
    fi

    # Recheck the exact objects and health immediately before committing. These
    # guards prevent a concurrent operator action from making the rollback
    # delete or rename a container other than the ones this transaction owns.
    [ "$(container_id "$NEW_CONTAINER")" = "$CANDIDATE_ID" ] \
        || die "relay candidate changed identity before commit"
    [ "$(container_inspect -f '{{.State.Running}} {{.RestartCount}}' "$NEW_CONTAINER" 2>/dev/null)" = "true 0" ] \
        || die "relay candidate stopped or restarted before commit"
    docker logs "$NEW_CONTAINER" 2>&1 | grep -qE 'registered relay|public endpoint unreachable' \
        || die "relay candidate registration evidence disappeared before commit"
    if [ "$HAD_PREVIOUS" = 1 ]; then
        [ "$(container_id "$OLD_CONTAINER")" = "$OLD_ID" ] \
            || die "previous relay changed identity before commit"
        [ "$(container_inspect -f '{{.State.Running}}' "$OLD_CONTAINER" 2>/dev/null)" = false ] \
            || die "previous relay restarted unexpectedly before commit"
    fi
    if container_exists "$CONTAINER"; then
        die "canonical container name '$CONTAINER' reappeared during the update"
    fi

    docker rename "$NEW_CONTAINER" "$CONTAINER" >/dev/null \
        || die "could not promote the verified relay candidate"
    TRANSACTION_ACTIVE=0
    trap - 0 HUP INT TERM

    if [ "$HAD_PREVIOUS" = 1 ]; then
        docker rm "$OLD_CONTAINER" >/dev/null \
            || die "the verified relay is live, but the stopped backup '$OLD_CONTAINER' could not be removed; inspect and remove that backup before the next update"
    fi

    log "installed: $CONTAINER is the live volunteer-class OpenRung relay on this host"
    report_reachability "$CONTAINER"
    log "  logs:    docker logs -f $CONTAINER"
    log "  update:  re-run the same one-line command (identity in $ENV_FILE is preserved)"
    log "  remove:  docker rm -f $CONTAINER   # and delete $ENV_FILE to forget the relay identity"
    final_label="$(sed -n 's/^OPENRUNG_LABEL=//p' "$ENV_FILE" | tail -1)" || final_label=""
    # Quoted heredoc: the art's backslashes and backtick stay literal. figlet
    # "standard", 47 columns — fits any 80-column terminal.
    cat <<'ART'

  ___  _ __   ___ _ __  _ __ _   _ _ __   __ _
 / _ \| '_ \ / _ \ '_ \| '__| | | | '_ \ / _` |
| (_) | |_) |  __/ | | | |  | |_| | | | | (_| |
 \___/| .__/ \___|_| |_|_|   \__,_|_| |_|\__, |
      |_|                                |___/

ART
    # Bold only when stdout is a terminal, so piped output stays plain text.
    if [ -t 1 ]; then
        thanks_bold=$(printf '\033[1m')
        thanks_reset=$(printf '\033[0m')
    else
        thanks_bold=''
        thanks_reset=''
    fi
    if [ -n "$final_label" ]; then
        printf "%sthank you for running '%s', together we will make the internet open again%s\n" \
            "$thanks_bold" "$final_label" "$thanks_reset"
    else
        printf '%sthank you for volunteering, together we will make the internet open again%s\n' \
            "$thanks_bold" "$thanks_reset"
    fi
}

main "$@"
