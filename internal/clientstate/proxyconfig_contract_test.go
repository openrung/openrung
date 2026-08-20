package clientstate_test

import (
	"testing"

	"github.com/openrung/openrung/connectcore/proxyconfig"

	"openrung/internal/clientstate"
)

// The store is the hosts' concrete PortStore/HelperStore behind the engine's
// stable proxy endpoint. proxyconfig moved into the connectcore module
// (ADR-001 D3) and can no longer name this store in its own tests, so the
// satisfaction check lives on this side of the module boundary instead.
var (
	_ proxyconfig.PortStore   = (*clientstate.Store)(nil)
	_ proxyconfig.HelperStore = (*clientstate.Store)(nil)
)

// TestStoreBacksProxyPortResolution round-trips the resolution path the
// engine actually runs — resolve once, persist, resolve again from a second
// store over the same directory — so the store's semantics keep matching what
// proxyconfig's own tests assert against their file-backed stand-in.
func TestStoreBacksProxyPortResolution(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(proxyconfig.PortEnv, "")

	first, err := proxyconfig.ResolvePort(clientstate.NewInDir(dir))
	if err != nil || first.PersistenceWarning != nil {
		t.Fatalf("first ResolvePort = %+v, %v", first, err)
	}
	second, err := proxyconfig.ResolvePort(clientstate.NewInDir(dir))
	if err != nil || second.Port != first.Port || second.PersistenceWarning != nil {
		t.Fatalf("second ResolvePort = %+v, %v; want persisted port %d", second, err, first.Port)
	}
}
