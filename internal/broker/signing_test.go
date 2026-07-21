package broker

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"openrung/internal/relay"
)

// testSigningSeed is the SPEC v1 §2.3 TEST-ONLY seed (32 bytes of 0x42) used
// wherever a test constructs a broker. Real signing seeds never enter the repo.
func testSigningSeed() []byte {
	return bytes.Repeat([]byte{0x42}, 32)
}

// signingVectors mirrors testdata/signing_vectors.json, the canonical vector
// file committed verbatim to every repo that signs or verifies relay lists.
type signingVectors struct {
	SpecVector struct {
		Note         string `json:"note"`
		SeedB64      string `json:"seed_b64"`
		PubkeyHex    string `json:"pubkey_hex"`
		KeyID        string `json:"key_id"`
		Body         string `json:"body"`
		SignatureB64 string `json:"signature_b64"`
	} `json:"spec_vector"`
	PinnedKeys []struct {
		Name               string `json:"name"`
		PubkeyHex          string `json:"pubkey_hex"`
		KeyID              string `json:"key_id"`
		VectorMessage      string `json:"vector_message"`
		VectorSignatureB64 string `json:"vector_signature_b64"`
	} `json:"pinned_keys"`
}

func loadSigningVectors(t *testing.T) signingVectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/signing_vectors.json")
	if err != nil {
		t.Fatalf("read signing vectors: %v", err)
	}
	var vectors signingVectors
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatalf("decode signing vectors: %v", err)
	}
	return vectors
}

func mustHex(t *testing.T, value string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(value)
	if err != nil {
		t.Fatalf("decode hex %q: %v", value, err)
	}
	return raw
}

func mustBase64(t *testing.T, value string) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatalf("decode base64 %q: %v", value, err)
	}
	return raw
}

// verifyRelayListSignature asserts the header parses as
// "ed25519;<key_id>;<base64 signature>" and that the signature verifies over
// the raw body bytes with the given hex public key. Returns the header key_id.
func verifyRelayListSignature(t *testing.T, header string, body []byte, pubkeyHex string) string {
	t.Helper()
	if header == "" {
		t.Fatal("missing X-OpenRung-Relays-Signature header")
	}
	parts := strings.Split(header, ";")
	if len(parts) != 3 || parts[0] != "ed25519" {
		t.Fatalf("malformed signature header %q", header)
	}
	pub := ed25519.PublicKey(mustHex(t, pubkeyHex))
	if !ed25519.Verify(pub, body, mustBase64(t, parts[2])) {
		t.Fatalf("signature did not verify over the raw wire bytes: %s", body)
	}
	return parts[1]
}

type relayListWireTimestamps struct {
	ServerTime string `json:"server_time"`
	NotAfter   string `json:"not_after"`
	Relays     []struct {
		RegisteredAt    string `json:"registered_at"`
		LastHeartbeatAt string `json:"last_heartbeat_at"`
		ExpiresAt       string `json:"expires_at"`
	} `json:"relays"`
}

func decodeRelayListWireTimestamps(t *testing.T, body []byte) relayListWireTimestamps {
	t.Helper()
	var wire relayListWireTimestamps
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("decode relay-list wire timestamps: %v", err)
	}
	return wire
}

func assertWholeSecondUTC(t *testing.T, field, value string) {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatalf("%s is not RFC 3339: %q: %v", field, value, err)
	}
	want := parsed.UTC().Truncate(time.Second).Format(time.RFC3339)
	if value != want {
		t.Fatalf("%s = %q, want whole-second UTC %q", field, value, want)
	}
}

func assertRelayListUsesWholeSecondUTCTimestamps(t *testing.T, body []byte) {
	t.Helper()
	wire := decodeRelayListWireTimestamps(t, body)
	assertWholeSecondUTC(t, "server_time", wire.ServerTime)
	assertWholeSecondUTC(t, "not_after", wire.NotAfter)
	for i, relay := range wire.Relays {
		assertWholeSecondUTC(t, fmt.Sprintf("relays[%d].registered_at", i), relay.RegisteredAt)
		assertWholeSecondUTC(t, fmt.Sprintf("relays[%d].last_heartbeat_at", i), relay.LastHeartbeatAt)
		assertWholeSecondUTC(t, fmt.Sprintf("relays[%d].expires_at", i), relay.ExpiresAt)
	}
}

