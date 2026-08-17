package wsscore

import (
	"syscall"
	"testing"
)

// TestWinsockTableIsRawAndDistinct guards the shared errno table against the
// two rewrites that would break it silently. Go's portable syscall.E* names
// hold real POSIX numbers on POSIX and invented APPLICATION_ERROR values on
// Windows, and neither ever matches what the Windows net stack surfaces — so a
// constant "cleaned up" to a portable name leaves the Winsock error range on
// every platform, and the range check catches on any CI host what previously
// needed a Windows test run (the old build-tagged table could only pin its raw
// numbers behind a windows tag). Collapsed or zeroed entries would make two
// tokens indistinguishable or match every non-syscall error.
func TestWinsockTableIsRawAndDistinct(t *testing.T) {
	table := WinsockErrnos()
	if len(table) == 0 {
		t.Fatal("the shared Winsock table is empty")
	}
	seen := make(map[syscall.Errno]string, len(table))
	for symbol, errno := range table {
		if errno < 10000 || errno > 11999 {
			t.Errorf("%s = %d, outside the Winsock error range 10000–11999; the portable syscall.E* names never match what the Windows net stack surfaces", symbol, errno)
		}
		if previous, duplicate := seen[errno]; duplicate {
			t.Errorf("%s and %s share errno %d, collapsing two distinct conditions", symbol, previous, errno)
		}
		seen[errno] = symbol
	}
}

// TestClassifyWinsockErrnos walks the shared table through SocketErrnoReason,
// pinning this taxonomy's token for every Winsock number on whatever host runs
// the suite. WSAEACCES is in the table for the client taxonomy's permission
// rung; this taxonomy has no permission token and deliberately leaves it
// unclassified.
func TestClassifyWinsockErrnos(t *testing.T) {
	want := map[string]string{
		"WSAEACCES":       ReasonUnclassified,
		"WSAENETUNREACH":  ReasonNetworkUnreachable,
		"WSAECONNABORTED": ReasonConnectionReset,
		"WSAECONNRESET":   ReasonConnectionReset,
		"WSAETIMEDOUT":    ReasonTCPTimeout,
		"WSAECONNREFUSED": ReasonConnectionRefused,
		"WSAEHOSTUNREACH": ReasonNetworkUnreachable,
	}
	table := WinsockErrnos()
	for symbol, errno := range table {
		wantReason, decided := want[symbol]
		if !decided {
			t.Errorf("%s is in the shared table but this test declares no token for it; decide its mapping here", symbol)
			continue
		}
		if got := SocketErrnoReason(errno); got != wantReason {
			t.Errorf("SocketErrnoReason(%s) = %q, want %q", symbol, got, wantReason)
		}
	}
	for symbol := range want {
		if _, exists := table[symbol]; !exists {
			t.Errorf("a token is declared for %s, which is not in the shared table", symbol)
		}
	}
}
