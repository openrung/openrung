package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"openrung/internal/relay"
)

func TestValidateRegisterRequest(t *testing.T) {
	req := validRegisterRequest()
	if err := validateRegisterRequest(req); err != nil {
		t.Fatalf("expected valid request: %v", err)
	}

	req.Protocol = "unknown"
	if err := validateRegisterRequest(req); err == nil {
		t.Fatal("expected protocol validation error")
	}
}

func TestHeartbeatRelayID(t *testing.T) {
	id, ok := heartbeatRelayID("/api/v1/volunteers/relay_abc/heartbeat")
	if !ok || id != "relay_abc" {
		t.Fatalf("expected relay_abc, got id=%q ok=%v", id, ok)
	}
}

func TestValidateExitModes(t *testing.T) {
	req := validRegisterRequest()
	req.ExitMode = relay.ExitModeDedicated
	if err := validateRegisterRequest(req); err != nil {
		t.Fatalf("expected dedicated exit mode to be schema-valid: %v", err)
	}
}

func TestValidateTransport(t *testing.T) {
	req := validRegisterRequest()

	req.Transport = relay.TransportTunnel
	if err := validateRegisterRequest(req); err != nil {
		t.Fatalf("expected tunnel transport to be valid: %v", err)
	}

	req.Transport = "" // empty is allowed and treated as direct downstream
	if err := validateRegisterRequest(req); err != nil {
		t.Fatalf("expected empty transport to be valid: %v", err)
	}

	req.Transport = "carrier-pigeon"
	if err := validateRegisterRequest(req); err == nil {
		t.Fatal("expected unknown transport to be rejected")
	}
}

func TestRegisterDefaultsTransportDirect(t *testing.T) {
	store := NewStore()
	desc, err := store.Register(validRegisterRequest(), time.Now().UTC(), time.Minute)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if desc.Transport != relay.TransportDirect {
		t.Fatalf("transport = %q, want %q", desc.Transport, relay.TransportDirect)
	}
}

func TestRegisterStoresAndReturnsLabel(t *testing.T) {
	server := NewServer(NewStore(), Config{})

	req := validRegisterRequest()
	req.Label = "happy-hippo"
	body, _ := json.Marshal(req)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/register", bytes.NewReader(body)))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var desc relay.Descriptor
	if err := json.Unmarshal(recorder.Body.Bytes(), &desc); err != nil {
		t.Fatalf("decode descriptor: %v", err)
	}
	if desc.Label != "happy-hippo" {
		t.Fatalf("expected label happy-hippo, got %q", desc.Label)
	}

	listRecorder := httptest.NewRecorder()
	server.ServeHTTP(listRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/relays", nil))
	if !strings.Contains(listRecorder.Body.String(), `"label":"happy-hippo"`) {
		t.Fatalf("expected label in list response: %s", listRecorder.Body.String())
	}
}

func TestRegisterRejectsUnsafeLabel(t *testing.T) {
	server := NewServer(NewStore(), Config{})

	req := validRegisterRequest()
	req.Label = "<script>alert(1)</script>"
	body, _ := json.Marshal(req)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/register", bytes.NewReader(body)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unsafe label, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestHealthzReportsStoreFailure(t *testing.T) {
	server := NewServer(failingStore{Store: NewStore(), pingErr: errors.New("database down")}, Config{})
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestListRelaysReportsStoreFailure(t *testing.T) {
	server := NewServer(failingStore{Store: NewStore(), listErr: errors.New("database down")}, Config{})
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/relays?limit=5", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestHeartbeatMissingRelayStillReturnsNotFound(t *testing.T) {
	server := NewServer(NewStore(), Config{})
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/volunteers/relay_missing/heartbeat", nil))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

type failingStore struct {
	*Store
	pingErr error
	listErr error
}

func (s failingStore) Ping(context.Context) error {
	return s.pingErr
}

func (s failingStore) List(now time.Time, limit int) ([]relay.Descriptor, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.Store.List(now, limit)
}