func TestParseSigningSeedFailFast(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"missing", ""},
		{"not base64", "!!!definitely-not-base64!!!"},
		{"wrong length", base64.StdEncoding.EncodeToString([]byte("too-short"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSigningSeed(tc.value)
			if err == nil {
				t.Fatalf("expected %s seed to be rejected", tc.name)
			}
			// The operator greps the crash-loop log for the env var name.
			if !strings.Contains(err.Error(), "OPENRUNG_RELAY_SIGNING_KEY") {
				t.Fatalf("expected error to name the env var, got: %v", err)
			}
		})
	}
}

func TestParseSigningSeedAcceptsSpecVector(t *testing.T) {
	vectors := loadSigningVectors(t)
	seed, err := ParseSigningSeed(vectors.SpecVector.SeedB64)
	if err != nil {
		t.Fatalf("expected the spec vector seed to parse: %v", err)
	}
	if !bytes.Equal(seed, testSigningSeed()) {
		t.Fatalf("expected 32 bytes of 0x42, got %x", seed)
	}
	if got := SigningKeyID(seed); got != vectors.SpecVector.KeyID {
		t.Fatalf("key_id = %q, want %q", got, vectors.SpecVector.KeyID)
	}
}

func TestKeyIDDerivationMatchesSpecVector(t *testing.T) {
	vectors := loadSigningVectors(t)
	key := ed25519.NewKeyFromSeed(testSigningSeed())
	pub := key.Public().(ed25519.PublicKey)
	if got := hex.EncodeToString(pub); got != vectors.SpecVector.PubkeyHex {
		t.Fatalf("pubkey = %s, want %s", got, vectors.SpecVector.PubkeyHex)
	}
	if got := signingKeyID(pub); got != vectors.SpecVector.KeyID {
		t.Fatalf("key_id = %q, want %q", got, vectors.SpecVector.KeyID)
	}
}

func TestSpecVectorSignatureRoundTrip(t *testing.T) {
	vectors := loadSigningVectors(t)
	key := ed25519.NewKeyFromSeed(testSigningSeed())
	sig := ed25519.Sign(key, []byte(vectors.SpecVector.Body))
	if got := base64.StdEncoding.EncodeToString(sig); got != vectors.SpecVector.SignatureB64 {
		t.Fatalf("signature = %s, want %s", got, vectors.SpecVector.SignatureB64)
	}
}

// TestPinnedKeyVectorsVerify is the CI guard from the key rotation runbook: a
// truncated or typo'd pinned public key fails here instead of being discovered
// on promotion day. Both production keys (active and standby) must verify
// their committed promotion vectors, or the standby is not promotable.
func TestPinnedKeyVectorsVerify(t *testing.T) {
	vectors := loadSigningVectors(t)
	if len(vectors.PinnedKeys) < 2 {
		t.Fatalf("expected active + standby pinned keys, got %d", len(vectors.PinnedKeys))
	}
	for _, pinned := range vectors.PinnedKeys {
		t.Run(pinned.Name, func(t *testing.T) {
			pub := mustHex(t, pinned.PubkeyHex)
			if len(pub) != ed25519.PublicKeySize {
				t.Fatalf("pinned %s pubkey is %d bytes, want %d", pinned.Name, len(pub), ed25519.PublicKeySize)
			}
			sig := mustBase64(t, pinned.VectorSignatureB64)
			if !ed25519.Verify(ed25519.PublicKey(pub), []byte(pinned.VectorMessage), sig) {
				t.Fatalf("pinned %s key does not verify its promotion vector", pinned.Name)
			}
			if got := signingKeyID(pub); got != pinned.KeyID {
				t.Fatalf("pinned %s key_id = %q, want %q", pinned.Name, got, pinned.KeyID)
			}
		})
	}
}

