package vpnservice

import (
	"testing"

	"openrung/internal/clientstate"
	"openrung/internal/connectcore"
)

// storeAdapter is the engine's whole storage hook — recents, the crash-recovery
// proxy snapshot, and the stable proxy port it resolves through proxyconfig —
// so the desktop app and the terminal client wire the same one thing.
var _ connectcore.Persistence = storeAdapter{}

// A missing configuration directory must reach proxyconfig as a nil interface.
// A nil *clientstate.Store wrapped in a non-nil interface would sail past
// proxyconfig's nil check and panic inside the shell helper instead of
// reporting that the directory is unavailable.
func TestHelperStoreIsNilWithoutAConfigurationDirectory(t *testing.T) {
	s := &Service{}
	if store := s.helperStore(); store != nil {
		t.Fatalf("helperStore() = %#v; want a nil interface", store)
	}

	s.store = clientstate.NewInDir(t.TempDir())
	if store := s.helperStore(); store == nil {
		t.Fatal("helperStore() = nil with a usable configuration directory")
	}
}
