// Package clientstate stores small pieces of end-user client state on disk:
// the "recents" row, the stable local proxy endpoint, and, while connected, the
// OS proxy snapshot used to recover after a crash. Files live under
// os.UserConfigDir()/openrung, next to the telemetry client-id, so all
// per-install state is in one place. The desktop app and the TUI client share
// this directory, so the file names and formats here are a compatibility
// contract with state written by earlier builds.
package clientstate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/openrung/openrung/connectcore/sudouser"

	"openrung/internal/proxymode"
)

const (
	dirName           = "openrung"
	recentsFile       = "recents.json"
	proxyPortFile     = "proxy-port.json"
	proxyPortLockFile = "proxy-port.lock"
	proxyEnvFile      = "proxy-env-%d.sh"
	proxySnapshotHdr  = "proxy-snapshot.json"
)

// RecentNode mirrors the contract's RecentNode (openrung-mobile-app
// src/native/types.ts): a recently used exit location.
type RecentNode struct {
	CountryCode string  `json:"countryCode"`
	Label       string  `json:"label"`
	Latitude    float64 `json:"latitude"`
	Longitude   float64 `json:"longitude"`
}

type proxyEndpoint struct {
	Port int `json:"port"`
}

// Store reads and writes the on-disk state. dir is resolved once; it is a field
// so tests can point it at a temp directory.
type Store struct {
	dir string
	// stateDir pins the directory by descriptor for the process lifetime, and
	// is set only under sudo. Writes then go through *at syscalls, so the
	// directory validated in New cannot be swapped for a symlink afterwards.
	stateDir *sudouser.StateDir
}

// New resolves the per-install config directory (os.UserConfigDir()/openrung),
// creating it on first use.
func New() (*Store, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(base, dirName)
	if err := sudouser.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	store := &Store{dir: dir}
	if sudouser.Active() {
		// Refusing an unsafe state directory is fatal: a Store rooted at a
		// symlinked path has root writing the user's files wherever the link
		// points, which for a name like proxy-env-1080.sh and a target like
		// /etc/profile.d means a user-writable script that root later sources.
		stateDir, err := sudouser.OpenStateDir(dir)
		if err != nil {
			return nil, err
		}
		store.stateDir = stateDir
		// The ownership repair is the opposite case. An earlier sudo'd run
		// (TUN mode) may have left the directory or the files in it
		// root-owned, which makes every later plain run silently fail to read
		// or write them. A repair that cannot run costs one root-owned file,
		// while failing New costs the caller its whole store — recents, the
		// stable proxy port, the crash snapshot — so each step is best-effort.
		_ = stateDir.RepairOwner()
		if entries, err := stateDir.Entries(); err == nil {
			for _, entry := range entries {
				if repairableStateFile(entry.Name()) {
					_ = stateDir.RepairEntry(entry.Name())
				}
			}
		}
	}
	return store, nil
}

// NewInDir builds a Store rooted at an explicit directory (tests).
func NewInDir(dir string) *Store {
	return &Store{dir: dir}
}

