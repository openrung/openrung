package clienttelemetry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/openrung/openrung/wsscore"

	"openrung/contract"
	"openrung/internal/client"
)

// classificationVectorsVersion pins the version of
// contract/vectors/classification.json this suite expects. The vectors are a
// cross-repo contract: a changed row has to reach the mobile repo's Kotlin,
// Swift, and TypeScript suites too, so the file's version must be bumped with
// it — and this constant with the file, which is what stops a row from being
// edited quietly on one side. contract.LoadVersioned enforces the pin.
const classificationVectorsVersion = 1

// suiteName identifies this suite in a vector's "suites" list.
const suiteName = "go"

type classificationVectors struct {
	Suites        []string `json:"suites"`
	Tokens        []string `json:"tokens"`
	TokenPatterns []string `json:"token_patterns"`
	Kinds         map[string]struct {
		Suites []string `json:"suites"`
	} `json:"kinds"`
	Cases               []classificationCase `json:"cases"`
	ErrnoPairExceptions []struct {
		ID       string `json:"id"`
		DefersTo string `json:"defers_to"`
		Reason   string `json:"reason"`
	} `json:"errno_pair_exceptions"`
}

type classificationCase struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Platform string `json:"platform"`
	Input    struct {
		Wrapped  bool   `json:"wrapped"`
		Sentinel string `json:"sentinel"`
		Status   int    `json:"status"`
		Symbol   string `json:"symbol"`
		Code     int    `json:"code"`
		Subkind  string `json:"subkind"`
		Message  string `json:"message"`
		Reason   string `json:"reason"`
	} `json:"input"`
	Expect string   `json:"expect"`
	Suites []string `json:"suites"`
	Pair   string   `json:"pair"`
}

// runsHere reports whether this suite must run the case: the kind's suite list,
// narrowed by the case's own list when it has one.
func (v classificationVectors) runsHere(testCase classificationCase) bool {
	if len(testCase.Suites) > 0 {
		return slices.Contains(testCase.Suites, suiteName)
	}
	return slices.Contains(v.Kinds[testCase.Kind].Suites, suiteName)
}

func loadClassificationVectors(t *testing.T) classificationVectors {
	t.Helper()
	var vectors classificationVectors
	if err := contract.LoadVersioned(contract.ClassificationVectors, classificationVectorsVersion, &vectors); err != nil {
		t.Fatalf("load classification vectors: %v", err)
	}
	return vectors
}

// TestClassificationVectors runs every row of the shared classification
// contract against ClassifyError. The platform "windows" rows carry raw Winsock
// numbers and run on every host: they cannot collide with a POSIX errno, so the
// Windows behavior is checked by CI on Linux and macOS rather than only where
// it ships.
func TestClassificationVectors(t *testing.T) {
	vectors := loadClassificationVectors(t)

	seen := make(map[string]struct{}, len(vectors.Cases))
	for _, testCase := range vectors.Cases {
		if _, duplicate := seen[testCase.ID]; duplicate {
			t.Fatalf("duplicate vector id %q", testCase.ID)
		}
		seen[testCase.ID] = struct{}{}

		if _, known := vectors.Kinds[testCase.Kind]; !known {
			t.Fatalf("vector %q has kind %q, which the file does not describe", testCase.ID, testCase.Kind)
		}
		if !vectors.runsHere(testCase) {
			continue
		}

		t.Run(testCase.ID, func(t *testing.T) {
			if testCase.Kind == "wss_reason" {
				// The WSS taxonomy is projected through the consumer-side
				// allowlist, not through an error: wsscore's DialError carries
				// no constructor that could carry an arbitrary token here.
				if got := wssFailureReason(testCase.Input.Reason); got != testCase.Expect {
					t.Fatalf("wssFailureReason(%q) = %q, want %q", testCase.Input.Reason, got, testCase.Expect)
				}
				return
			}
			err := vectorError(t, testCase)
			if got := ClassifyError(err); got != testCase.Expect {
				t.Fatalf("ClassifyError(%v) = %q, want %q", err, got, testCase.Expect)
			}
		})
	}
}

