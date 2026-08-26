package clienttelemetry

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/openrung/openrung/connectcore/sudouser"
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

	if file, err := sudouser.OpenRegularFile(path, os.O_RDONLY, 0); err == nil {
		data, readErr := io.ReadAll(file)
		if id := strings.TrimSpace(string(data)); readErr == nil && id != "" {
			// A file left root-owned by an earlier sudo'd run would be
			// unreadable on the next plain run; repair it while we can.
			_ = sudouser.ChownFile(file)
			_ = file.Close()
			return id, nil
		}
		_ = file.Close()
	}

	id, err := newUUID()
	if err != nil {
		return "", err
	}
	if err := sudouser.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return id, nil
	}
	file, err := sudouser.OpenRegularFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return id, nil
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return id, nil
	}
	if _, err := file.Write([]byte(id + "\n")); err != nil {
		_ = file.Close()
		return id, nil
	}
	_ = sudouser.ChownFile(file)
	_ = file.Close()
	return id, nil
}
