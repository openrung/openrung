//go:build darwin

package directsetup

import (
	"context"
	"strings"
	"testing"
)

func TestDarwinReportsSignedHelperPackagingBlocker(t *testing.T) {
	manager := NewManagerWithPlatform("darwin", darwinPlatform{})
	status := manager.Status(context.Background())
	if status.State != StateUnavailable || status.CanEnable {
		t.Fatalf("darwin status = %+v, want unavailable", status)
	}
	if status.Reason != ReasonReleaseSigningRequired ||
		!strings.Contains(status.Message, "SMAppService") ||
		!strings.Contains(status.Message, "ad-hoc signed") ||
		!strings.Contains(status.Message, "will not request sudo") {
		t.Fatalf("darwin blocker is incomplete: %+v", status)
	}
}
