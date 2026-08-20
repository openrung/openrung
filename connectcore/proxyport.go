package connectcore

import (
	"fmt"
	"net"
	"strconv"

	"github.com/openrung/openrung/connectcore/proxyconfig"
)

// The proxyconfig package owns the endpoint and its resolution policy; the engine
// adds only when to check the port is free and how long a resolution sticks.

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

// LocalProxyPort pins only a successfully resolved endpoint for this process,
// so a transient failure (an unusable override, a failed allocation) stays
// retryable on the next call. A nil Persistence means no configuration
// directory: a fresh port plus a non-fatal warning that it may change.
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

// portStore must return a nil interface, never a nil pointer inside one, which
// proxyconfig's nil check cannot see through.
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
