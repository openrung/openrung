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
	"syscall"
	"testing"

	"openrung/contract"
	"openrung/internal/client"
)

// classificationVectorsVersion is the version of
// contract/vectors/classification.json this suite was written against. The
// vectors are a cross-repo contract: a changed row has to reach the mobile
// repo's Kotlin, Swift, and TypeScript suites too, so the file's version must
// be bumped with it — and this constant with the file, which is what stops a
// row from being edited quietly on one side.
const classificationVectorsVersion = 1

// suiteName identifies this suite in a vector's "suites" list.
const suiteName = "go"

type classificationVectors struct {
	Version       int      `json:"version"`
	Tokens        []string `json:"tokens"`
	TokenPatterns []string `json:"token_patterns"`
	Kinds         map[string]struct {
		Suites []string `json:"suites"`
	} `json:"kinds"`
	Cases []classificationCase `json:"cases"`
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
	if err := contract.Load(contract.ClassificationVectors, &vectors); err != nil {
		t.Fatalf("load classification vectors: %v", err)
	}
	if vectors.Version != classificationVectorsVersion {
		t.Fatalf("classification vectors are version %d, this suite was written against %d — "+
			"bump classificationVectorsVersion together with the file, and re-vendor it in openrung-mobile-app",
			vectors.Version, classificationVectorsVersion)
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
