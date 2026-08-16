package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
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

// Concurrent renders on an idle engine must converge on ONE generated
// identity: prepareIdentity serializes generation and always starts from the
// freshest stored identity, so every returned config matches the identity a
// subsequent Start registers with.
func TestConcurrentRendersConvergeOnOneIdentity(t *testing.T) {
	// Reality keys present so generation never shells out to xray; the
	// missing client ID, short ID, and seed are what the racers would fork.
	partial := testIdentity
	partial.ClientID = ""
	partial.ShortID = ""
	partial.IdentitySeed = ""

	var mu sync.Mutex
	var persisted []Identity
	broker := &fakeBroker{}
	ts := httptest.NewServer(broker.handler())
	defer ts.Close()

	eng := New(Config{
		BrokerURL:   ts.URL,
		Mode:        ModeDirect,
		PublicHost:  "203.0.113.7",
		ListenPort:  freePort(t),
		Identity:    partial,
		DisableXray: true,
		ConfigDir:   t.TempDir(),
	}, Events{OnIdentity: func(id Identity) {
		mu.Lock()
		persisted = append(persisted, id)
		mu.Unlock()
	}})

	const renders = 16
	configs := make([][]byte, renders)
	errs := make([]error, renders)
	var wg sync.WaitGroup
	wg.Add(renders)
	for i := 0; i < renders; i++ {
		go func(i int) {
			defer wg.Done()
			configs[i], errs[i] = eng.RenderXrayConfig()
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("render %d: %v", i, err)
		}
		if !bytes.Equal(configs[i], configs[0]) {
			t.Fatalf("render %d produced a different config: one identity must win\n%s\nvs\n%s", i, configs[i], configs[0])
		}
	}
	mu.Lock()
	if len(persisted) != 1 {
		mu.Unlock()
		t.Fatalf("OnIdentity fired %d times across %d concurrent renders, want exactly 1", len(persisted), renders)
	}
	winner := persisted[0]
	mu.Unlock()

	// The identity a subsequent Start registers with is the rendered one.
	if err := eng.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer eng.Stop()
	eventually(t, 5*time.Second, "relay online", func() bool {
		return eng.Status().Phase == PhaseOnline
	})
	_, _, last := broker.stats()
	if last.ClientID != winner.ClientID || last.ShortID != winner.ShortID {
		t.Fatalf("registered identity (%s/%s) does not match the rendered one (%s/%s)",
			last.ClientID, last.ShortID, winner.ClientID, winner.ShortID)
	}
}

// UpdateConfig must wait for an in-flight identity preparation: without the
// shared lock, a render generating keys across an UpdateConfig would write
// its stale identity back over the just-installed configuration.
func TestUpdateConfigWaitsForInFlightIdentityGeneration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake xray requires a POSIX shell")
	}

	dir := t.TempDir()
	startedPath := filepath.Join(dir, "started")
	releasePath := filepath.Join(dir, "release")
	fakeXray := filepath.Join(dir, "xray")
	script := fmt.Sprintf(`#!/bin/sh
touch %q
while [ ! -e %q ]; do sleep 0.05; done
echo "Private key: generated-private-key"
echo "Public key: generated-public-key"
`, startedPath, releasePath)
	if err := os.WriteFile(fakeXray, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake xray: %v", err)
	}

	// Missing Reality keys force prepareIdentity through the fake xray, which
	// holds the identity lock until the release file appears.
	partial := testIdentity
	partial.RealityPrivateKey = ""
	partial.RealityPublicKey = ""
	eng := New(Config{
		BrokerURL: "http://127.0.0.1:1",
		Mode:      ModeDirect,
		XrayPath:  fakeXray,
		Identity:  partial,
	}, Events{})

	renderDone := make(chan error, 1)
	go func() {
		_, err := eng.RenderXrayConfig()
		renderDone <- err
	}()
	eventually(t, 5*time.Second, "identity generation in flight", func() bool {
		_, err := os.Stat(startedPath)
		return err == nil
	})

	updateDone := make(chan error, 1)
	go func() {
		updateDone <- eng.UpdateConfig(Config{
			BrokerURL: "http://127.0.0.1:1",
			Mode:      ModeDirect,
			XrayPath:  fakeXray,
			Identity:  testIdentity,
		})
	}()
	select {
	case err := <-updateDone:
		t.Fatalf("UpdateConfig returned (%v) while identity generation was still in flight", err)
	case <-time.After(200 * time.Millisecond):
	}

	if err := os.WriteFile(releasePath, []byte("go"), 0o600); err != nil {
		t.Fatalf("release fake xray: %v", err)
	}
	if err := <-renderDone; err != nil {
		t.Fatalf("render: %v", err)
	}
	if err := <-updateDone; err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}

	if got := eng.currentConfig().Identity; got != testIdentity {
		t.Fatalf("identity after UpdateConfig = %+v, want the installed testIdentity (a stale render writeback overwrote it)", got)
	}
}

// Rendering while running is refused: a live direct session rebinds xray to a
// runtime-reserved loopback port, so a mid-session render would not describe
// the running relay.
func TestRenderXrayConfigRefusesWhileRunning(t *testing.T) {
	_, addr := startTestHub(t)
	eng := New(Config{
		Mode:         ModeTunnel,
		HubAddr:      addr,
		HubPlaintext: true,
		Identity:     testIdentity,
		DisableXray:  true,
		ConfigDir:    t.TempDir(),
	}, Events{})
	if err := eng.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer eng.Stop()

	if _, err := eng.RenderXrayConfig(); err == nil {
		t.Fatal("RenderXrayConfig() error = nil, want a running-engine refusal")
	}

	eng.Stop()
	if _, err := eng.RenderXrayConfig(); err != nil {
		t.Fatalf("RenderXrayConfig after Stop: %v", err)
	}
}
