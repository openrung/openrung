package main

import (
	"errors"
	"runtime"

	"openrung/internal/clientstate"
	"openrung/internal/connectcore"
	"openrung/internal/proxyconfig"
	"openrung/internal/proxymode"
)

// engineHost bundles the connection engine with the CLI host wiring the TUI
// and the headless connect driver share: internal/clientstate persistence
// (recents, crash-recovery proxy snapshot, and the stable proxy port — the
// same on-disk state the desktop app reads) and per-OS system proxy control.
type engineHost struct {
	engine *connectcore.Engine
	store  *clientstate.Store // nil when the config directory is unavailable
}

func newEngineHost(sink connectcore.EventSink, singBoxPath string) *engineHost {
	host := &engineHost{engine: connectcore.New()}
	engine := host.engine
	engine.Sink = sink
	engine.SingBoxPath = singBoxPath
	// The CLI identifies itself distinctly from the desktop GUI: telemetry
	// events carry platform="cli" so dashboards can tell the two apart on the
	// same OS (the broker stores unknown platform strings as ordinary
	// attributes and sends no platform header for them).
	engine.Platform = connectcore.PlatformCLI
	engine.OSProxy = osProxyAdapter{ctrl: proxymode.New()}
	// TUN mode is gated on this hook; proxy mode never invokes it.
	engine.Elevation = elevation{}
	if store, err := clientstate.New(); err == nil {
		host.store = store
		engine.Persistence = storeAdapter{store: store}
	}
	return host
}

// helperStore must return a nil interface, never a nil pointer inside one, which
// proxyconfig's nil check cannot see through.
func (h *engineHost) helperStore() proxyconfig.HelperStore {
	if h.store == nil {
		return nil
	}
	return h.store
}

// shellProxyHelper resolves the stable local endpoint and writes the
// sourceable shell helper, returning the copyable enable/restore commands —
// the same proxyconfig surface the desktop Settings screen exposes through
// GetProxyInfo. The engine call and the file write both happen here, so the
// TUI invokes it from a tea.Cmd, never from Update.
func (h *engineHost) shellProxyHelper() (proxyconfig.Info, error) {
	if runtime.GOOS == "windows" {
		return proxyconfig.Info{}, errors.New("shell integration is not available on Windows")
	}
	port, err := h.engine.LocalProxyPort()
	if err != nil {
		return proxyconfig.Info{}, err
	}
	return proxyconfig.WriteShellHelper(h.helperStore(), port)
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

func (a storeAdapter) LoadProxyPort() (int, bool) { return a.store.LoadProxyPort() }

func (a storeAdapter) LoadOrSaveProxyPort(candidate int) (int, error) {
	return a.store.LoadOrSaveProxyPort(candidate)
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
