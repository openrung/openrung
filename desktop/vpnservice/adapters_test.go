package vpnservice

import (
	"testing"

	"github.com/openrung/openrung/connectcore"
	"openrung/internal/clientstate"
)

var _ connectcore.Persistence = storeAdapter{}

// A missing configuration directory must reach proxyconfig as a nil interface,
// or its nil check is defeated and the shell helper panics instead of
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
