#!/bin/sh
#
# Regression tests for volunteer-up.sh.
#
# The helper is deliberately a POSIX-sh, curl-to-sh entry point rather than a
# sourceable library.  These tests therefore exercise it as an operator would:
# under dash (when available), with a file-backed Docker stub.  A temporary
# copy changes only the two absolute /etc paths so the suite never needs root
# and never touches the host's real relay configuration.
set -u

HERE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
SCRIPT=$HERE/volunteer-up.sh

if [ -n "${OPENRUNG_TEST_SHELL:-}" ]; then
    TEST_SHELL=$OPENRUNG_TEST_SHELL
elif command -v dash >/dev/null 2>&1; then
    TEST_SHELL=$(command -v dash)
else
    TEST_SHELL=/bin/sh
fi
[ -x "$TEST_SHELL" ] || {
    echo "FAIL: requested test shell is not executable: $TEST_SHELL" >&2
    exit 1
}

TEST_TMP=$(mktemp -d "${TMPDIR:-/tmp}/openrung-volunteer-test.XXXXXX") || exit 1
trap 'rm -rf "$TEST_TMP"' 0
trap 'exit 1' HUP INT TERM

ENV_FILE=$TEST_TMP/relay.env
LEGACY_ENV_FILE=$TEST_TMP/volunteer.env
RUN_SCRIPT=$TEST_TMP/volunteer-up.sh
LIB_SCRIPT=$TEST_TMP/volunteer-up-lib.sh
FAKE_BIN=$TEST_TMP/bin
SIM_DIR=$TEST_TMP/docker
CONTAINERS=$SIM_DIR/containers
DOCKER_LOG=$TEST_TMP/docker.log
OUTPUT=$TEST_TMP/output

mkdir -p "$FAKE_BIN" "$CONTAINERS" || exit 1

# Keep this transformation intentionally narrow.  If the production helper's
# entry point or path assignments change, fail loudly instead of accidentally
# running an unmodified root-oriented script from the test suite.
if [ "$(tail -n 1 "$SCRIPT")" != 'main "$@"' ]; then
    echo "FAIL: volunteer-up.sh no longer ends with its guarded main entry point" >&2
    exit 1
fi

sed \
    -e "s|^    ENV_FILE=/etc/openrung/relay.env$|    ENV_FILE='$ENV_FILE'|" \
    -e "s|/etc/openrung/volunteer.env|$LEGACY_ENV_FILE|g" \
    "$SCRIPT" >"$RUN_SCRIPT" || exit 1
sed '$d' "$RUN_SCRIPT" >"$LIB_SCRIPT" || exit 1

cat >"$FAKE_BIN/id" <<'EOF'
#!/bin/sh
printf '0\n'
EOF

cat >"$FAKE_BIN/sleep" <<'EOF'
#!/bin/sh
# Registration timeout tests must not take a real minute.
exit 0
EOF

cat >"$FAKE_BIN/docker" <<'EOF'
#!/bin/sh
set -u

containers=$SIM_DIR/containers
log=$DOCKER_LOG
scenario=${SIM_SCENARIO:-ok}
op=${1:-}
[ "$#" -gt 0 ] && shift

last_arg() {
    last=
    for value do last=$value; done
    printf '%s' "$last"
}

read_state() {
    state_name=$1
    [ -f "$containers/$state_name" ] || return 1
    read -r state_running state_restarts state_registered state_generation state_id \
        <"$containers/$state_name"
}

resolve_name() {
    resolve_ref=$1
    if [ -f "$containers/$resolve_ref" ]; then
        printf '%s' "$resolve_ref"
        return 0
    fi
    for resolve_path in "$containers"/*; do
        [ -f "$resolve_path" ] || continue
        resolve_candidate=${resolve_path##*/}
        read_state "$resolve_candidate" || continue
        if [ "$state_id" = "$resolve_ref" ]; then
            printf '%s' "$resolve_candidate"
            return 0
        fi
    done
    return 1
}

write_state() {
    printf '%s %s %s %s %s\n' "$2" "$3" "$4" "$5" "$6" >"$containers/$1"
}