func TestWriteSignedUsesWholeSecondUTCTimestampsWithoutMutatingSource(t *testing.T) {
	vectors := loadSigningVectors(t)
	zone := time.FixedZone("UTC+8", 8*60*60)
	serverTime := time.Date(2026, time.July, 21, 16, 36, 39, 987654321, zone)
	resp := relay.ListResponse{
		Count:      1,
		ServerTime: serverTime,
		NotAfter:   serverTime.Add(apiNotAfterWindow),
		KeyID:      vectors.SpecVector.KeyID,
		Channel:    relay.ChannelAPI,
		Limit:      1,
		Relays: []relay.Descriptor{{
			RegisteredAt:    serverTime.Add(-time.Hour),
			LastHeartbeatAt: serverTime.Add(-12 * time.Second),
			ExpiresAt:       serverTime.Add(3 * time.Minute),
		}},
	}
	originalServerTime := resp.ServerTime
	originalNotAfter := resp.NotAfter
	originalRelay := resp.Relays[0]

	recorder := httptest.NewRecorder()
	newSigner(testSigningSeed()).writeSigned(recorder, resp)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.Bytes()
	verifyRelayListSignature(t, recorder.Header().Get(signatureHeader), body, vectors.SpecVector.PubkeyHex)
	assertRelayListUsesWholeSecondUTCTimestamps(t, body)

	wire := decodeRelayListWireTimestamps(t, body)
	if wire.ServerTime != "2026-07-21T08:36:39Z" {
		t.Errorf("server_time = %q, want %q", wire.ServerTime, "2026-07-21T08:36:39Z")
	}
	if wire.NotAfter != "2026-07-21T09:06:39Z" {
		t.Errorf("not_after = %q, want %q", wire.NotAfter, "2026-07-21T09:06:39Z")
	}
	if len(wire.Relays) != 1 {
		t.Fatalf("relays = %d, want 1", len(wire.Relays))
	}
	if wire.Relays[0].RegisteredAt != "2026-07-21T07:36:39Z" {
		t.Errorf("registered_at = %q, want %q", wire.Relays[0].RegisteredAt, "2026-07-21T07:36:39Z")
	}
	if wire.Relays[0].LastHeartbeatAt != "2026-07-21T08:36:27Z" {
		t.Errorf("last_heartbeat_at = %q, want %q", wire.Relays[0].LastHeartbeatAt, "2026-07-21T08:36:27Z")
	}
	if wire.Relays[0].ExpiresAt != "2026-07-21T08:39:39Z" {
		t.Errorf("expires_at = %q, want %q", wire.Relays[0].ExpiresAt, "2026-07-21T08:39:39Z")
	}

	if resp.ServerTime != originalServerTime || resp.NotAfter != originalNotAfter || resp.Relays[0] != originalRelay {
		t.Fatal("writeSigned mutated the caller's timestamps")
	}
}

// TestListRelaysSignsExactWireBytes pins sign-what-you-send end to end: a real
// HTTP GET whose raw wire bytes must verify against the spec vector public
// key. This is the test that catches the Marshal-vs-Encode trailing-newline
// class of bug.
func TestListRelaysSignsExactWireBytes(t *testing.T) {
	vectors := loadSigningVectors(t)
	store := NewStore()
	if _, err := store.Register(validRegisterRequest(), time.Now().UTC(), time.Minute); err != nil {
		t.Fatalf("register: %v", err)
	}
	server := httptest.NewServer(NewServer(store, Config{SigningSeed: testSigningSeed()}))
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/relays?limit=1")
	if err != nil {
		t.Fatalf("get relays: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read raw body: %v", err)
	}
	assertRelayListUsesWholeSecondUTCTimestamps(t, body)

	keyID := verifyRelayListSignature(t, resp.Header.Get("X-OpenRung-Relays-Signature"), body, vectors.SpecVector.PubkeyHex)
	if keyID != vectors.SpecVector.KeyID {
		t.Fatalf("header key_id = %q, want %q", keyID, vectors.SpecVector.KeyID)
	}
	if bytes.HasSuffix(body, []byte("\n")) {
		t.Fatal("signed body must not carry Encode's trailing newline")
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store, no-transform" {
		t.Fatalf("Cache-Control = %q, want %q", got, "no-store, no-transform")
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := resp.Header.Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Fatalf("Content-Length = %q, want %d", got, len(body))
	}

	var out relay.ListResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode relay list: %v", err)
	}
	if out.KeyID != keyID {
		t.Fatalf("body key_id %q != header key_id %q", out.KeyID, keyID)
	}
	if out.Channel != relay.ChannelAPI {
		t.Fatalf("channel = %q, want %q", out.Channel, relay.ChannelAPI)
	}
	if out.Count != 1 || len(out.Relays) != 1 {
		t.Fatalf("expected the registered relay in the response, got count=%d len=%d", out.Count, len(out.Relays))
	}
}

