package tunnel

import (
	"context"
	"testing"
	"time"
)

// TestClientRunStopsOnContextCancel verifies the reconnect loop exits promptly
// when the context is cancelled even while the hub is unreachable.
func TestClientRunStopsOnContextCancel(t *testing.T) {
	client := &Client{
		HubAddr:      "127.0.0.1:1", // nothing listening here
		Hello:        HelloFrame{Token: "x"},
		TargetHost:   "127.0.0.1",
		TargetPort:   1,
		ReconnectMin: 10 * time.Millisecond,
		ReconnectMax: 20 * time.Millisecond,
		DialTimeout:  100 * time.Millisecond,
		Logger:       discardLogger(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after context cancel")
	}
}