case "$op" in
    ps)
        for state_path in "$containers"/*; do
            [ -f "$state_path" ] || continue
            printf '%s\n' "${state_path##*/}"
        done
        ;;
    inspect)
        format=
        name=
        while [ "$#" -gt 0 ]; do
            case "$1" in
                -f | --format)
                    format=$2
                    shift 2
                    ;;
                *)
                    name=$1
                    shift
                    ;;
            esac
        done
        name=$(resolve_name "$name") || exit 1
        read_state "$name" || exit 1
        case "$format" in
            *com.docker.compose.project*)
                printf '\n'
                ;;
            *'{{.Id}}'*)
                printf '%s\n' "$state_id"
                ;;
            *State.Running*RestartCount*)
                printf '%s %s\n' "$state_running" "$state_restarts"
                ;;
            *RestartCount*)
                printf '%s\n' "$state_restarts"
                ;;
            *State.Running*)
                printf '%s\n' "$state_running"
                ;;
            *)
                printf '\n'
                ;;
        esac
        ;;
    pull)
        printf 'pull %s\n' "${1:-}" >>"$log"
        ;;
    stop)
        stop_ref=$(last_arg "$@")
        name=$(resolve_name "$stop_ref") || exit 1
        printf 'stop %s\n' "$stop_ref" >>"$log"
        read_state "$name" || exit 1
        write_state "$name" false "$state_restarts" "$state_registered" "$state_generation" "$state_id"
        ;;
    rename)
        source_ref=${1:-}
        target_name=${2:-}
        source_name=$(resolve_name "$source_ref") || exit 1
        printf 'rename %s %s\n' "$source_name" "$target_name" >>"$log"
        [ -f "$containers/$source_name" ] || exit 1
        [ ! -e "$containers/$target_name" ] || exit 1
        mv "$containers/$source_name" "$containers/$target_name"
        ;;
    create)
        name=
        while [ "$#" -gt 0 ]; do
            case "$1" in
                --name)
                    name=$2
                    shift 2
                    ;;
                --name=*)
                    name=${1#--name=}
                    shift
                    ;;
                *)
                    shift
                    ;;
            esac
        done
        printf 'create %s\n' "$name" >>"$log"
        [ -n "$name" ] || exit 2
        [ ! -e "$containers/$name" ] || exit 1
        if [ "$scenario" = create_fail ]; then
            # Model a concurrent name-conflict race: create returns no ID, so
            # the helper must not claim or remove the object that now owns the
            # requested name.
            write_state "$name" false 0 no alien id-alien
            exit 1
        fi
        case "$scenario" in
            crash_loop)
                write_state "$name" false 1 no candidate id-candidate
                ;;
            no_register)
                write_state "$name" false 0 no candidate id-candidate
                ;;
            *)
                write_state "$name" false 0 yes candidate id-candidate
                ;;
        esac
        printf 'id-candidate\n'
        ;;
    logs)
        logs_ref=$(last_arg "$@")
        name=$(resolve_name "$logs_ref") || exit 1
        read_state "$name" || exit 1
        if [ "$state_registered" = yes ]; then
            printf 'registered relay (simulated)\n'
        elif [ "$state_restarts" != 0 ]; then
            printf 'simulated relay crash\n'
        else
            printf 'simulated relay awaiting registration\n'
        fi
        ;;
    rm)
        status=0
        for rm_ref do
            case "$rm_ref" in -*) continue ;; esac
            name=$(resolve_name "$rm_ref") || {
                status=1
                continue
            }
            printf 'rm %s\n' "$name" >>"$log"
            if [ -e "$containers/$name" ]; then
                rm "$containers/$name"
            else
                status=1
            fi
        done
        exit "$status"
        ;;
    start)
        start_ref=$(last_arg "$@")
        name=$(resolve_name "$start_ref") || exit 1
        printf 'start %s\n' "$start_ref" >>"$log"
        read_state "$name" || exit 1
        if [ "$scenario" = start_fail ] && [ "$state_generation" = candidate ]; then
            exit 1
        fi
        write_state "$name" true "$state_restarts" "$state_registered" "$state_generation" "$state_id"
        printf '%s\n' "$start_ref"
        ;;
    *)
        echo "unexpected docker operation: $op $*" >&2
        exit 2
        ;;
