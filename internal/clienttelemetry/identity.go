package clienttelemetry

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	identityDirName  = "openrung"
	identityFileName = "client-id"
)

// clientIDPath resolves the persistent client-id file location. It is a package
// var so tests can point it at a temp directory.
var clientIDPath = func() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, identityDirName, identityFileName), nil
}

var identityMu sync.Mutex

// ClientID returns a stable per-installation identifier, mirroring the Android
// ClientIdentity.getOrCreate (which persists a UUID in SharedPreferences). It
// reads os.UserConfigDir()/openrung/client-id, creating it on first use. If the
// file cannot be resolved or written, it falls back to an ephemeral per-process
// UUID so telemetry never blocks connecting.
func ClientID() (string, error) {
	identityMu.Lock()
	defer identityMu.Unlock()

	path, err := clientIDPath()
	if err != nil {
		return newUUID()
	}

	if data, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id, nil
		}
	}

	id, err := newUUID()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return id, nil
	}
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return id, nil
	}
	return id, nil
}
