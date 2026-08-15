package connectcore

import (
	"net"
	"strings"
	"testing"
)

func TestEnsureProxyPortAvailableReportsStablePortCollision(t *testing.T) {
	listener, err := net.Listen("tcp", ProxyHost+":0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := EnsureProxyPortAvailable(port); err == nil || !strings.Contains(err.Error(), ProxyPortEnv) {
		t.Fatalf("occupied port error = %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := EnsureProxyPortAvailable(port); err != nil {
		t.Fatalf("released port still unavailable: %v", err)
	}
}