esac
EOF

chmod +x "$FAKE_BIN/id" "$FAKE_BIN/sleep" "$FAKE_BIN/docker" || exit 1

PASS=0
FAIL=0

pass() {
    PASS=$((PASS + 1))
}

fail() {
    echo "FAIL: $*" >&2
    FAIL=$((FAIL + 1))
}

assert_nonzero() {
    if [ "$1" -ne 0 ]; then
        pass
    else
        fail "$2: expected failure, got success"
    fi
}

assert_log_has_create() {
    if grep -q '^create ' "$DOCKER_LOG"; then
        pass
    else
        fail "$1: candidate docker create was not exercised"
    fi
}

assert_no_mutating_docker_calls() {
    if grep -q \
        -e '^pull ' \
        -e '^stop ' \
        -e '^rename ' \
        -e '^create ' \
        -e '^rm ' \
        -e '^start ' \
        "$DOCKER_LOG"; then
        fail "$1: rejected configuration reached a Docker mutation"
    else
        pass
    fi
}

assert_prior_is_live() {
    if [ ! -f "$CONTAINERS/openrung-relay" ]; then
        fail "$1: openrung-relay was not restored"
        return
    fi
    read -r restored_running restored_restarts restored_registered restored_generation restored_id \
        <"$CONTAINERS/openrung-relay"
    if [ "$restored_running" = true ] \
        && [ "$restored_generation" = prior ] \
        && [ "$restored_id" = id-prior ] \
        && [ ! -e "$CONTAINERS/openrung-relay-old" ] \
        && [ ! -e "$CONTAINERS/openrung-relay-new" ]; then
        pass
    else
        fail "$1: live state is '$restored_running $restored_restarts $restored_registered $restored_generation $restored_id', expected only the running prior container under the canonical name"
    fi
}

assert_candidate_is_live() {
    if [ ! -f "$CONTAINERS/openrung-relay" ]; then
        fail "$1: promoted openrung-relay is missing"
        return
    fi
    read -r promoted_running promoted_restarts promoted_registered promoted_generation promoted_id \
        <"$CONTAINERS/openrung-relay"
    if [ "$promoted_running" = true ] \
        && [ "$promoted_restarts" = 0 ] \
        && [ "$promoted_registered" = yes ] \
        && [ "$promoted_generation" = candidate ] \
        && [ "$promoted_id" = id-candidate ] \
        && [ ! -e "$CONTAINERS/openrung-relay-old" ] \
        && [ ! -e "$CONTAINERS/openrung-relay-new" ]; then
        pass
    else
        fail "$1: live state is '$promoted_running $promoted_restarts $promoted_registered $promoted_generation $promoted_id', expected only the verified candidate under the canonical name"
    fi
}

assert_prior_and_unowned_candidate_are_preserved() {
    if [ ! -f "$CONTAINERS/openrung-relay" ] \
        || [ ! -f "$CONTAINERS/openrung-relay-new" ] \
        || [ -e "$CONTAINERS/openrung-relay-old" ]; then
        fail "$1: expected restored canonical relay plus untouched unowned name-conflict container"
        return
    fi
    read -r _ _ _ conflict_generation conflict_id \
        <"$CONTAINERS/openrung-relay-new"
    read -r restored_running restored_restarts restored_registered restored_generation restored_id \
        <"$CONTAINERS/openrung-relay"
    if [ "$restored_running" = true ] \
        && [ "$restored_generation" = prior ] \
        && [ "$restored_id" = id-prior ] \
        && [ "$conflict_generation" = alien ] \
        && [ "$conflict_id" = id-alien ]; then
        pass
    else
        fail "$1: create failure claimed or disturbed a container ID it did not create"
    fi
}

reset_simulation() {
    rm -rf "$CONTAINERS"
    mkdir -p "$CONTAINERS"
    printf 'true 0 yes prior id-prior\n' >"$CONTAINERS/openrung-relay"
    : >"$DOCKER_LOG"
    rm -f "$LEGACY_ENV_FILE"
}

