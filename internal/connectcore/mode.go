package connectcore

import (
	"context"
	"errors"

	"openrung/internal/client"
)

// Capture mode (docs/adr/001 PR B3). The engine's default is the
// zero-privilege proxy mode the desktop app has always run; TUN mode drives
// the same ladder, telemetry, and recovery machinery through
// client.ModeTUN's full-device inbound instead.
//
// Mode is deliberately its own type rather than client.InboundMode: that
// type's zero value is ModeTUN, so an Engine literal would silently claim to
// need root. Here the zero value is ModeProxy, matching every host that
// predates this field.
type Mode int

const (
	// ModeProxy runs a loopback mixed HTTP/SOCKS inbound and points the OS
	// proxy at it. No privileges, no routing changes, and only proxy-aware
	// traffic is carried.
	ModeProxy Mode = iota
	// ModeTUN captures the whole device through a TUN interface. It needs the
	// privileges the Elevation hook checks for, takes over the default route,
	// and leaves no OS proxy behind.
	ModeTUN
)

func (m Mode) String() string {
	if m == ModeTUN {
		return "tun"
	}
	return "proxy"
}

// inboundMode maps to the sing-box config builder's own mode selector.
func (m Mode) inboundMode() client.InboundMode {
	if m == ModeTUN {
		return client.ModeTUN
	}
	return client.ModeProxy
}

// Mode returns the capture mode subsequent connects will use.
func (s *Engine) Mode() Mode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}

// SetMode selects the capture mode for subsequent connects. It refuses while a
// connection is live: the inbound shape, the OS-proxy policy, the readiness
// and health probes, and the elevation requirement all follow from the mode, so
// a session keeps the mode it started with. connectMu is held so the check
// cannot land in ConnectTarget's teardown-then-install window, where s.conn is
// momentarily nil even though a connect is already under way.
func (s *Engine) SetMode(mode Mode) error {
	s.connectMu.Lock()
	defer s.connectMu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		return errors.New("disconnect before changing the capture mode")
	}
	s.mode = mode
	return nil
}

// tunMode reports whether the engine is running full-device TUN capture. Every
// mode-dependent branch in the connect flow reads it, which is safe mid-session
// because SetMode cannot land while a connection is live.
func (s *Engine) tunMode() bool { return s.Mode() == ModeTUN }

// prepare is the OS-consent step: proxy mode needs none, while TUN mode must
// hold the privileges to create the tunnel device before anything is dialed.
// The Elevation hook owns the platform mechanics (and the guidance text a
// refusal carries), so the engine performs no ad-hoc privilege checks.
func (s *Engine) prepare(ctx context.Context) (bool, error) {
	if !s.tunMode() {
		return true, nil
	}
	if s.Elevation == nil {
		return false, errors.New("TUN mode is unavailable: this build has no elevation support")
	}
	if err := s.Elevation.Elevate(ctx); err != nil {
		return false, err
	}
	return true, nil
}