// TestClassificationVectorsCoverage checks the vectors themselves: every token
// of the closed enum is exercised by some row, and no row expects a token
// outside it. Together these make "the vectors cover the whole ladder" a
// machine check rather than a review promise.
func TestClassificationVectorsCoverage(t *testing.T) {
	vectors := loadClassificationVectors(t)

	patterns := make([]*regexp.Regexp, 0, len(vectors.TokenPatterns))
	for _, pattern := range vectors.TokenPatterns {
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			t.Fatalf("token pattern %q: %v", pattern, err)
		}
		patterns = append(patterns, compiled)
	}

	expected := make(map[string]struct{}, len(vectors.Cases))
	kindsUsed := make(map[string]struct{}, len(vectors.Kinds))
	for _, testCase := range vectors.Cases {
		expected[testCase.Expect] = struct{}{}
		kindsUsed[testCase.Kind] = struct{}{}

		if slices.Contains(vectors.Tokens, testCase.Expect) {
			continue
		}
		matched := slices.ContainsFunc(patterns, func(pattern *regexp.Regexp) bool {
			return pattern.MatchString(testCase.Expect)
		})
		if !matched {
			t.Errorf("vector %q expects %q, which is neither a listed token nor a listed token pattern",
				testCase.ID, testCase.Expect)
		}
	}

	for _, token := range vectors.Tokens {
		if _, covered := expected[token]; !covered {
			t.Errorf("token %q is in the closed enum but no vector produces it", token)
		}
	}
	for kind := range vectors.Kinds {
		if _, used := kindsUsed[kind]; !used {
			t.Errorf("kind %q is described but has no vector", kind)
		}
	}
}

// TestWSSReasonAllowlistMatchesVectors ties the pass-through allowlist to the
// vectors from both sides, breaking the circle the token universe used to live
// in (four hand-synced copies validating each other): (a) the vectors'
// pass-through wss_reason rows are exactly the allowlist, so extending the
// allowlist means minting a vector row every suite sees; (b) every allowlist
// member is in the vectors' closed token list, so the dashboard enum cannot
// lag it. A pass-through row is one whose input reason equals its expectation;
// "wss_transport_failed" is the degrade target and can never be a pass-through
// claim, whatever a row's input says.
func TestWSSReasonAllowlistMatchesVectors(t *testing.T) {
	vectors := loadClassificationVectors(t)

	passThrough := make(map[string]string, len(vectors.Cases))
	for _, testCase := range vectors.Cases {
		if testCase.Kind != "wss_reason" || testCase.Input.Reason != testCase.Expect ||
			testCase.Expect == "wss_transport_failed" {
			continue
		}
		passThrough[testCase.Expect] = testCase.ID
	}

	allowed := make(map[string]bool, len(wssReasonAllowlist))
	for _, token := range wssReasonAllowlist {
		if allowed[token] {
			t.Errorf("the allowlist lists %q twice", token)
		}
		allowed[token] = true
		if _, covered := passThrough[token]; !covered {
			t.Errorf("the allowlist passes %q through but no wss_reason vector row pins it — add the row (and re-vendor it) or drop the token", token)
		}
		if !slices.Contains(vectors.Tokens, token) {
			t.Errorf("the allowlist passes %q through but the vectors' closed token list does not contain it", token)
		}
	}
	for token, id := range passThrough {
		if !allowed[token] {
			t.Errorf("vector row %q pins %q as a pass-through, but the allowlist does not contain it", id, token)
		}
	}
}

// TestWSSReasonAllowlistCoversWsscore walks wsscore's machine-readable token
// registry and requires a decision for every token: pass it through (the
// allowlist, which the test above ties to a vector row) or exclude it by name
// with a reason in wssReasonExcluded. A wsscore token in neither fails, so a
// token added or renamed there must produce a vector row or a named exclusion
// before it ships — the consumer-side decision point the frozen-set contract
// requires. The reverse direction keeps the allowlist honest: a member wsscore
// no longer emits is a stale decision to delete, not to carry.
func TestWSSReasonAllowlistCoversWsscore(t *testing.T) {
	registry := wsscore.Reasons()
	for _, token := range registry {
		allowed := slices.Contains(wssReasonAllowlist, token)
		_, excluded := wssReasonExcluded[token]
		switch {
		case allowed && excluded:
			t.Errorf("wsscore token %q is both allowlisted and excluded", token)
		case !allowed && !excluded:
			t.Errorf("wsscore token %q is neither in the allowlist nor in wssReasonExcluded — decide, and name the decision", token)
		}
	}
	for token, rationale := range wssReasonExcluded {
		if !slices.Contains(registry, token) {
			t.Errorf("wssReasonExcluded names %q (%s), which is not a wsscore token; delete the stale entry", token, rationale)
		}
		if strings.TrimSpace(rationale) == "" {
			t.Errorf("wssReasonExcluded entry %q carries no rationale", token)
		}
	}
	for _, token := range wssReasonAllowlist {
		if !slices.Contains(registry, token) {
			t.Errorf("the allowlist passes %q through, which wsscore's registry no longer contains; delete the stale entry", token)
		}
	}
}

