// Package persist stores the volunteer app's per-install state: user settings
// (including the consent flag) and the relay's cryptographic identity. Files
// live under os.UserConfigDir()/openrung-volunteer — deliberately a different
// directory from the desktop client's "openrung", so the two apps share
// nothing on disk and a volunteer install is not linkable to a client install.
package persist

import (
	"encoding/json"
	"os"
	"path/filepath"

	"openrung/internal/volunteer/engine"
)

const (
	dirName      = "openrung-volunteer"
	settingsFile = "settings.json"
	identityFile = "identity.json"
)

// Settings is everything the user can configure, plus the consent flag. Zero
// values mean "use the app default"; Normalize resolves them.
type Settings struct {
	Label       string `json:"label"`
	MaxSessions int    `json:"maxSessions"`
	MaxMbps     int    `json:"maxMbps"`
	ListenPort  int    `json:"listenPort"`
	BrokerURL   string `json:"brokerUrl"`
	HubAddress  string `json:"hubAddress"`
	// ConnectionMode is "" / "automatic" (probe → direct or hub) or "direct"
	// (never use the hub — for publicly reachable machines that want to run
	// independently of the shared hub).
	ConnectionMode  string `json:"connectionMode"`
	ConsentAccepted bool   `json:"consentAccepted"`
}

// Store reads and writes the on-disk state. dir is a field so tests can point
// it at a temp directory.
type Store struct {
	dir string
}

// New resolves the per-install config directory, creating it on first use.
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

// Dir is the store's root directory; the app also writes the generated xray
// config here so the Reality private key never lands in a world-readable
// temp directory.
func (s *Store) Dir() string { return s.dir }

// LoadSettings returns the persisted settings; missing or corrupt files yield
// the zero value (callers normalize defaults).
func (s *Store) LoadSettings() Settings {
	var out Settings
	data, err := os.ReadFile(filepath.Join(s.dir, settingsFile))
	if err != nil {
		return out
	}
	_ = json.Unmarshal(data, &out)
	return out
}

func (s *Store) SaveSettings(settings Settings) error {
	return s.writeJSON(settingsFile, settings, 0o600)
}

// LoadIdentity returns the persisted relay identity; missing or corrupt files
// yield the zero value (the engine generates and reports a fresh one).
func (s *Store) LoadIdentity() engine.Identity {
	var out engine.Identity
	data, err := os.ReadFile(filepath.Join(s.dir, identityFile))
	if err != nil {
		return out
	}
	_ = json.Unmarshal(data, &out)
	return out
}

// SaveIdentity persists the relay identity (contains the Reality private key —
// 0600 like the xray config).
func (s *Store) SaveIdentity(id engine.Identity) error {
	return s.writeJSON(identityFile, id, 0o600)
}

// writeJSON writes atomically (temp file + rename) so a crash mid-write never
// leaves a truncated state file.
func (s *Store) writeJSON(name string, v any, mode os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, name+".tmp")
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, name))
}
