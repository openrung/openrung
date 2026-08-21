package connectcore

import (
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/openrung/openrung/connectcore/proxyconfig"
)

func TestEnsureProxyPortAvailableReportsStablePortCollision(t *testing.T) {
	listener, err := net.Listen("tcp", proxyconfig.Host+":0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := EnsureProxyPortAvailable(port); err == nil || !strings.Contains(err.Error(), proxyconfig.PortEnv) {
		t.Fatalf("occupied port error = %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := EnsureProxyPortAvailable(port); err != nil {
		t.Fatalf("released port still unavailable: %v", err)
	}
}

// An override is this launch's choice, not the installation's: it wins over
// anything stored and is never written back.
func TestLocalProxyPortOverrideWinsAndIsNotPersisted(t *testing.T) {
	store := &fakePersistence{port: 51000}
	s := New()
	s.Persistence = store
	t.Setenv(proxyconfig.PortEnv, "46685")

	port, err := s.LocalProxyPort()
	if err != nil || port != 46685 {
		t.Fatalf("LocalProxyPort = %d, %v; want the override", port, err)
	}
	if _, saves := store.portCalls(); saves != 0 {
		t.Fatalf("override was persisted (%d saves)", saves)
	}
	if store.port != 51000 {
		t.Fatalf("override overwrote the stored port: %d", store.port)
	}
	if warning := s.LocalProxyPortWarning(); warning != nil {
		t.Fatalf("override reported a persistence warning: %v", warning)
	}
}

// Without an override the endpoint is chosen once and reused on later
// launches.
func TestLocalProxyPortAutomaticSelectionIsStableAcrossEngines(t *testing.T) {
	t.Setenv(proxyconfig.PortEnv, "")
	store := &fakePersistence{}

	first := New()
	first.Persistence = store
	allocated, err := first.LocalProxyPort()
	if err != nil {
		t.Fatalf("first resolution: %v", err)
	}
	if store.port != allocated {
		t.Fatalf("first launch persisted %d but returned %d", store.port, allocated)
	}
	if warning := first.LocalProxyPortWarning(); warning != nil {
		t.Fatalf("successful persistence still warned: %v", warning)
	}

	// A second engine over the same store stands in for the next launch.
	second := New()
	second.Persistence = store
	reused, err := second.LocalProxyPort()
	if err != nil || reused != allocated {
		t.Fatalf("next launch = %d, %v; want the persisted %d", reused, err, allocated)
	}
}

// Two simultaneous first launches must not end up on different endpoints: both
// allocate, and the one that loses the locked save adopts the winner's port.
func TestLocalProxyPortAdoptsAnotherProcessWinner(t *testing.T) {
	t.Setenv(proxyconfig.PortEnv, "")
	store := &fakePersistence{winner: 51234}

	s := New()
	s.Persistence = store
	port, err := s.LocalProxyPort()
	if err != nil {
		t.Fatalf("resolution: %v", err)
	}
	if port != 51234 {
		t.Fatalf("kept its own allocation %d instead of the store's winner 51234", port)
	}
	if warning := s.LocalProxyPortWarning(); warning != nil {
		t.Fatalf("losing the race warned: %v", warning)
	}
}

// Losing persistence must never deny access: the endpoint still resolves, stays
// put for this process, and reports a warning rather than an error.
func TestLocalProxyPortPersistenceFailureIsANonFatalWarning(t *testing.T) {
	t.Setenv(proxyconfig.PortEnv, "")

	for name, engine := range map[string]*Engine{
		"no configuration directory": func() *Engine {
			s := New() // nil Persistence
			return s
		}(),
		"unwritable store": func() *Engine {
			s := New()
			s.Persistence = &fakePersistence{saveErr: errors.New("read-only config directory")}
			return s
		}(),
	} {
		port, err := engine.LocalProxyPort()
		if err != nil {
			t.Fatalf("%s: resolution failed instead of warning: %v", name, err)
		}
		if !proxyconfig.ValidPort(port) {
			t.Fatalf("%s: unusable port %d", name, port)
		}
		warning := engine.LocalProxyPortWarning()
		if warning == nil || !strings.Contains(warning.Error(), "may change next launch") {
			t.Fatalf("%s: warning = %v", name, warning)
		}
		// Still pinned for this process, so the endpoint a user configured in a
		// browser keeps working for the life of the session.
		if again, _ := engine.LocalProxyPort(); again != port {
			t.Fatalf("%s: endpoint moved within the process: %d then %d", name, port, again)
		}
	}
}
