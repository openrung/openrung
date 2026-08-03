//go:build windows

package wsscore

import "syscall"

// Windows needs its own errno set. Go's syscall package defines ECONNREFUSED
// and friends here as invented APPLICATION_ERROR values, while the net package
// surfaces the raw Winsock numbers from connectex/WSARecv untranslated — so
// matching the E* names would compile cleanly and never match anything, and
// every socket failure would silently degrade to "unclassified". The standard
// library has the same split (net/error_windows.go tests
// syscall.WSAECONNRESET where net/error_unix.go tests syscall.ECONNRESET).
//
// syscall exports only WSAECONNRESET by name, so the rest are their documented
// Winsock values.
const (
	errnoConnectionRefused  = syscall.Errno(10061) // WSAECONNREFUSED
	errnoNetworkUnreachable = syscall.Errno(10051) // WSAENETUNREACH
	errnoHostUnreachable    = syscall.Errno(10065) // WSAEHOSTUNREACH
	errnoConnectionReset    = syscall.WSAECONNRESET
	// WSAECONNABORTED, the local stack aborting a connection, is the closest
	// analogue of a broken pipe: it is what a send raises after the connection
	// has gone away without a peer reset.
	errnoBrokenPipe = syscall.Errno(10053) // WSAECONNABORTED
	// Windows syscall.Errno.Timeout only reports true for the invented
	// EAGAIN/EWOULDBLOCK/ETIMEDOUT, so a real WSAETIMEDOUT needs naming here or
	// the generic timeout rule misses it.
	errnoTimedOut = syscall.Errno(10060) // WSAETIMEDOUT
)
