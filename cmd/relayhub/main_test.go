package main

import (
	"net"
	"testing"
	"time"

	"openrung/internal/relayhub"
)

func TestVersionInfo(t *testing.T) {
	originalVersion, originalRevision := version, revision
	version, revision = " 1.2.3 ", " abcdef0 "
	t.Cleanup(func() {
		version, revision = originalVersion, originalRevision
	})

	if got := versionInfo(); got != "relayhub/1.2.3 revision=abcdef0" {
		t.Fatalf("versionInfo() = %q, want component, version, and revision", got)
	}
}

func TestEnvDefault(t *testing.T) {
	t.Setenv("OPENRUNG_TEST_KEY", "from-env")
	if got := envDefault("OPENRUNG_TEST_KEY", "fallback"); got != "from-env" {
		t.Fatalf("envDefault = %q, want from-env", got)
	}
	if got := envDefault("OPENRUNG_TEST_MISSING", "fallback"); got != "fallback" {
		t.Fatalf("envDefault = %q, want fallback", got)
	}
}

func TestEnvDurationDefault(t *testing.T) {
	t.Setenv("OPENRUNG_TEST_DUR", "45s")
	if got := envDurationDefault("OPENRUNG_TEST_DUR", time.Second); got != 45*time.Second {
		t.Fatalf("envDurationDefault = %s, want 45s", got)
	}
	t.Setenv("OPENRUNG_TEST_DUR", "not-a-duration")
	if got := envDurationDefault("OPENRUNG_TEST_DUR", 10*time.Second); got != 10*time.Second {
		t.Fatalf("envDurationDefault fallback = %s, want 10s", got)
	}
}

func TestControlListenerPlaintext(t *testing.T) {
	cfg := relayhub.Config{ControlAddr: "127.0.0.1:0"}
	listener, err := controlListener(cfg)
	if err != nil {
		t.Fatalf("controlListener: %v", err)
	}
	defer listener.Close()

	conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("dial control listener: %v", err)
	}
	_ = conn.Close()
}

func TestControlListenerBadTLS(t *testing.T) {
	cfg := relayhub.Config{
		ControlAddr: "127.0.0.1:0",
		TLSCertPath: "/nonexistent/cert.pem",
		TLSKeyPath:  "/nonexistent/key.pem",
	}
	if _, err := controlListener(cfg); err == nil {
		t.Fatal("expected error loading missing TLS key pair")
	}
}
