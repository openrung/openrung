# shellcheck shell=bash
#
# Shared relay-name generator for the provisioning helpers, sourced (not run).
#
# The provisioning scripts must name a cloud VM *before* any relay exists, so
# they cannot ask a relay what it is called — but they can read the same word
# lists the relay binary compiles in via go:embed
# (internal/relayruntime/label_{adjectives,nouns}.txt). Reading the canonical
# files is what keeps VM names and dashboard labels drawn from one vocabulary.
#
# volunteer-up.sh deliberately does NOT use this helper: it is curl-piped and
# has no repository checkout, so it asks the relay image for a name instead.
#
# Usage:
#   source "$(dirname "${BASH_SOURCE[0]}")/../lib/relay-label.sh"
#   NAME="${1:-$(openrung_random_label)}"

# Resolved once at source time so callers can cd freely afterwards.
_OPENRUNG_LABEL_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../internal/relayruntime" 2>/dev/null && pwd)"

# openrung_random_label prints one "adjective-noun" name.
openrung_random_label() {
    local adjectives=() nouns=() word
    local adjectives_file="${_OPENRUNG_LABEL_DIR}/label_adjectives.txt"
    local nouns_file="${_OPENRUNG_LABEL_DIR}/label_nouns.txt"

    if [[ ! -r "$adjectives_file" || ! -r "$nouns_file" ]]; then
        echo "relay-label: cannot read the label vocabulary under ${_OPENRUNG_LABEL_DIR:-<unresolved>};" \
             "run this script from a repository checkout, or pass an explicit name" >&2
        return 1
    fi

    # read-loop rather than mapfile: these scripts run on macOS too, where the
    # system bash is 3.2 and has no mapfile.
    while IFS= read -r word || [[ -n "$word" ]]; do
        [[ -n "$word" ]] && adjectives+=("$word")
    done <"$adjectives_file"
    while IFS= read -r word || [[ -n "$word" ]]; do
        [[ -n "$word" ]] && nouns+=("$word")
    done <"$nouns_file"

    if (( ${#adjectives[@]} == 0 || ${#nouns[@]} == 0 )); then
        echo "relay-label: label vocabulary under ${_OPENRUNG_LABEL_DIR} is empty" >&2
        return 1
    fi

    # $RANDOM is not crypto-grade and its modulo is slightly biased; names are
    # cosmetic, and the relay binary uses crypto/rand for its own naming.
    printf '%s-%s' \
        "${adjectives[RANDOM % ${#adjectives[@]}]}" \
        "${nouns[RANDOM % ${#nouns[@]}]}"
}
