package tunnel

import (
	"errors"
	"net"
	"testing"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func TestPortAllocatorInvalidRange(t *testing.T) {
	if _, err := NewPortAllocator(0, 10); err == nil {
		t.Fatal("expected error for start < 1")
	}
	if _, err := NewPortAllocator(100, 50); err == nil {
		t.Fatal("expected error for start > end")
	}
	if _, err := NewPortAllocator(70000, 70001); err == nil {
		t.Fatal("expected error for end > 65535")
	}
}

func TestPortAllocatorAllocateReleaseExhaust(t *testing.T) {
	port := freePort(t)
	alloc, err := NewPortAllocator(port, port)
	if err != nil {
		t.Fatalf("NewPortAllocator: %v", err)
	}

	got, err := alloc.Allocate()
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got != port {
		t.Fatalf("Allocate returned %d, want %d", got, port)
	}
	if alloc.InUse() != 1 {
		t.Fatalf("InUse = %d, want 1", alloc.InUse())
	}

	if _, err := alloc.Allocate(); !errors.Is(err, errPortsExhausted) {
		t.Fatalf("expected errPortsExhausted, got %v", err)
	}

	alloc.Release(port)
	if alloc.InUse() != 0 {
		t.Fatalf("InUse after release = %d, want 0", alloc.InUse())
	}
	if _, err := alloc.Allocate(); err != nil {
		t.Fatalf("Allocate after release: %v", err)
	}
}