// TestWinsockTableDivergence walks the shared Winsock errno table
// (wsscore/winsock.go) and checks, per code, that this ladder and wsscore's
// dial classifier assign the same token — or that the disagreement is declared
// below, naming the vector row that pins this ladder's side of it. The two
// taxonomies deliberately disagree in places (wsscore folds WSAECONNABORTED
// into its reset family and splits timeouts by phase); this test keeps each
// disagreement a stated fact with a frozen vector behind it rather than an
// accident either side could drift into.
func TestWinsockTableDivergence(t *testing.T) {
	vectors := loadClassificationVectors(t)
	rowsByID := make(map[string]classificationCase, len(vectors.Cases))
	for _, testCase := range vectors.Cases {
		rowsByID[testCase.ID] = testCase
	}

	// The declared divergences: what each side reports, and the vector row
	// that pins this ladder's side.
	divergences := map[string]struct {
		client, wss, pinnedBy string
	}{
		"WSAEACCES": {
			// The WSS taxonomy has no permission token.
			client:   "permission_denied",
			wss:      wsscore.ReasonUnclassified,
			pinnedBy: "errno_wsaeacces",
		},
		"WSAETIMEDOUT": {
			// wsscore splits timeouts by handshake phase; this ladder emits
			// one bare token.
			client:   "timeout",
			wss:      wsscore.ReasonTCPTimeout,
			pinnedBy: "errno_wsaetimedout",
		},
		"WSAECONNABORTED": {
			// 10053: deliberately unmapped here, together with EPIPE, its
			// POSIX twin; wsscore folds it into the reset family.
			client:   "unknown",
			wss:      wsscore.ReasonConnectionReset,
			pinnedBy: "errno_wsaeconnaborted",
		},
	}

	table := wsscore.WinsockErrnos()
	for symbol, errno := range table {
		clientToken := ClassifyError(errno)
		wssToken := wsscore.SocketErrnoReason(errno)
		divergence, declared := divergences[symbol]
		if !declared {
			if clientToken != wssToken {
				t.Errorf("%s: ClassifyError reports %q but wsscore reports %q — declare the divergence here with the vector row that pins it, or align the mapping",
					symbol, clientToken, wssToken)
			}
			continue
		}
		if clientToken != divergence.client {
			t.Errorf("%s: ClassifyError reports %q, but the declared divergence says %q", symbol, clientToken, divergence.client)
		}
		if wssToken != divergence.wss {
			t.Errorf("%s: wsscore reports %q, but the declared divergence says %q", symbol, wssToken, divergence.wss)
		}
		row, pinned := rowsByID[divergence.pinnedBy]
		switch {
		case !pinned:
			t.Errorf("%s: the divergence claims vector row %q pins it, but no such row exists", symbol, divergence.pinnedBy)
		case row.Kind != "errno" || row.Platform != "windows" || syscall.Errno(row.Input.Code) != errno:
			t.Errorf("%s: pinning row %q is not the windows errno row for code %d", symbol, divergence.pinnedBy, uint32(errno))
		case row.Expect != divergence.client:
			t.Errorf("%s: pinning row %q expects %q, not the declared client token %q", symbol, divergence.pinnedBy, row.Expect, divergence.client)
		}
	}
	for symbol := range divergences {
		if _, exists := table[symbol]; !exists {
			t.Errorf("a divergence is declared for %s, which is not in the shared table", symbol)
		}
	}
}

// TestClassificationVectorsSuiteDeclaration checks that the suites this file
// says consume it are exactly the suites its rows are claimed by.
//
// A suite named in the header but claimed by no row is the failure mode worth
// guarding: someone stands up that suite, it iterates the rows, finds none
// naming it, asserts nothing, and reports green forever — a vacuous pass that
// looks like coverage. The reverse, a row claimed by an undeclared suite, means
// the header undercounts its consumers and a change would not be routed to all
// of them.
func TestClassificationVectorsSuiteDeclaration(t *testing.T) {
	vectors := loadClassificationVectors(t)
	if len(vectors.Suites) == 0 {
		t.Fatal("the file declares no consuming suites")
	}

	claimed := make(map[string]int, len(vectors.Suites))
	claim := func(suites []string) {
		for _, suite := range suites {
			claimed[suite]++
		}
	}
	for _, testCase := range vectors.Cases {
		if len(testCase.Suites) > 0 {
			claim(testCase.Suites)
			continue
		}
		claim(vectors.Kinds[testCase.Kind].Suites)
	}

	for _, suite := range vectors.Suites {
		if claimed[suite] == 0 {
			t.Errorf("suite %q is declared as a consumer but no row is claimed by it — it would iterate this file, "+
				"assert nothing, and pass vacuously; either give it rows or drop it from \"suites\"", suite)
		}
	}
	for suite := range claimed {
		if !slices.Contains(vectors.Suites, suite) {
			t.Errorf("rows are claimed by suite %q, which is not declared in \"suites\"", suite)
		}
	}
	if !slices.Contains(vectors.Suites, suiteName) {
		t.Errorf("this suite (%q) runs these vectors but is not declared in \"suites\"", suiteName)
	}
}

