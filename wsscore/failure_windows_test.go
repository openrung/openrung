//go:build windows

package wsscore

import (
	"syscall"
	"testing"
)

// TestErrnoTableHoldsRawWinsockNumbers guards the reversion that
// TestSocketErrnoConstantsAreDistinct cannot catch: Go's portable syscall.E*
// names on Windows are invented APPLICATION_ERROR-range values, which are also
// non-zero and collision-free, so a table rewritten to use them would pass the
// distinctness check while never matching the raw Winsock numbers the net
// package actually surfaces. Pinning the documented Winsock values makes any
// Windows test run fail loudly instead. CI does not provide such a run, so the
// hardcoded literals in failure_windows.go remain the everyday defense; this
// test exists so a one-off developer run is enough to catch a regression.
func TestErrnoTableHoldsRawWinsockNumbers(t *testing.T) {
	for name, entry := range map[string]struct {
		got  syscall.Errno
		want syscall.Errno
	}{
		"connection refused (WSAECONNREFUSED)":   {errnoConnectionRefused, 10061},
		"network unreachable (WSAENETUNREACH)":   {errnoNetworkUnreachable, 10051},
		"host unreachable (WSAEHOSTUNREACH)":     {errnoHostUnreachable, 10065},
		"connection reset (WSAECONNRESET)":       {errnoConnectionReset, 10054},
		"broken pipe analogue (WSAECONNABORTED)": {errnoBrokenPipe, 10053},
		"timed out (WSAETIMEDOUT)":               {errnoTimedOut, 10060},
	} {
		if entry.got != entry.want {
			t.Errorf("%s errno = %d, want the raw Winsock number %d; the portable syscall.E* names are invented APPLICATION_ERROR values on Windows and never match what the net package returns", name, entry.got, entry.want)
		}
	}
}
