//go:build windows

package relayruntime

import (
	"fmt"
	"testing"

	"golang.org/x/sys/windows"
)

func TestClassifyProbeBindErrorWindowsWinsockErrors(t *testing.T) {
	if got := classifyProbeBindError(fmt.Errorf("bind: %w", windows.WSAEACCES)); got != DirectProbePermissionDenied {
		t.Fatalf("WSAEACCES outcome = %q, want %q", got, DirectProbePermissionDenied)
	}
	if got := classifyProbeBindError(fmt.Errorf("bind: %w", windows.WSAEADDRINUSE)); got != DirectProbePortInUse {
		t.Fatalf("WSAEADDRINUSE outcome = %q, want %q", got, DirectProbePortInUse)
	}
}