// TestClassificationVectorsErrnoPairing checks that the two platforms report
// the same token for the same condition. A Winsock code is easy to add without
// its POSIX counterpart — or, as WSAEACCES was, to leave out entirely while its
// POSIX twin is mapped — and either way the divergence is invisible until a
// Windows user's failures land in the wrong dashboard bucket. Machine-checking
// the pairing is what keeps that from depending on review.
func TestClassificationVectorsErrnoPairing(t *testing.T) {
	vectors := loadClassificationVectors(t)

	byID := make(map[string]classificationCase, len(vectors.Cases))
	for _, testCase := range vectors.Cases {
		byID[testCase.ID] = testCase
	}

	tokensByPlatform := map[string]map[string]struct{}{
		"posix":   {},
		"windows": {},
	}
	for _, testCase := range vectors.Cases {
		if testCase.Kind != "errno" {
			continue
		}
		if _, known := tokensByPlatform[testCase.Platform]; !known {
			t.Errorf("errno vector %q has platform %q, want posix or windows", testCase.ID, testCase.Platform)
			continue
		}
		tokensByPlatform[testCase.Platform][testCase.Expect] = struct{}{}

		pair, found := byID[testCase.Pair]
		switch {
		case testCase.Pair == "":
			t.Errorf("errno vector %q names no cross-platform pair", testCase.ID)
		case !found:
			t.Errorf("errno vector %q pairs with %q, which does not exist", testCase.ID, testCase.Pair)
		case pair.Kind != "errno" || pair.Platform == testCase.Platform:
			t.Errorf("errno vector %q pairs with %q, which is not an errno row on the other platform", testCase.ID, testCase.Pair)
		case pair.Expect != testCase.Expect:
			t.Errorf("errno vector %q expects %q but its pair %q expects %q — the same condition must report one token on both platforms",
				testCase.ID, testCase.Expect, testCase.Pair, pair.Expect)
		case pair.Pair != testCase.ID && !declaredException(vectors, testCase.ID, testCase.Pair):
			// Pairing is one-to-one by default. Left unrestricted, a new row
			// could point at a pair already spoken for and satisfy both this
			// check and the token-set check below while its own counterpart
			// went unmapped — the token is already covered by the pair it
			// borrowed. A collapse has to be declared to be allowed.
			t.Errorf("errno vector %q pairs with %q, which pairs back with %q instead — declare the collapse in "+
				"errno_pair_exceptions with a reason, or give %q its own counterpart",
				testCase.ID, testCase.Pair, pair.Pair, testCase.ID)
		}
	}

	for _, exception := range vectors.ErrnoPairExceptions {
		switch {
		case byID[exception.ID].Kind != "errno":
			t.Errorf("errno_pair_exceptions names %q, which is not an errno vector", exception.ID)
		case byID[exception.ID].Pair != byID[exception.DefersTo].Pair:
			t.Errorf("errno_pair_exceptions says %q defers to %q, but they do not share a counterpart (%q vs %q)",
				exception.ID, exception.DefersTo, byID[exception.ID].Pair, byID[exception.DefersTo].Pair)
		case strings.TrimSpace(exception.Reason) == "":
			t.Errorf("errno_pair_exceptions entry %q carries no reason; a collapse without one is indistinguishable "+
				"from a forgotten counterpart", exception.ID)
		}
	}

	for token := range tokensByPlatform["posix"] {
		if _, covered := tokensByPlatform["windows"][token]; !covered {
			t.Errorf("a POSIX errno reports %q but no Winsock code does; either map its counterpart or pin the gap with a row", token)
		}
	}
	for token := range tokensByPlatform["windows"] {
		if _, covered := tokensByPlatform["posix"][token]; !covered {
			t.Errorf("a Winsock code reports %q but no POSIX errno does", token)
		}
	}
}

