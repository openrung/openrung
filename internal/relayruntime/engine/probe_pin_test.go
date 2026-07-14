package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"openrung/internal/relayruntime"
	"openrung/internal/tunnel"
)

// TestProbePinnedSelfSignedHubSelectsDirect is the regression for the bug where
// probeClient ignored HubCertFingerprint: the reachability probe hits the hub's
// HTTPS API, so if it does not apply the same cert pin as the tunnel dial it
// fails verification against the hub's self-signed cert, and auto mode can never
// choose direct — every publicly reachable relay is forced through the hub.
//
// It stands up the real reachability prober behind a self-signed TLS server and
// asserts that with the matching pin the probe SUCCEEDS (reachable → direct
// mode), and that a wrong pin FAILS (→ tunnel fallback), proving the pin is both
// applied and enforced on the probe path.
func TestProbePinnedSelfSignedHubSelectsDirect(t *testing.T) {
	prober := tunnel.NewReachabilityProber("", slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	prober.Register(mux)
	ts := httptest.NewTLSServer(mux) // self-signed cert, like a real bare-IP hub
	defer ts.Close()

	// Fingerprint of the server's self-signed leaf — the DER the pin verifier
	// hashes.
	sum := sha256.Sum256(ts.Certificate().Raw)
	fp := hex.EncodeToString(sum[:])

	eng := New(Config{}, Events{})
	port := freePort(t)

	// Matching pin: the HTTPS probe validates, the hub dials the relay's
	// loopback listener back, and detection reports reachable → direct mode.
	reachable, host, err := relayruntime.DetectDirectReachable(
		context.Background(), ts.URL, "", "::", port, eng.probeClient(Config{HubCertFingerprint: fp}))
	if err != nil {
		t.Fatalf("probe under matching pin must not error (it forces tunnel fallback): %v", err)
	}
	if !reachable {
		t.Fatal("probe under matching pin must report reachable so auto mode selects direct")
	}
	if host != "127.0.0.1" {
		t.Fatalf("observed host = %q, want 127.0.0.1", host)
	}

	// Wrong pin: the probe must fail cert validation (→ inconclusive → tunnel),
	// proving the pin is genuinely enforced and not merely skipping verification.
	_, _, err = relayruntime.DetectDirectReachable(
		context.Background(), ts.URL, "", "::", freePort(t), eng.probeClient(Config{HubCertFingerprint: strings.Repeat("00", 32)}))
	if err == nil {
		t.Fatal("probe under a wrong pin must fail, not silently succeed")
	}
}

// TestProbeUnpinnedSelfSignedHubFails documents the pre-fix behaviour that
// caused the bug: with no pin, standard verification rejects the hub's
// self-signed cert, so the probe errors and auto mode falls back to tunnel.
func TestProbeUnpinnedSelfSignedHubFails(t *testing.T) {
	prober := tunnel.NewReachabilityProber("", slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	prober.Register(mux)
	ts := httptest.NewTLSServer(mux)
	defer ts.Close()

	eng := New(Config{}, Events{})
	_, _, err := relayruntime.DetectDirectReachable(
		context.Background(), ts.URL, "", "::", freePort(t), eng.probeClient(Config{}))
	if err == nil {
		t.Fatal("unpinned probe against a self-signed hub must fail standard verification")
	}
}