func TestListRelaysBodyFields(t *testing.T) {
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed()})
	wantKeyID := SigningKeyID(testSigningSeed())

	cases := []struct {
		name      string
		query     string
		wantLimit int
	}{
		{"limit 1", "?limit=1", 1},
		{"limit 20", "?limit=20", 20},
		{"default", "", 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/relays"+tc.query, nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
			}
			var out relay.ListResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode relay list: %v", err)
			}
			if out.Limit != tc.wantLimit {
				t.Fatalf("echoed limit = %d, want %d", out.Limit, tc.wantLimit)
			}
			if out.Channel != relay.ChannelAPI {
				t.Fatalf("channel = %q, want %q", out.Channel, relay.ChannelAPI)
			}
			if got := out.NotAfter.Sub(out.ServerTime); got != apiNotAfterWindow {
				t.Fatalf("not_after - server_time = %s, want %s", got, apiNotAfterWindow)
			}
			if out.KeyID != wantKeyID {
				t.Fatalf("key_id = %q, want %q", out.KeyID, wantKeyID)
			}
		})
	}
}

func TestMirrorRelayListFields(t *testing.T) {
	vectors := loadSigningVectors(t)
	store := NewStore()
	if _, err := store.Register(validRegisterRequest(), time.Now().UTC(), time.Minute); err != nil {
		t.Fatalf("register: %v", err)
	}
	server := httptest.NewServer(NewServer(store, Config{SigningSeed: testSigningSeed()}))
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/relays.mirror")
	if err != nil {
		t.Fatalf("get mirror relays: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read raw body: %v", err)
	}
	assertRelayListUsesWholeSecondUTCTimestamps(t, body)

	verifyRelayListSignature(t, resp.Header.Get("X-OpenRung-Relays-Signature"), body, vectors.SpecVector.PubkeyHex)
	var out relay.ListResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode mirror list: %v", err)
	}
	if out.Channel != relay.ChannelMirror {
		t.Fatalf("channel = %q, want %q", out.Channel, relay.ChannelMirror)
	}
	if got := out.NotAfter.Sub(out.ServerTime); got != mirrorNotAfterWindow {
		t.Fatalf("not_after - server_time = %s, want %s", got, mirrorNotAfterWindow)
	}
	if out.Count != 1 || len(out.Relays) != 1 {
		t.Fatalf("expected the full directory in the mirror body, got count=%d len=%d", out.Count, len(out.Relays))
	}
	// The mirror body is not request-shaped: it must carry no limit field at
	// all, so clients skip the echo check on this channel.
	if strings.Contains(string(body), `"limit"`) {
		t.Fatalf("mirror body must not carry a limit field: %s", body)
	}
}

func TestRelayListErrorsCarryNoSignature(t *testing.T) {
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed()})
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/relays?limit=0", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad limit, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get(signatureHeader); got != "" {
		t.Fatalf("400 response must not be signed, got header %q", got)
	}

	failing := NewServer(failingStore{Store: NewStore(), listErr: errors.New("database down")}, Config{SigningSeed: testSigningSeed()})
	for _, path := range []string{"/api/v1/relays", "/api/v1/relays.mirror"} {
		errRecorder := httptest.NewRecorder()
		failing.ServeHTTP(errRecorder, httptest.NewRequest(http.MethodGet, path, nil))
		if errRecorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 from %s, got %d: %s", path, errRecorder.Code, errRecorder.Body.String())
		}
		if got := errRecorder.Header().Get(signatureHeader); got != "" {
			t.Fatalf("503 response from %s must not be signed, got header %q", path, got)
		}
	}
}

func TestHealthzReportsSigningKeyID(t *testing.T) {
	vectors := loadSigningVectors(t)
	server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed()})
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var health struct {
		OK           bool   `json:"ok"`
		SigningKeyID string `json:"signing_key_id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &health); err != nil {
		t.Fatalf("decode healthz: %v", err)
	}
	if !health.OK {
		t.Fatal("expected healthz ok")
	}
	if health.SigningKeyID != vectors.SpecVector.KeyID {
		t.Fatalf("signing_key_id = %q, want %q", health.SigningKeyID, vectors.SpecVector.KeyID)
	}
}

// TestNewServerRequiresSigningSeed pins the no-serve-unsigned contract at the
// library boundary too: constructing a broker without a seed is a programming
// error, never a silent unsigned mode.
func TestNewServerRequiresSigningSeed(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected NewServer to panic without a signing seed")
		}
	}()
	NewServer(NewStore(), Config{})
}