write_env() {
    {
        printf 'OPENRUNG_BROKER_URL=https://broker-origin.openrung.org\n'
        printf 'OPENRUNG_PUBLIC_HOST=8.8.8.8\n'
        printf 'OPENRUNG_IDENTITY_SEED=test-identity-seed\n'
        while [ "$#" -gt 0 ]; do
            printf '%s\n' "$1"
            shift
        done
    } >"$ENV_FILE"
}

run_volunteer_up() {
    run_scenario=$1
    if PATH="$FAKE_BIN:$PATH" \
        SIM_DIR="$SIM_DIR" \
        DOCKER_LOG="$DOCKER_LOG" \
        SIM_SCENARIO="$run_scenario" \
        "$TEST_SHELL" "$RUN_SCRIPT" >"$OUTPUT" 2>&1; then
        RUN_RC=0
    else
        RUN_RC=$?
    fi
}

run_volunteer_up_first_run() {
    # First-run path: no env file, no prior container. OPENRUNG_NONINTERACTIVE
    # keeps the name prompt off even when the suite runs in a real terminal
    # (the prompt reads /dev/tty, which exists there), and OPENRUNG_PUBLIC_HOST
    # skips public-IP detection.
    run_scenario=$1
    shift
    if env "$@" \
        OPENRUNG_NONINTERACTIVE=1 \
        OPENRUNG_PUBLIC_HOST=8.8.8.8 \
        PATH="$FAKE_BIN:$PATH" \
        SIM_DIR="$SIM_DIR" \
        DOCKER_LOG="$DOCKER_LOG" \
        SIM_SCENARIO="$run_scenario" \
        "$TEST_SHELL" "$RUN_SCRIPT" >"$OUTPUT" 2>&1; then
        RUN_RC=0
    else
        RUN_RC=$?
    fi
}

run_volunteer_up_with_host_foundation_token() {
    run_scenario=$1
    if OPENRUNG_FOUNDATION_TOKEN=host-foundation-secret \
        PATH="$FAKE_BIN:$PATH" \
        SIM_DIR="$SIM_DIR" \
        DOCKER_LOG="$DOCKER_LOG" \
        SIM_SCENARIO="$run_scenario" \
        "$TEST_SHELL" "$RUN_SCRIPT" >"$OUTPUT" 2>&1; then
        RUN_RC=0
    else
        RUN_RC=$?
    fi
}

test_foundation_env_is_refused_before_mutation() {
    reset_simulation
    write_env 'OPENRUNG_FOUNDATION_TOKEN=foundation-secret'
    run_volunteer_up ok
    assert_nonzero "$RUN_RC" "Foundation-owned env"
    assert_no_mutating_docker_calls "Foundation-owned env"
    if grep -qi 'foundation' "$OUTPUT"; then
        pass
    else
        fail "Foundation-owned env: refusal does not explain Foundation ownership"
    fi
    assert_prior_is_live "Foundation-owned env"
}

test_foundation_token_duplicates_fail_closed() {
    # Docker --env-file gives the last duplicate to the container, but this
    # helper cannot safely infer volunteer ownership from a blank or ambiguous
    # Foundation assignment.  Treat every occurrence as a fail-closed
    # Foundation ownership marker, regardless of ordering.
    reset_simulation
    write_env \
        'OPENRUNG_FOUNDATION_TOKEN=' \
        'OPENRUNG_FOUNDATION_TOKEN=last-value-is-usable'
    run_volunteer_up create_fail
    assert_nonzero "$RUN_RC" "last non-empty Foundation token"
    assert_no_mutating_docker_calls "last non-empty Foundation token"
    assert_prior_is_live "last non-empty Foundation token"

    reset_simulation
    write_env \
        'OPENRUNG_FOUNDATION_TOKEN=shadowed-value' \
        'OPENRUNG_FOUNDATION_TOKEN='
    run_volunteer_up create_fail
    assert_nonzero "$RUN_RC" "last empty Foundation token"
    assert_no_mutating_docker_calls "last empty Foundation token"
    assert_prior_is_live "last empty Foundation token"
}