// LoadRecents returns the persisted recents (newest first), or an empty slice
// when none are stored or the file is unreadable/corrupt — recents are a
// convenience, never a hard dependency.
func (s *Store) LoadRecents() []RecentNode {
	data, err := s.readFile(recentsFile)
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

// LoadProxyPort returns the stable per-install loopback proxy port. Missing,
// unreadable, corrupt, and out-of-range files are treated as absent so the
// caller can allocate and persist a fresh port.
func (s *Store) LoadProxyPort() (int, bool) {
	data, err := s.readFile(proxyPortFile)
	if err != nil {
		return 0, false
	}
	var endpoint proxyEndpoint
	if err := json.Unmarshal(data, &endpoint); err != nil || !validPort(endpoint.Port) {
		return 0, false
	}
	return endpoint.Port, true
}

// SaveProxyPort persists the stable per-install loopback proxy port.
func (s *Store) SaveProxyPort(port int) error {
	if !validPort(port) {
		return fmt.Errorf("proxy port %d is outside 1..65535", port)
	}
	return s.writeJSON(proxyPortFile, proxyEndpoint{Port: port})
}

// LoadOrSaveProxyPort serializes first-launch selection across client
// processes. If another process persisted a port while this caller was
// allocating candidate, the existing winner is returned.
func (s *Store) LoadOrSaveProxyPort(candidate int) (int, error) {
	if !validPort(candidate) {
		return 0, fmt.Errorf("proxy port %d is outside 1..65535", candidate)
	}
	var selected int
	err := withFileLock(filepath.Join(s.dir, proxyPortLockFile), func() error {
		if port, ok := s.LoadProxyPort(); ok {
			selected = port
			return nil
		}
		if err := s.SaveProxyPort(candidate); err != nil {
			return err
		}
		selected = candidate
		return nil
	})
	return selected, err
}

// SaveProxyEnvScript atomically writes a port-qualified sourceable POSIX shell
// helper and returns its absolute path. Port qualification prevents concurrent
// app instances with explicit overrides from rewriting each other's command.
func (s *Store) SaveProxyEnvScript(port int, script []byte) (string, error) {
	if !validPort(port) {
		return "", fmt.Errorf("proxy port %d is outside 1..65535", port)
	}
	name := fmt.Sprintf(proxyEnvFile, port)
	path := filepath.Join(s.dir, name)
	if err := s.writeFile(name, script); err != nil {
		return "", err
	}
	return path, nil
}

// SaveProxySnapshot persists the OS proxy snapshot captured before a connect,
// so a crash mid-session can be cleaned up on the next launch.
func (s *Store) SaveProxySnapshot(snap proxymode.Snapshot) error {
	return s.writeJSON(proxySnapshotHdr, snap)
}

// LoadProxySnapshot returns the persisted snapshot and whether one existed. A
// present snapshot on startup means a prior session did not restore cleanly.
func (s *Store) LoadProxySnapshot() (proxymode.Snapshot, bool) {
	data, err := s.readFile(proxySnapshotHdr)
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
	var err error
	if s.stateDir != nil {
		// Deleting by pathname would follow a symlink swapped in after New
		// validated the directory, letting root unlink the file elsewhere.
		err = s.stateDir.Remove(proxySnapshotHdr)
	} else {
		err = os.Remove(filepath.Join(s.dir, proxySnapshotHdr))
	}
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// readFile reads one state file, through the pinned descriptor when the run is
// elevated. Every caller treats an error as "absent", so a symlink or a FIFO
// left at a state path reads as missing state instead of sending root through
// it — os.ReadFile would follow the one and block in open(2) on the other.
func (s *Store) readFile(name string) ([]byte, error) {
	if s.stateDir != nil {
		return s.stateDir.ReadFile(name)
	}
	return os.ReadFile(filepath.Join(s.dir, name))
}

func (s *Store) writeJSON(name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return s.writeFile(name, data)
}

func (s *Store) writeFile(name string, data []byte) error {
	if s.stateDir != nil {
		// Elevated runs write relative to the pinned directory descriptor.
		return s.stateDir.WriteFile(name, data, 0o600)
	}
	// Write-then-rename for atomicity, so a crash never leaves a half file.
	// A unique temporary name also lets concurrent callers safely refresh the
	// same port-qualified helper.
	file, err := os.CreateTemp(s.dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := sudouser.ChownFile(file); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, name))
}

func repairableStateFile(name string) bool {
	switch name {
	case recentsFile, proxyPortFile, proxyPortLockFile, proxySnapshotHdr, "client-id":
		return true
	}
	if !strings.HasPrefix(name, "proxy-env-") || !strings.HasSuffix(name, ".sh") {
		return false
	}
	portText := strings.TrimSuffix(strings.TrimPrefix(name, "proxy-env-"), ".sh")
	port, err := strconv.Atoi(portText)
	return err == nil && validPort(port) && strconv.Itoa(port) == portText
}

func validPort(port int) bool {
	return port >= 1 && port <= 65535
}
