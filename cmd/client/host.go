package main

import (
	"errors"

	"openrung/internal/clientstate"
	"openrung/internal/connectcore"
	"openrung/internal/proxyconfig"
	"openrung/internal/proxymode"
)

// engineHost bundles the connection engine with the CLI host wiring the TUI
// and the headless connect driver share: internal/clientstate persistence
// (recents, crash-recovery proxy snapshot, and the stable proxy port — the
// same on-disk state the desktop app reads), per-OS system proxy control, and
// the stable proxy-port resolver (OPENRUNG_PROXY_PORT honored, not persisted).
type engineHost struct {
	engine *connectcore.Engine
	store  *clientstate.Store // nil when the config directory is unavailable
}

func newEngineHost(sink connectcore.EventSink, singBoxPath string) *engineHost {
	host := &engineHost{engine: connectcore.New()}
	engine := host.engine
	engine.Sink = sink
	engine.SingBoxPath = singBoxPath
	engine.OSProxy = osProxyAdapter{ctrl: proxymode.New()}
	if store, err := clientstate.New(); err == nil {
		host.store = store
		engine.Persistence = storeAdapter{store: store}
	}
	engine.ResolveProxyPort = func() (connectcore.ProxyPortResolution, error) {
		resolution, err := proxyconfig.ResolvePort(host.store)
		if err != nil {
			return connectcore.ProxyPortResolution{}, err
		}
		return connectcore.ProxyPortResolution{
			Port:               resolution.Port,
			PersistenceWarning: resolution.PersistenceWarning,
		}, nil
	}
	return host
}

// The adapters below mirror desktop/vpnservice/adapters.go over the shared
// packages; the copies collapse when connectcore is promoted to a nested
// module (docs/adr/001 D3) and can own them.

// storeAdapter implements connectcore.Persistence over internal/clientstate.
// The engine treats the proxy snapshot as opaque; this adapter is where it
// regains its proxymode shape.
type storeAdapter struct{ store *clientstate.Store }

func (a storeAdapter) LoadRecents() []connectcore.RecentNode {
	stored := a.store.LoadRecents()
	out := make([]connectcore.RecentNode, 0, len(stored))
	for _, r := range stored {
		out = append(out, connectcore.RecentNode(r))
	}
	return out
}

func (a storeAdapter) SaveRecents(recents []connectcore.RecentNode) error {
	stored := make([]clientstate.RecentNode, 0, len(recents))
	for _, r := range recents {
		stored = append(stored, clientstate.RecentNode(r))
	}
	return a.store.SaveRecents(stored)
}

func (a storeAdapter) SaveProxySnapshot(snap connectcore.OSProxySnapshot) error {
	typed, ok := snap.(proxymode.Snapshot)
	if !ok {
		return errors.New("proxy snapshot is not a proxymode snapshot")
	}
	return a.store.SaveProxySnapshot(typed)
}

func (a storeAdapter) LoadProxySnapshot() (connectcore.OSProxySnapshot, bool) {
	snap, ok := a.store.LoadProxySnapshot()
	if !ok {
		return nil, false
	}
	return snap, true
}

func (a storeAdapter) ClearProxySnapshot() error {
	return a.store.ClearProxySnapshot()
}

// osProxyAdapter implements connectcore.OSProxy over the per-OS controllers in
// internal/proxymode.
type osProxyAdapter struct{ ctrl proxymode.Controller }

func (a osProxyAdapter) Supported() bool { return a.ctrl.Supported() }

func (a osProxyAdapter) Snapshot() (connectcore.OSProxySnapshot, error) {
	return a.ctrl.Snapshot()
}

func (a osProxyAdapter) Set(host string, port int) error {
	return a.ctrl.Set(host, port)
}

func (a osProxyAdapter) Restore(snap connectcore.OSProxySnapshot) error {
	typed, ok := snap.(proxymode.Snapshot)
	if !ok {
		return errors.New("proxy snapshot is not a proxymode snapshot")
	}
	return a.ctrl.Restore(typed)
}
