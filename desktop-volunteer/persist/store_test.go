// SPDX-License-Identifier: GPL-3.0-or-later

package persist

import (
	"os"
	"path/filepath"
	"testing"

	"openrung/internal/relayruntime/engine"
)

func TestLoadSettingsRejectsCorruptFile(t *testing.T) {
	for _, body := range []string{
		`{"label":"saved","maxSessions":"invalid","consentAccepted":true}`,
		`{"label":"saved",`,
	} {
		t.Run(body, func(t *testing.T) {
			s := NewInDir(t.TempDir())
			if err := os.WriteFile(filepath.Join(s.Dir(), settingsFile), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := s.LoadSettings(); got != (Settings{}) {
				t.Fatalf("corrupt settings returned partial state: %+v", got)
			}
		})
	}
}

func TestLoadIdentityRejectsCorruptFile(t *testing.T) {
	for _, body := range []string{
		`{"clientId":"saved","realityPrivateKey":123,"shortId":"saved"}`,
		`{"clientId":"saved",`,
	} {
		t.Run(body, func(t *testing.T) {
			s := NewInDir(t.TempDir())
			if err := os.WriteFile(filepath.Join(s.Dir(), identityFile), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := s.LoadIdentity(); got != (engine.Identity{}) {
				t.Fatalf("corrupt identity returned partial state: %+v", got)
			}
		})
	}
}

func TestStoreRoundTrip(t *testing.T) {
	s := NewInDir(t.TempDir())
	if got := s.LoadSettings(); got != (Settings{}) {
		t.Fatalf("missing settings = %+v", got)
	}
	if got := s.LoadIdentity(); got != (engine.Identity{}) {
		t.Fatalf("missing identity = %+v", got)
	}
	settings := Settings{Label: "saved", MaxSessions: 8, ListenPort: 8443, ConsentAccepted: true}
	if err := s.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	if got := s.LoadSettings(); got != settings {
		t.Fatalf("settings = %+v, want %+v", got, settings)
	}
	identity := engine.Identity{ClientID: "saved", RealityPrivateKey: "private", RealityPublicKey: "public", ShortID: "short", IdentitySeed: "seed"}
	if err := s.SaveIdentity(identity); err != nil {
		t.Fatal(err)
	}
	if got := s.LoadIdentity(); got != identity {
		t.Fatalf("identity = %+v, want %+v", got, identity)
	}
}

func TestLoadOlderFiles(t *testing.T) {
	s := NewInDir(t.TempDir())
	for name, body := range map[string]string{
		settingsFile: `{"label":"saved","futureField":true}`,
		identityFile: `{"clientId":"saved","futureField":true}`,
	} {
		if err := os.WriteFile(filepath.Join(s.Dir(), name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.LoadSettings(); got != (Settings{Label: "saved"}) {
		t.Fatalf("settings with optional fields omitted = %+v", got)
	}
	if got := s.LoadIdentity(); got != (engine.Identity{ClientID: "saved"}) {
		t.Fatalf("identity with optional fields omitted = %+v", got)
	}
}
