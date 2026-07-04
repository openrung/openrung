package clienttelemetry

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClientIDPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "openrung", "client-id")

	original := clientIDPath
	clientIDPath = func() (string, error) { return path, nil }
	t.Cleanup(func() { clientIDPath = original })

	first, err := ClientID()
	if err != nil {
		t.Fatalf("first ClientID: %v", err)
	}
	if first == "" {
		t.Fatal("expected a non-empty client id")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("client-id file not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("client-id file perm = %o, want 600", perm)
	}

	second, err := ClientID()
	if err != nil {
		t.Fatalf("second ClientID: %v", err)
	}
	if second != first {
		t.Fatalf("client id changed across calls: %q != %q", first, second)
	}
}

func TestClientIDFallsBackWhenPathUnavailable(t *testing.T) {
	original := clientIDPath
	clientIDPath = func() (string, error) { return "", os.ErrNotExist }
	t.Cleanup(func() { clientIDPath = original })

	id, err := ClientID()
	if err != nil {
		t.Fatalf("ClientID fallback: %v", err)
	}
	if id == "" {
		t.Fatal("expected ephemeral client id on fallback")
	}
}
