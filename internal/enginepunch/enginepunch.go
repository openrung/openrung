// Package enginepunch wires internal/punch's QUIC punch transport into the
// connectcore engine's PunchEstablisher seam (ADR-001 D3): the engine module
// owns when to punch and how outcomes are reported, while the quic-go
// transport stays in this repository's root module. It is its own package —
// not part of internal/punch — so the relay-side binaries that share the
// punch transport never link the client engine.
package enginepunch

import (
	"context"

	"github.com/openrung/openrung/connectcore"
	"github.com/openrung/openrung/punchcore"

	"openrung/internal/punch"
)

// Establish adapts punch.Dialer.Establish to connectcore.PunchEstablisher.
// Hosts assign it to Engine.PunchEstablisher.
func Establish(ctx context.Context, hub punchcore.HubClient, relayID string) (*connectcore.PunchPath, punchcore.PunchResult, error) {
	dialer := &punch.Dialer{Hub: hub, RelayID: relayID}
	est, res, err := dialer.Establish(ctx)
	if err != nil {
		return nil, res, err
	}
	return &connectcore.PunchPath{
		BridgeHost: est.BridgeHost,
		BridgePort: est.BridgePort,
		PeerIP:     est.PeerIP,
		SessionID:  est.SessionID,
		NATClass:   est.NATClass,
		Bridge:     establishmentBridge{est: est},
	}, res, nil
}

// establishmentBridge exposes an Establishment as the engine's PunchBridge:
// Serve runs the loopback bridge, and Close preserves the pinned teardown —
// bridge first, then the punched socket.
type establishmentBridge struct{ est *punch.Establishment }

func (b establishmentBridge) Serve(ctx context.Context) error { return b.est.Bridge.Serve(ctx) }

func (b establishmentBridge) Close() error { return b.est.Close() }
