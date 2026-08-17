// SPDX-License-Identifier: GPL-3.0-or-later

package connectcore

import "errors"

// SplitTunnel reports whether subsequent connections will route private
// networks and the built-in mainland-China destination set directly. The
// proxy remains the final route for every destination outside that set.
func (s *Engine) SplitTunnel() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.splitTunnel
}

// SetSplitTunnel selects the routing policy for subsequent connections. A
// live session keeps the policy it started with; changing it requires a clean
// reconnect because every candidate and recovery config must agree.
func (s *Engine) SetSplitTunnel(enabled bool) error {
	s.connectMu.Lock()
	defer s.connectMu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		return errors.New("disconnect before changing split tunneling")
	}
	s.splitTunnel = enabled
	return nil
}
