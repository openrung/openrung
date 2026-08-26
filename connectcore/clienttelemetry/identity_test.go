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

func TestClientIDDoesNotFollowSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("sensitive\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "openrung", "client-id")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	origPath := clientIDPath
	clientIDPath = func() (string, error) { return path, nil }
	t.Cleanup(func() { clientIDPath = origPath })

	id, err := ClientID()
	if err != nil {
		t.Fatalf("ClientID: %v", err)
	}
	if id == "" || id == "sensitive" {
		t.Fatalf("ClientID = %q, want an ephemeral ID unrelated to target", id)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "sensitive\n" {
		t.Fatalf("symlink target changed to %q", data)
	}
}
