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
# Overridable via env — pass through sudo, e.g.
#   curl -fsSL .../volunteer-up.sh | sudo env OPENRUNG_PUBLIC_HOST=203.0.113.7 sh
#   OPENRUNG_IMAGE        image to run (default ghcr.io/openrung/openrung-relay:main)
#   OPENRUNG_BROKER_URL   broker to register with (default the broker's direct
#                         TLS origin — the CDN front challenges datacenter IPs)
#   OPENRUNG_PUBLIC_HOST  public IP/DNS clients use to reach this relay
#                         (skips auto-detection; required on the first run when
#                         detection is unavailable)
#   OPENRUNG_LABEL        optional friendly name shown in the broker dashboard
# Overrides configure the FIRST run; once /etc/openrung/relay.env exists it is
# authoritative and overrides are ignored (edit the file instead).
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

verify_candidate() {
    verify_container=$1
    log "waiting for the relay candidate to register with the broker"
    verify_i=0
    while :; do
        verify_state="$(container_inspect -f '{{.State.Running}} {{.RestartCount}}' "$verify_container" 2>/dev/null)" \
            || verify_state=""
        if [ "$verify_state" = "true 0" ] \
            && docker logs "$verify_container" 2>&1 | grep -q 'registered relay'; then
            log "$(docker logs "$verify_container" 2>&1 | grep 'registered relay' | tail -1)"
            return 0
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
        if [ "$verify_i" -ge 30 ]; then
            echo "volunteer-up: error: the relay candidate did not register within 60s; recent logs:" >&2
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
        case "$OPENRUNG_LABEL" in *[!A-Za-z0-9._-]*) die "OPENRUNG_LABEL may use only letters, digits, '.', '_', '-'" ;; esac
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

    if [ -f "$ENV_FILE" ]; then
        log "reusing existing $ENV_FILE (stable relay identity preserved)"
        if [ -n "${OPENRUNG_BROKER_URL:-}" ] || [ -n "${OPENRUNG_PUBLIC_HOST:-}" ] || [ -n "${OPENRUNG_LABEL:-}" ]; then
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

        umask 077
        install -d -m 700 /etc/openrung
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
            if [ -n "${OPENRUNG_LABEL:-}" ]; then printf 'OPENRUNG_LABEL=%s\n' "$OPENRUNG_LABEL"; fi
            # The binary's default listen host is '::' (dual-stack, the fleet
            # posture). Pin the IPv4 wildcard only when the kernel has no IPv6
            # support at all, where a '::' bind cannot work.
            if [ ! -e /proc/net/if_inet6 ]; then printf 'OPENRUNG_LISTEN_HOST=0.0.0.0\n'; fi
        } >"$ENV_FILE.tmp"
        mv "$ENV_FILE.tmp" "$ENV_FILE"
        log "wrote $ENV_FILE (root-owned, mode 0600) with public host ${PUBLIC_HOST}"
    fi

    log "pulling $IMAGE"
    docker pull "$IMAGE" >/dev/null

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

    verify_candidate "$NEW_CONTAINER" \
        || die "the relay candidate failed verification; automatic rollback will restore the previous relay"

    # Recheck the exact objects and health immediately before committing. These
    # guards prevent a concurrent operator action from making the rollback
    # delete or rename a container other than the ones this transaction owns.
    [ "$(container_id "$NEW_CONTAINER")" = "$CANDIDATE_ID" ] \
        || die "relay candidate changed identity before commit"
    [ "$(container_inspect -f '{{.State.Running}} {{.RestartCount}}' "$NEW_CONTAINER" 2>/dev/null)" = "true 0" ] \
        || die "relay candidate stopped or restarted before commit"
    docker logs "$NEW_CONTAINER" 2>&1 | grep -q 'registered relay' \
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

    log "done — this host is now serving as a volunteer-class OpenRung relay"
    log "  open inbound TCP 443 in your provider's firewall (and 'ufw allow 443/tcp' if you use ufw)"
    log "  logs:    docker logs -f $CONTAINER"
    log "  update:  re-run the same one-line command (identity in $ENV_FILE is preserved)"
    log "  remove:  docker rm -f $CONTAINER   # and delete $ENV_FILE to forget the relay identity"
}

main "$@"
