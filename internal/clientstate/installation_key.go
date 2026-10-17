// SPDX-License-Identifier: GPL-3.0-or-later

package clientstate

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

// The installation key (ADR-006) is this client installation's P-256
// identity: the broker derives a stable user ID from it. It lives in the
// per-install state directory as a PKCS#8 PEM file readable only by the user
// (0600), created on first use. The desktop app and the terminal client share
// the directory and therefore one identity. OS keychain backends are a later
// improvement; this file is the portable baseline.
const (
	installationKeyFile     = "installation-key.pem"
	installationKeyLockFile = "installation-key.lock"
	userIDFile              = "user-id.json"

	pemPrivateKey = "PRIVATE KEY"
)

// InstallationKey loads (or creates) the key lazily and caches it. It
// implements connectcore.InstallationKey.
type InstallationKey struct {
	store *Store

	mu  sync.Mutex
	key *ecdsa.PrivateKey
}

// InstallationKey returns the store's installation key handle. Nothing is
// read or written until the key is first used.
func (s *Store) InstallationKey() *InstallationKey {
	return &InstallationKey{store: s}
}

// PublicKey returns the DER SubjectPublicKeyInfo of the public key.
func (k *InstallationKey) PublicKey() ([]byte, error) {
	key, err := k.load()
	if err != nil {
		return nil, err
	}
	return x509.MarshalPKIXPublicKey(&key.PublicKey)
}

// Sign returns the raw 64-byte r‖s signature over a SHA-256 digest.
func (k *InstallationKey) Sign(digest []byte) ([]byte, error) {
	key, err := k.load()
	if err != nil {
		return nil, err
	}
	r, s, err := ecdsa.Sign(rand.Reader, key, digest)
	if err != nil {
		return nil, err
	}
	raw := make([]byte, 64)
	r.FillBytes(raw[:32])
	s.FillBytes(raw[32:])
	return raw, nil
}

func (k *InstallationKey) load() (*ecdsa.PrivateKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.key != nil {
		return k.key, nil
	}
	key, err := k.store.loadOrCreateInstallationKey()
	if err != nil {
		return nil, fmt.Errorf("load installation key: %w", err)
	}
	k.key = key
	return key, nil
}

// loadOrCreateInstallationKey serializes first use across client processes
// (the desktop app and the terminal client may start together), so both end
// up with the one key that was written. An unreadable or corrupt file is
// replaced: a key that cannot be parsed identifies nobody, and a new key is
// simply a new user.
func (s *Store) loadOrCreateInstallationKey() (*ecdsa.PrivateKey, error) {
	var key *ecdsa.PrivateKey
	err := withFileLock(filepath.Join(s.dir, installationKeyLockFile), func() error {
		if data, err := s.readFile(installationKeyFile); err == nil {
			if parsed, err := parseInstallationKey(data); err == nil {
				key = parsed
				return nil
			}
		}
		created, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return err
		}
		der, err := x509.MarshalPKCS8PrivateKey(created)
		if err != nil {
			return err
		}
		if err := s.writeFile(installationKeyFile, pem.EncodeToMemory(&pem.Block{Type: pemPrivateKey, Bytes: der})); err != nil {
			return err
		}
		key = created
		return nil
	})
	return key, err
}

func parseInstallationKey(data []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != pemPrivateKey {
		return nil, errors.New("installation key is not a PKCS#8 PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("installation key is not a P-256 ECDSA key")
	}
	return key, nil
}

type storedUserID struct {
	UserID string `json:"user_id"`
}

// SaveUserID records the broker-assigned user ID of this installation, so
// support can identify it without the key. The engine saves it after every
// successful authentication.
func (s *Store) SaveUserID(userID string) error {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return errors.New("user ID is empty")
	}
	if current := s.LoadUserID(); current == userID {
		return nil
	}
	return s.writeJSON(userIDFile, storedUserID{UserID: userID})
}

// LoadUserID returns the recorded user ID, or "" when none is stored.
func (s *Store) LoadUserID() string {
	data, err := s.readFile(userIDFile)
	if err != nil {
		return ""
	}
	var stored storedUserID
	if err := json.Unmarshal(data, &stored); err != nil {
		return ""
	}
	return stored.UserID
}
