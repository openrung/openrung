// Package persist stores small pieces of desktop client state on disk: the
// "recents" row and, while connected, the OS proxy snapshot used to recover
// after a crash. Files live under os.UserConfigDir()/openrung, next to the
// telemetry client-id, so all per-install state is in one place.
package persist

import (
	"encoding/json"
	"os"
	"path/filepath"

	"openrung/desktop/proxymode"
)

const (
	dirName          = "openrung"
	recentsFile      = "recents.json"
	proxySnapshotHdr = "proxy-snapshot.json"
)

// RecentNode mirrors the contract's RecentNode (openrung-mobile-app
// src/native/types.ts): a recently used exit location.
type RecentNode struct {
	CountryCode string  `json:"countryCode"`
	Label       string  `json:"label"`
	Latitude    float64 `json:"latitude"`
	Longitude   float64 `json:"longitude"`
}

// Store reads and writes the on-disk state. dir is resolved once; it is a field
// so tests can point it at a temp directory.
type Store struct {
	dir string
}

// New resolves the per-install config directory (os.UserConfigDir()/openrung),
// creating it on first use.
func New() (*Store, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(base, dirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// NewInDir builds a Store rooted at an explicit directory (tests).
func NewInDir(dir string) *Store {
	return &Store{dir: dir}
}

// LoadRecents returns the persisted recents (newest first), or an empty slice
// when none are stored or the file is unreadable/corrupt — recents are a
// convenience, never a hard dependency.
func (s *Store) LoadRecents() []RecentNode {
	data, err := os.ReadFile(filepath.Join(s.dir, recentsFile))
	if err != nil {
		return nil
	}
	var recents []RecentNode
	if err := json.Unmarshal(data, &recents); err != nil {
		return nil
	}
	return recents
}

// SaveRecents persists the recents list (best-effort).
func (s *Store) SaveRecents(recents []RecentNode) error {
	return s.writeJSON(recentsFile, recents)
}

// PrependRecent inserts node at the front, de-duplicated by countryCode, capped
// at max (matching the contract's cap-8 newest-first recents), and persists the
// result. It returns the new list so the caller can mirror it into state.
func PrependRecent(existing []RecentNode, node RecentNode, max int) []RecentNode {
	out := make([]RecentNode, 0, len(existing)+1)
	out = append(out, node)
	for _, r := range existing {
		if r.CountryCode == node.CountryCode {
			continue
		}
		out = append(out, r)
	}
	if len(out) > max {
		out = out[:max]
	}
	return out
}

// SaveProxySnapshot persists the OS proxy snapshot captured before a connect,
// so a crash mid-session can be cleaned up on the next launch.
func (s *Store) SaveProxySnapshot(snap proxymode.Snapshot) error {
	return s.writeJSON(proxySnapshotHdr, snap)
}

// LoadProxySnapshot returns the persisted snapshot and whether one existed. A
// present snapshot on startup means a prior session did not restore cleanly.
func (s *Store) LoadProxySnapshot() (proxymode.Snapshot, bool) {
	data, err := os.ReadFile(filepath.Join(s.dir, proxySnapshotHdr))
	if err != nil {
		return proxymode.Snapshot{}, false
	}
	var snap proxymode.Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return proxymode.Snapshot{}, false
	}
	return snap, true
}

// ClearProxySnapshot removes the snapshot after a clean restore.
func (s *Store) ClearProxySnapshot() error {
	err := os.Remove(filepath.Join(s.dir, proxySnapshotHdr))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *Store) writeJSON(name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	// Write-then-rename for atomicity, so a crash never leaves a half file.
	tmp := filepath.Join(s.dir, name+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, name))
}