// declaredException reports whether the file declares that caseID deliberately
// shares another row's cross-platform counterpart.
func declaredException(vectors classificationVectors, caseID, pairID string) bool {
	for _, exception := range vectors.ErrnoPairExceptions {
		if exception.ID == caseID && byIDPair(vectors, exception.DefersTo) == pairID {
			return true
		}
	}
	return false
}

func byIDPair(vectors classificationVectors, caseID string) string {
	for _, testCase := range vectors.Cases {
		if testCase.ID == caseID {
			return testCase.Pair
		}
	}
	return ""
}

// vectorError builds the platform-specific error one vector describes. Each
// suite implements this differently — that is the point of describing inputs
// abstractly — so the mapping here is the Go half of the contract.
func vectorError(t *testing.T, testCase classificationCase) error {
	t.Helper()
	input := testCase.Input

	wrap := func(err error) error {
		if input.Wrapped {
			return fmt.Errorf("connect relay: %w", err)
		}
		return err
	}

	switch testCase.Kind {
	case "none":
		return nil
	case "cancellation":
		return wrap(context.Canceled)
	case "deadline":
		return wrap(context.DeadlineExceeded)
	case "selection":
		return wrap(selectionSentinel(t, input.Sentinel))
	case "http_status":
		return wrap(statusStub{code: input.Status})
	case "errno":
		errno := vectorErrno(t, testCase)
		if input.Wrapped {
			// The shape a failed dial actually produces, so the vector proves
			// the whole chain is walked and not just the outermost error.
			return &net.OpError{
				Op:  "dial",
				Net: "tcp",
				Err: &os.SyscallError{Syscall: "connect", Err: errno},
			}
		}
		return errno
	case "dns":
		switch input.Subkind {
		case "not_found":
			return wrap(&net.DNSError{Err: "no such host", Name: "broker.openrung.org", IsNotFound: true})
		case "timeout":
			return wrap(&net.DNSError{Err: "i/o timeout", Name: "broker.openrung.org", IsTimeout: true})
		}
	case "tls":
		switch input.Subkind {
		case "not_tls":
			return wrap(tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"})
		case "unknown_authority":
			return wrap(x509.UnknownAuthorityError{})
		case "hostname_mismatch":
			return wrap(x509.HostnameError{Host: "broker.openrung.org"})
		case "cert_expired":
			return wrap(x509.CertificateInvalidError{Reason: x509.Expired})
		}
	case "permission":
		if input.Subkind == "os_permission" {
			return wrap(os.ErrPermission)
		}
	case "process_exit":
		return wrap(exitError(t))
	case "timeout":
		if input.Subkind == "io_timeout" {
			return wrap(timeoutStub{})
		}
	case "unrecognized":
		return errors.New(input.Message)
	}

	t.Fatalf("vector %q: unsupported kind %q / subkind %q", testCase.ID, testCase.Kind, input.Subkind)
	return nil
}

func selectionSentinel(t *testing.T, sentinel string) error {
	t.Helper()
	switch sentinel {
	case "no_relays_available":
		return client.ErrNoRelaysAvailable
	case "relay_not_in_list":
		return client.ErrRelayNotInList
	case "no_relay_in_country":
		return client.ErrNoRelayInCountry
	case "no_usable_relay":
		return client.ErrNoUsableRelay
	}
	t.Fatalf("unknown selection sentinel %q", sentinel)
	return nil
}

// vectorErrno resolves one errno row. POSIX rows name a symbol, because the
// number differs per kernel; Windows rows carry the Winsock number itself,
// because Go's syscall package does not export most of them off Windows and
// the raw number is what the net stack surfaces there anyway.
func vectorErrno(t *testing.T, testCase classificationCase) syscall.Errno {
	t.Helper()
	if testCase.Platform == "windows" {
		if testCase.Input.Code == 0 {
			t.Fatalf("vector %q is a windows row but carries no errno code", testCase.ID)
		}
		return syscall.Errno(testCase.Input.Code)
	}
	switch testCase.Input.Symbol {
	case "ECONNREFUSED":
		return syscall.ECONNREFUSED
	case "ECONNRESET":
		return syscall.ECONNRESET
	case "ENETUNREACH":
		return syscall.ENETUNREACH
	case "EHOSTUNREACH":
		return syscall.EHOSTUNREACH
	case "ETIMEDOUT":
		return syscall.ETIMEDOUT
	case "EACCES":
		return syscall.EACCES
	case "EPERM":
		return syscall.EPERM
	case "EPIPE":
		return syscall.EPIPE
	}
	t.Fatalf("vector %q: unknown errno symbol %q", testCase.ID, testCase.Input.Symbol)
	return 0
}
