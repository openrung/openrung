package vpnservice

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"openrung/desktop/config"
	"openrung/desktop/discovery"
	"openrung/internal/relay"
)

// TestLivePunchCGNAT drives the real maybePunch path against the live broker and
// the cgnat-test relay's hub. Skipped by default (it needs network and a
// punchable NAT on both ends); run explicitly:
//
//	OPENRUNG_LIVE_PUNCH=1 go test ./vpnservice/ -run TestLivePunchCGNAT -v
func TestLivePunchCGNAT(t *testing.T) {
	if os.Getenv("OPENRUNG_LIVE_PUNCH") == "" {
		t.Skip("set OPENRUNG_LIVE_PUNCH=1 to run the live punch check")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fetch, err := discovery.FirstReachable(ctx, config.BrokerCandidates(""), discovery.Options{Limit: config.DirectoryRelayLimit})
	if err != nil {
		t.Fatalf("fetch relays: %v", err)
	}

	var target relay.Descriptor
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

	s := New() // PunchEnabled=true, PunchInsecure=true by default
	est := s.maybePunch(ctx, nil, target)
	if est == nil {
		t.Fatalf("punch did NOT establish (see log lines): %s", strings.Join(s.GetState().LogLines, " | "))
	}
	defer est.Close()

	t.Logf("PUNCH OK: bridge=%s:%d peer=%s nat=%s session=%s",
		est.BridgeHost, est.BridgePort, est.PeerIP, est.NATClass, est.SessionID)
	if est.BridgeHost == "" || est.BridgePort == 0 {
		t.Fatalf("punch established but bridge endpoint is empty: %+v", est)
	}
}
