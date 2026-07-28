//go:build windows

package relayruntime

import (
	"errors"

	"golang.org/x/sys/windows"
)

// Winsock returns its own error values rather than the portable syscall
// sentinels. In particular, syscall.Errno.Is(os.ErrPermission) does not map
// WSAEACCES, so classify the stable numeric errors without relying on localized
// bind error text.
func platformProbePermissionDenied(err error) bool {
	return errors.Is(err, windows.WSAEACCES)
}

func platformProbePortInUse(err error) bool {
	return errors.Is(err, windows.WSAEADDRINUSE)
}
