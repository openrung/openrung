// SPDX-License-Identifier: GPL-3.0-or-later

package clientstate

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestInstallationKeyIsCreatedOncePrivatelyAndReused(t *testing.T) {
	dir := t.TempDir()
	store := NewInDir(dir)
	if _, err := os.Stat(filepath.Join(dir, installationKeyFile)); !os.IsNotExist(err) {
		t.Fatalf("key exists before first use: %v", err)
	}

	// Two handles racing first use (the desktop app and the CLI) agree.
	var wg sync.WaitGroup
	keys := make([][]byte, 4)
	for i := range keys {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pub, err := NewInDir(dir).InstallationKey().PublicKey()
			if err != nil {
				t.Error(err)
			}
			keys[i] = pub
		}(i)
	}
	wg.Wait()
	for _, pub := range keys[1:] {
		if string(pub) != string(keys[0]) {
			t.Fatal("concurrent first use created two keys")
		}
	}
	info, err := os.Stat(filepath.Join(dir, installationKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, want 0600", info.Mode().Perm())
	}

	handle := store.InstallationKey()
	spki, err := handle.PublicKey()
	if err != nil || string(spki) != string(keys[0]) {
		t.Fatalf("reload returned another key: %v", err)
	}
	digest := sha256.Sum256([]byte("statement"))
	sig, err := handle.Sign(digest[:])
	if err != nil || len(sig) != 64 {
		t.Fatalf("Sign = %d bytes, %v", len(sig), err)
	}
	parsed, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		t.Fatal(err)
	}
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(parsed.(*ecdsa.PublicKey), digest[:], r, s) {
		t.Fatal("signature does not verify under the public key")
	}
}

func TestInstallationKeyReplacesACorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, installationKeyFile), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := NewInDir(dir).InstallationKey().PublicKey()
	if err != nil {
		t.Fatalf("corrupt key file was not replaced: %v", err)
	}
	again, err := NewInDir(dir).InstallationKey().PublicKey()
	if err != nil || string(again) != string(first) {
		t.Fatalf("the replacement key was not persisted: %v", err)
	}
}

func TestUserIDRoundTrip(t *testing.T) {
	store := NewInDir(t.TempDir())
	if got := store.LoadUserID(); got != "" {
		t.Fatalf("LoadUserID on an empty store = %q", got)
	}
	if err := store.SaveUserID("user_0123"); err != nil {
		t.Fatal(err)
	}
	if got := store.LoadUserID(); got != "user_0123" {
		t.Fatalf("LoadUserID = %q", got)
	}
	if err := store.SaveUserID(" "); err == nil {
		t.Fatal("saved an empty user ID")
	}
}
