//go:build !windows

package wsscore

import "syscall"

// The socket errnos the classifier matches. They are platform-specific because
// Go defines the E* names on Windows as its own invented values rather than the
// Winsock numbers the net stack actually returns there — see failure_windows.go
// and the same split in the standard library's net/error_unix.go and
// net/error_windows.go.
const (
	errnoConnectionRefused  = syscall.ECONNREFUSED
	errnoNetworkUnreachable = syscall.ENETUNREACH
	errnoHostUnreachable    = syscall.EHOSTUNREACH
	errnoConnectionReset    = syscall.ECONNRESET
	errnoBrokenPipe         = syscall.EPIPE
	// Redundant here (syscall.Errno.Timeout reports true for ETIMEDOUT, so the
	// generic timeout rule already catches it) but load-bearing on Windows.
	errnoTimedOut = syscall.ETIMEDOUT
)
