package vpnservice

import (
	"errors"

	"openrung/internal/clientstate"
	"openrung/internal/connectcore"
	"openrung/internal/proxyconfig"
	"openrung/internal/proxymode"
)

// storeAdapter implements connectcore.Persistence over internal/clientstate,
// keeping the on-disk formats (recents.json, proxy-snapshot.json) exactly as
// before the engine extraction. The engine treats the proxy snapshot as
// opaque; this adapter is where it regains its proxymode shape.
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
		return errors.New("proxy snapshot is not a desktop proxymode snapshot")
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

// helperStore must return a nil interface, never a nil pointer inside one, which
// proxyconfig's nil check cannot see through.
func (s *Service) helperStore() proxyconfig.HelperStore {
	if s.store == nil {
		return nil
	}
	return s.store
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
		return errors.New("proxy snapshot is not a desktop proxymode snapshot")
	}
	return a.ctrl.Restore(typed)
}