test_bare_foundation_marker_is_refused() {
    reset_simulation
    write_env 'OPENRUNG_FOUNDATION_TOKEN'
    run_volunteer_up_with_host_foundation_token create_fail
    assert_nonzero "$RUN_RC" "bare Foundation token marker"
    assert_no_mutating_docker_calls "bare Foundation token marker"
    if grep -qi 'foundation' "$OUTPUT"; then
        pass
    else
        fail "bare Foundation token marker: refusal does not explain Foundation ownership"
    fi
    assert_prior_is_live "bare Foundation token marker"
}

test_failed_candidate_create_preserves_unowned_conflict() {
    reset_simulation
    write_env
    run_volunteer_up create_fail
    assert_nonzero "$RUN_RC" "candidate create failure"
    assert_log_has_create "candidate create failure"
    assert_prior_and_unowned_candidate_are_preserved "candidate create failure"
    if grep -q '^rm openrung-relay-new$' "$DOCKER_LOG"; then
        fail "candidate create failure: removed the unowned name-conflict container"
    else
        pass
    fi
}

test_failed_candidate_start_restores_prior() {
    reset_simulation
    write_env
    run_volunteer_up start_fail
    assert_nonzero "$RUN_RC" "candidate start failure"
    assert_log_has_create "candidate start failure"
    if grep -q '^start id-candidate$' "$DOCKER_LOG"; then
        pass
    else
        fail "candidate start failure: candidate was not started by its exact create ID"
    fi
    assert_prior_is_live "candidate start failure"
}

test_crash_looping_candidate_restores_prior() {
    reset_simulation
    write_env
    run_volunteer_up crash_loop
    assert_nonzero "$RUN_RC" "candidate crash loop"
    assert_log_has_create "candidate crash loop"
    assert_prior_is_live "candidate crash loop"
}

test_unregistered_candidate_restores_prior() {
    reset_simulation
    write_env
    run_volunteer_up no_register
    assert_nonzero "$RUN_RC" "candidate registration timeout"
    assert_log_has_create "candidate registration timeout"
    assert_prior_is_live "candidate registration timeout"
}

test_successful_update_promotes_candidate() {
    reset_simulation
    write_env
    run_volunteer_up ok
    if [ "$RUN_RC" -eq 0 ]; then
        pass
    else
        fail "successful candidate update: expected success, got exit $RUN_RC"
    fi
    assert_log_has_create "successful candidate update"
    assert_candidate_is_live "successful candidate update"
    promotion_line=$(sed -n '/^rename openrung-relay-new openrung-relay$/=' "$DOCKER_LOG" | head -1)
    backup_rm_line=$(sed -n '/^rm openrung-relay-old$/=' "$DOCKER_LOG" | head -1)
    if [ -n "$promotion_line" ] \
        && [ -n "$backup_rm_line" ] \
        && [ "$promotion_line" -lt "$backup_rm_line" ]; then
        pass
    else
        fail "successful candidate update: candidate was not promoted before the old container was removed"
    fi
    # The suite's env files carry no label, so the closing line uses the
    # generic wording; either way, the banner shows and thanks come last.
    if tail -n 1 "$OUTPUT" | grep -q 'thank you for volunteering, together we will make the internet open again' \
        && grep -qF '| (_) | |_) |' "$OUTPUT"; then
        pass
    else
        fail "successful candidate update: banner or final thank-you line is wrong"
    fi
}

