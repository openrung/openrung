package engine

import (
	"encoding/json"
	"net"
	"testing"
)

type renderedXrayConfig struct {
	Inbounds []struct {
		Listen   string `json:"listen"`
		Port     int    `json:"port"`
		Protocol string `json:"protocol"`
	} `json:"inbounds"`
}

func renderAndParse(t *testing.T, eng *Engine) renderedXrayConfig {
	t.Helper()
	raw, err := eng.RenderXrayConfig()
	if err != nil {
		t.Fatalf("RenderXrayConfig: %v", err)
	}
	var parsed renderedXrayConfig
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("rendered config is not valid JSON: %v", err)
	}
	if len(parsed.Inbounds) != 1 || parsed.Inbounds[0].Protocol != "vless" {
		t.Fatalf("unexpected inbounds: %+v", parsed.Inbounds)
	}
	return parsed
}

func TestRenderXrayConfigDirect(t *testing.T) {
	identityReports := 0
	eng := New(Config{
		BrokerURL:  "http://127.0.0.1:1",
		Mode:       ModeDirect,
		ListenPort: 8443,
		Identity:   testIdentity,
	}, Events{OnIdentity: func(Identity) { identityReports++ }})

	parsed := renderAndParse(t, eng)
	if parsed.Inbounds[0].Listen != directOnlyListenHost || parsed.Inbounds[0].Port != 8443 {
		t.Fatalf("inbound = %s:%d, want %s:8443", parsed.Inbounds[0].Listen, parsed.Inbounds[0].Port, directOnlyListenHost)
	}
	// A complete identity renders as-is; nothing is generated or re-reported.
	if identityReports != 0 {
		t.Fatalf("OnIdentity fired %d times for a complete identity, want 0", identityReports)
	}
}

func TestRenderXrayConfigWSSPinsIPv4Wildcard(t *testing.T) {
	eng := New(Config{
		BrokerURL:       "https://broker.test",
		FoundationToken: "fnd-secret",
		Mode:            ModeDirect,
		WSSFronts:       wssTestFronts(),
		Identity:        testIdentity,
	}, Events{})

	parsed := renderAndParse(t, eng)
	if parsed.Inbounds[0].Listen != wssDirectListenHost || parsed.Inbounds[0].Port != 443 {
		t.Fatalf("inbound = %s:%d, want %s:443", parsed.Inbounds[0].Listen, parsed.Inbounds[0].Port, wssDirectListenHost)
	}
}

func TestRenderXrayConfigTunnelUsesLoopback(t *testing.T) {
	eng := New(Config{
		Mode:     ModeTunnel,
		HubAddr:  "hub.example:9443",
		Identity: testIdentity,
	}, Events{})

	parsed := renderAndParse(t, eng)
	ip := net.ParseIP(parsed.Inbounds[0].Listen)
	if ip == nil || !ip.IsLoopback() {
		t.Fatalf("tunnel render listen = %q, want a loopback address", parsed.Inbounds[0].Listen)
	}
	if parsed.Inbounds[0].Port <= 0 {
		t.Fatalf("tunnel render port = %d, want a reserved port", parsed.Inbounds[0].Port)
	}
}

func TestRenderXrayConfigValidatesFirst(t *testing.T) {
	eng := New(Config{Mode: ModeTunnel, Identity: testIdentity}, Events{}) // tunnel without a hub
	if _, err := eng.RenderXrayConfig(); err == nil {
		t.Fatal("RenderXrayConfig() error = nil, want a validation error")
	}
}
