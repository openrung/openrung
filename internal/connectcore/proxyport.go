package connectcore

import (
	"fmt"
	"net"
	"strconv"

	"openrung/internal/proxyconfig"
)

// The engine's use of the stable local proxy endpoint. internal/proxyconfig
// owns the endpoint itself — the loopback host, the OPENRUNG_PROXY_PORT
// override, the port-validity rule, and the resolution policy (env override,
// persisted port, fresh allocation). What lives here is the engine's own
// policy on top of it: when to check the port is free, and pinning a resolved
// endpoint for the process.

// Persistence carries proxyconfig's port-store shape, which is what lets
// LocalProxyPort hand the engine's own storage hook straight to the shared
// resolution policy instead of taking a callback from each host.
var _ proxyconfig.PortStore = Persistence(nil)

// EnsureProxyPortAvailable performs an early, actionable bind check before
// relay discovery. It deliberately does not choose another port: silently
// rotating a stable endpoint would break browser and shell configuration. As
// before, sing-box's later bind retains a small bind-and-close race window.
func EnsureProxyPortAvailable(port int) error {
	if !proxyconfig.ValidPort(port) {
		return fmt.Errorf("proxy port %d is outside 1..65535", port)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(proxyconfig.Host, strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("local proxy port %d is unavailable; set %s to another unused port: %w", port, proxyconfig.PortEnv, err)
	}
	return listener.Close()
}

// LocalProxyPort resolves the stable endpoint through proxyconfig, pinning
// only a successfully resolved endpoint for this process. A transient
// resolution failure (an unusable override, an allocation that failed) remains
// retryable on the next call.
//
// Persistence is the engine's one storage hook and satisfies
// proxyconfig.PortStore, so it is handed over as-is: a nil hook means no
// configuration directory, which resolves to a fresh port plus the non-fatal
// warning that it may change next launch.
func (s *Engine) LocalProxyPort() (int, error) {
	s.proxyPortMu.Lock()
	defer s.proxyPortMu.Unlock()
	if s.proxyPort != 0 {
		return s.proxyPort, nil
	}
	resolution, err := proxyconfig.ResolvePort(s.portStore())
	if err != nil {
		return 0, err
	}
	s.proxyPort = resolution.Port
	s.proxyPortWarn = resolution.PersistenceWarning
	return s.proxyPort, nil
}

// portStore narrows the persistence hook to the half proxyconfig needs. The
// explicit nil check keeps a nil Persistence a nil interface rather than a
// non-nil one holding a nil value, which proxyconfig's own nil check could not
// see through.
func (s *Engine) portStore() proxyconfig.PortStore {
	if s.Persistence == nil {
		return nil
	}
	return s.Persistence
}

// LocalProxyPortWarning returns the pinned resolution's non-fatal persistence
// warning, if any.
func (s *Engine) LocalProxyPortWarning() error {
	s.proxyPortMu.Lock()
	defer s.proxyPortMu.Unlock()
	return s.proxyPortWarn
}
