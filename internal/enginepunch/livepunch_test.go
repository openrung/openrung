package enginepunch_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"
	"github.com/openrung/openrung/connectcore"
	"github.com/openrung/openrung/connectcore/discovery"

	"openrung/internal/enginepunch"
)

// TestLivePunchCGNAT drives the real connectcore.AttemptPunch path — with
// enginepunch.Establish, exactly as the desktop and CLI hosts wire it —
// against the live broker and the cgnat-test relay's hub. It lives beside the
// establisher wiring because that is exactly what it exercises (the
// connectcore module only carries the PunchEstablisher seam). Skipped by default (it needs
// network and a punchable NAT on both ends); run explicitly:
//
//	OPENRUNG_LIVE_PUNCH=1 go test ./internal/enginepunch/ -run TestLivePunchCGNAT -v
func TestLivePunchCGNAT(t *testing.T) {
	if os.Getenv("OPENRUNG_LIVE_PUNCH") == "" {
		t.Skip("set OPENRUNG_LIVE_PUNCH=1 to run the live punch check")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fetch, err := discovery.FirstReachable(ctx, brokerapi.BrokerCandidates(""), discovery.Options{Limit: connectcore.DirectoryRelayLimit})
	if err != nil {
		t.Fatalf("fetch relays: %v", err)
	}

	var target brokerapi.RelayDescriptor
	for _, r := range fetch.Response.Relays {
		if r.PunchCapable {
			target = r
			break
		}
	}
	if target.ID == "" {
		t.Fatal("no punch-capable relay advertised by the broker")
	}
	t.Logf("target relay %q (label=%q) punch_endpoint=%q city=%q",
		target.ID, target.Label, target.PunchEndpoint, target.City)

	var logs []string
	est := connectcore.AttemptPunch(ctx, nil, target, connectcore.PunchOptions{
		Enabled: true,
		// Live relay hubs serve the punch API with a self-signed cert, so this
		// check opts out of hub TLS verification the way the desktop app does.
		Insecure:  true,
		Log:       func(line string) { logs = append(logs, line) },
		Establish: enginepunch.Establish,
	})
	if est == nil {
		t.Fatalf("punch did NOT establish (see log lines): %s", strings.Join(logs, " | "))
	}
	defer est.Close()

	t.Logf("PUNCH OK: bridge=%s:%d peer=%s nat=%s session=%s",
		est.BridgeHost, est.BridgePort, est.PeerIP, est.NATClass, est.SessionID)
	if est.BridgeHost == "" || est.BridgePort == 0 {
		t.Fatalf("punch established but bridge endpoint is empty: %+v", est)
	}
}
