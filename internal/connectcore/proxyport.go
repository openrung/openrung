package connectcore

import (
	"errors"
	"fmt"
	"net"
	"strconv"
)

// The generic pieces of the stable local proxy endpoint, moved from
// desktop/proxyconfig (docs/adr/001 PR A1): the engine owns the loopback
// host, the override env name, the pre-ladder bind check, and the per-process
// pinning of a resolved port. Resolution itself (env override, persisted
// port, shell helper) stays behind ResolveProxyPort, in the shared
// internal/proxyconfig package (PR A3).

const (
	// ProxyHost is intentionally fixed to IPv4 loopback. The mixed HTTP/SOCKS
	// inbound has no authentication and must never become a LAN-facing proxy.
	ProxyHost = "127.0.0.1"
	// ProxyPortEnv is the supported process-level override for the stable port.
	ProxyPortEnv = "OPENRUNG_PROXY_PORT"
)

// ProxyPortResolution separates a usable process-local endpoint from a
// non-fatal persistence warning. Losing persistence must never prevent
// access, but the UI should not promise restart stability when saving failed.
type ProxyPortResolution struct {
	Port               int
	PersistenceWarning error
}

// EnsureProxyPortAvailable performs an early, actionable bind check before
// relay discovery. It deliberately does not choose another port: silently
// rotating a stable endpoint would break browser and shell configuration. As
// before, sing-box's later bind retains a small bind-and-close race window.
func EnsureProxyPortAvailable(port int) error {
	if !validPort(port) {
		return fmt.Errorf("proxy port %d is outside 1..65535", port)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(ProxyHost, strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("local proxy port %d is unavailable; set %s to another unused port: %w", port, ProxyPortEnv, err)
	}
	return listener.Close()
}

func validPort(port int) bool {
	return port >= 1 && port <= 65535
}

// LocalProxyPort resolves the stable endpoint through ResolveProxyPort,
// pinning only a successfully resolved endpoint for this process. A transient
// resolution failure remains retryable on the next call.
func (s *Engine) LocalProxyPort() (int, error) {
	s.proxyPortMu.Lock()
	defer s.proxyPortMu.Unlock()
	if s.proxyPort != 0 {
		return s.proxyPort, nil
	}
	if s.ResolveProxyPort == nil {
		return 0, errors.New("no local proxy port resolver configured")
	}
	resolution, err := s.ResolveProxyPort()
	if err != nil {
		return 0, err
	}
	s.proxyPort = resolution.Port
	s.proxyPortWarn = resolution.PersistenceWarning
	return s.proxyPort, nil
}

// LocalProxyPortWarning returns the pinned resolution's non-fatal persistence
// warning, if any.
func (s *Engine) LocalProxyPortWarning() error {
	s.proxyPortMu.Lock()
	defer s.proxyPortMu.Unlock()
	return s.proxyPortWarn
}
