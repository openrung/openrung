// Package clienttelemetry reports OpenRung CLI session telemetry to the broker.
//
// It mirrors the Android telemetry package (com.openrung.client.telemetry): a
// persistent client identity, per-session ids, a queued/batched outbox, and the
// connect-session lifecycle events. The wire contract is JSON shared with the
// broker (see internal/broker/telemetry.go); the Event struct below carries
// byte-for-byte identical JSON tags so the broker accepts CLI events unchanged.
package clienttelemetry

import (
	"crypto/rand"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"
)

// SchemaVersion is the telemetry schema the broker validates against
// (internal/broker/telemetry.go validateTelemetryEvent requires it to be 1).
const SchemaVersion = 1

// Event is a single telemetry event. JSON tags match the broker's TelemetryEvent
// exactly. The Android-only application_* / destination_* / protocol fields are
// omitted; the broker marks them omitempty, so leaving them off stays compatible.
type Event struct {
	SchemaVersion int               `json:"schema_version"`
	EventID       string            `json:"event_id"`
	Event         string            `json:"event"`
	OccurredAt    time.Time         `json:"occurred_at"`
	ClientID      string            `json:"client_id"`
	SessionID     string            `json:"session_id"`
	RelayID       string            `json:"relay_id,omitempty"`
	Attributes    map[string]string `json:"attributes,omitempty"`
	Measurements  map[string]int64  `json:"measurements,omitempty"`
}

// batch is the request body accepted by POST /api/v1/telemetry/events.
type batch struct {
	Events []Event `json:"events"`
}

// newUUID returns a random RFC 4122 v4 UUID. Mirrors the crypto/rand pattern in
// internal/relayruntime/config.go GenerateUUID (replicated to avoid importing the
// relay runtime package into the client layer).
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// DeviceAttributes returns the CLI analog of the Android device attributes. Only
// portable, non-identifying values are included; Android-only fields
// (android_api, device_*, network_*) have no CLI equivalent and are dropped.
func DeviceAttributes(appVersion string) map[string]string {
	attrs := map[string]string{
		"app_version": appVersion,
		// operating_system is the key the broker dashboard reads for the OS
		// column (internal/broker/dashboard.go); os/arch are kept for detail.
		"operating_system": operatingSystem(),
		"os":               runtime.GOOS,
		"arch":             runtime.GOARCH,
		"timezone":         timezone(),
	}
	if locale := locale(); locale != "" {
		attrs["locale"] = locale
	}
	return attrs
}

// operatingSystem returns a human-readable OS label (e.g. "macOS (arm64)") for
// the dashboard, mirroring how the Android client surfaces "Android (API NN)".
func operatingSystem() string {
	name := runtime.GOOS
	switch runtime.GOOS {
	case "darwin":
		name = "macOS"
	case "linux":
		name = "Linux"
	case "windows":
		name = "Windows"
	}
	return name + " (" + runtime.GOARCH + ")"
}

func timezone() string {
	if tz := strings.TrimSpace(os.Getenv("TZ")); tz != "" {
		return tz
	}
	return time.Local.String()
}

// locale derives a best-effort locale tag from the standard POSIX environment
// variables (e.g. "en_US.UTF-8" -> "en_US"); returns "" when unset.
func locale() string {
	for _, key := range []string{"LC_ALL", "LANG", "LANGUAGE"} {
		value := strings.TrimSpace(os.Getenv(key))
		if value == "" || strings.EqualFold(value, "C") || strings.EqualFold(value, "POSIX") {
			continue
		}
		if idx := strings.IndexAny(value, ".@:"); idx >= 0 {
			value = value[:idx]
		}
		if value != "" {
			return value
		}
	}
	return ""
}