test_first_run_generates_label_and_promotes() {
    reset_simulation
    rm -f "$CONTAINERS/openrung-relay" "$ENV_FILE"
    run_volunteer_up_first_run ok
    if [ "$RUN_RC" -eq 0 ]; then
        pass
    else
        fail "first run: expected success, got exit $RUN_RC"
    fi
    if grep -Eq '^OPENRUNG_LABEL=[a-z]+-[a-z]+$' "$ENV_FILE" \
        && grep -q '^OPENRUNG_PUBLIC_HOST=8.8.8.8$' "$ENV_FILE" \
        && grep -Eq '^OPENRUNG_IDENTITY_SEED=..*' "$ENV_FILE"; then
        pass
    else
        fail "first run: env file is missing the generated label, public host, or identity seed"
    fi
    assert_candidate_is_live "first run"
    if grep -q '^stop ' "$DOCKER_LOG" || grep -q '^rm ' "$DOCKER_LOG"; then
        fail "first run: stopped or removed a container that did not exist"
    else
        pass
    fi
    first_run_label=$(sed -n 's/^OPENRUNG_LABEL=//p' "$ENV_FILE" | tail -1)
    if tail -n 1 "$OUTPUT" | grep -q "thank you for running '$first_run_label', together we will make the internet open again"; then
        pass
    else
        fail "first run: the final line does not thank the volunteer by relay name"
    fi
    # The openrung banner precedes the thank-you (fixed strings: the art is
    # full of regex metacharacters).
    if grep -qF '| (_) | |_) |' "$OUTPUT"; then
        pass
    else
        fail "first run: the openrung ASCII banner is missing"
    fi
}

test_first_run_honors_explicit_label() {
    reset_simulation
    rm -f "$CONTAINERS/openrung-relay" "$ENV_FILE"
    run_volunteer_up_first_run ok OPENRUNG_LABEL=my.relay_1
    if [ "$RUN_RC" -eq 0 ] && grep -q '^OPENRUNG_LABEL=my.relay_1$' "$ENV_FILE"; then
        pass
    else
        fail "explicit label: OPENRUNG_LABEL was not written verbatim (exit $RUN_RC)"
    fi
    if tail -n 1 "$OUTPUT" | grep -q "thank you for running 'my.relay_1', together we will make the internet open again"; then
        pass
    else
        fail "explicit label: the final line does not thank the volunteer by relay name"
    fi
}

expect_public_ipv4() {
    # The selected child shell, not this harness, must expand its positional
    # parameters after sourcing the helper library.
    # shellcheck disable=SC2016
    if "$TEST_SHELL" -c '. "$1"; is_public_ipv4 "$2"' sh "$LIB_SCRIPT" "$1" \
        >"$OUTPUT" 2>&1; then
        pass
    else
        fail "is_public_ipv4 rejected known public address $1"
    fi
}

expect_non_public_ipv4() {
    # shellcheck disable=SC2016
    if "$TEST_SHELL" -c '. "$1"; is_public_ipv4 "$2"' sh "$LIB_SCRIPT" "$1" \
        >"$OUTPUT" 2>&1; then
        fail "is_public_ipv4 accepted non-global address $1"
    else
        pass
    fi
}

test_special_ipv4_ranges() {
    expect_public_ipv4 1.1.1.1
    expect_public_ipv4 8.8.8.8

    # RFC 5737 documentation networks (TEST-NET-1/2/3).
    expect_non_public_ipv4 192.0.2.1
    expect_non_public_ipv4 198.51.100.1
    expect_non_public_ipv4 203.0.113.1

    # IETF protocol-assignment and deprecated 6to4-relay anycast space.
    expect_non_public_ipv4 192.0.0.1
    expect_non_public_ipv4 192.88.99.1

    # RFC 2544 benchmarking network (the full 198.18.0.0/15).
    expect_non_public_ipv4 198.18.0.1
    expect_non_public_ipv4 198.19.255.254
}

test_foundation_env_is_refused_before_mutation
test_foundation_token_duplicates_fail_closed
test_bare_foundation_marker_is_refused
test_failed_candidate_create_preserves_unowned_conflict
test_failed_candidate_start_restores_prior
test_crash_looping_candidate_restores_prior
test_unregistered_candidate_restores_prior
test_successful_update_promotes_candidate
test_first_run_generates_label_and_promotes
test_first_run_honors_explicit_label
test_special_ipv4_ranges

if [ "$FAIL" -ne 0 ]; then
    echo "volunteer-up tests: $PASS passed, $FAIL failed" >&2
    exit 1
fi

echo "volunteer-up tests: $PASS passed"
