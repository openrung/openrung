// SPDX-License-Identifier: GPL-3.0-or-later

package connectcore

import "testing"

func TestSplitTunnelDefaultsOffAndShapesCandidateConfig(t *testing.T) {
	s := New()
	if s.SplitTunnel() {
		t.Fatal("split tunneling must default off for backward compatibility")
	}

	if err := s.SetSplitTunnel(true); err != nil {
		t.Fatalf("SetSplitTunnel: %v", err)
	}
	if !s.SplitTunnel() {
		t.Fatal("split tunneling did not turn on")
	}
	input := s.candidateConfigInput(usableRelay("split", "CN", "Shanghai", "China"), 46685)
	if !input.SplitTunnel {
		t.Fatal("candidate config lost the engine split-tunnel policy")
	}
	if input.ProxyListenPort != 46685 {
		t.Fatalf("candidate proxy port = %d, want 46685", input.ProxyListenPort)
	}
}

func TestSetSplitTunnelRefusedWhileConnectionIsLive(t *testing.T) {
	s := New()
	s.mu.Lock()
	s.conn = &connection{}
	s.mu.Unlock()

	if err := s.SetSplitTunnel(true); err == nil {
		t.Fatal("SetSplitTunnel succeeded during a live connection")
	}
	if s.SplitTunnel() {
		t.Fatal("refused SetSplitTunnel still changed the policy")
	}

	s.mu.Lock()
	s.conn = nil
	s.mu.Unlock()
	if err := s.SetSplitTunnel(true); err != nil {
		t.Fatalf("SetSplitTunnel after disconnect: %v", err)
	}
}
