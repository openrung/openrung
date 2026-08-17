package wsscore

import "syscall"

// The Winsock error numbers OpenRung classifiers match, defined once for every
// consumer. Go's syscall package defines ECONNREFUSED and friends on Windows
// as invented APPLICATION_ERROR values, while the net package surfaces the raw
// Winsock numbers from connectex/WSARecv untranslated — so the raw numbers are
// what a classifier has to match, and they are spelled as literals because
// syscall exports only WSAECONNRESET by name, and only on Windows. The file is
// untagged: the numbers sit far above every errno a POSIX kernel returns
// (Linux tops out at 133, darwin at 106) and far below Go's invented Windows
// values, so they cannot collide with anything, and defining them everywhere
// is what lets CI exercise the Windows mappings on Linux and macOS.
//
// The table shares the numbers and their names, never token decisions. Each
// consumer — classifyDialFailure here, and the client taxonomy in
// openrung/internal/clienttelemetry — keeps its own errno→token mapping over
// these constants, and the two deliberately disagree: this taxonomy folds
// WSAECONNABORTED into its reset family and splits timeouts by handshake
// phase, while the client taxonomy leaves WSAECONNABORTED unmapped (pinned by
// the contract vector row errno_wsaeconnaborted) and emits one bare "timeout".
// A number can also be one consumer's alone: WSAEACCES feeds the client
// ladder's permission rung, and this taxonomy, which has no permission token,
// leaves it unclassified. TestWinsockTableDivergence in
// internal/clienttelemetry walks the table and keeps every such disagreement
// declared rather than accidental.
const (
	WSAEACCES       = syscall.Errno(10013)
	WSAENETUNREACH  = syscall.Errno(10051)
	WSAECONNABORTED = syscall.Errno(10053)
	WSAECONNRESET   = syscall.Errno(10054)
	WSAETIMEDOUT    = syscall.Errno(10060)
	WSAECONNREFUSED = syscall.Errno(10061)
	WSAEHOSTUNREACH = syscall.Errno(10065)
)

// WinsockErrnos returns the shared table keyed by Winsock symbol, for tests
// that walk it: a number added to the const block without a mapping decision
// in every consumer must fail a test there, not surface as a per-platform
// divergence in the field. TestWinsockErrnosCoversTheConstBlock keeps this map
// in lockstep with the constants.
func WinsockErrnos() map[string]syscall.Errno {
	return map[string]syscall.Errno{
		"WSAEACCES":       WSAEACCES,
		"WSAENETUNREACH":  WSAENETUNREACH,
		"WSAECONNABORTED": WSAECONNABORTED,
		"WSAECONNRESET":   WSAECONNRESET,
		"WSAETIMEDOUT":    WSAETIMEDOUT,
		"WSAECONNREFUSED": WSAECONNREFUSED,
		"WSAEHOSTUNREACH": WSAEHOSTUNREACH,
	}
}
