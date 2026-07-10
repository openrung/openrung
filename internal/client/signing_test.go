package client

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"openrung/internal/relay"
)

// vectorsFile mirrors the canonical testdata/signing_vectors.json layout
// shared across the client repos (SPEC v1 §2.3, §11).
type vectorsFile struct {
	SignatureHeader string `json:"signature_header"`
	SpecVector      struct {
		SeedB64      string `json:"seed_b64"`
		PubKeyHex    string `json:"pubkey_hex"`
		KeyID        string `json:"key_id"`
		Body         string `json:"body"`
		SignatureB64 string `json:"signature_b64"`
		Header       string `json:"header"`
	} `json:"spec_vector"`
	PinnedKeys []struct {
		Role               string `json:"role"`
		PubKeyHex          string `json:"pubkey_hex"`
		KeyID              string `json:"key_id"`
		VectorMessage      string `json:"vector_message"`
		VectorSignatureB64 string `json:"vector_signature_b64"`
	} `json:"pinned_keys"`
}

func loadVectors(t *testing.T) vectorsFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/signing_vectors.json")
	if err != nil {
		t.Fatalf("read signing vectors: %v", err)
	}
	var v vectorsFile
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parse signing vectors: %v", err)
	}
	return v
}

// vectorSigner derives the §2.3 TEST-ONLY private key from the committed seed.
func vectorSigner(t *testing.T, v vectorsFile) ed25519.PrivateKey {
	t.Helper()
	seed, err := base64.StdEncoding.DecodeString(v.SpecVector.SeedB64)
	if err != nil || len(seed) != ed25519.SeedSize {
		t.Fatalf("vector seed is not a 32-byte base64 seed: %v", err)
	}
	return ed25519.NewKeyFromSeed(seed)
}

// vectorPinnedKeys is a pinned set holding only the spec-vector public key.
func vectorPinnedKeys(t *testing.T, v vectorsFile) []pinnedKey {
	t.Helper()
	return mustPinnedKeys(v.SpecVector.PubKeyHex)
}

// vectorNow is a local clock for which the committed vector body's not_after
// (2026-07-10T00:30:00Z) is comfortably fresh.
var vectorNow = time.Date(2026, 7, 10, 0, 5, 0, 0, time.UTC)

// signedHeader builds the §2.1 wire header for body under key.
func signedHeader(key ed25519.PrivateKey, keyID string, body []byte) string {
	return "ed25519;" + keyID + ";" + base64.StdEncoding.EncodeToString(ed25519.Sign(key, body))
}

// signedListBody marshals a ListResponse and signs it, returning the exact
// bytes and their header — the sign-what-you-send pairing entry-point tests
// serve from stub round-trippers.
func signedListBody(t *testing.T, key ed25519.PrivateKey, keyID string, resp relay.ListResponse) ([]byte, string) {
	t.Helper()
	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal signed body: %v", err)
	}
	return body, signedHeader(key, keyID, body)
}

func TestSpecVectorVerifies(t *testing.T) {
	v := loadVectors(t)
	out, keyIDUsed, err := verifyRelayList(vectorPinnedKeys(t, v), v.SpecVector.Header,
		[]byte(v.SpecVector.Body), relay.ChannelAPI, 1, vectorNow)
	if err != nil {
		t.Fatalf("spec vector must verify: %v", err)
	}
	if keyIDUsed != v.SpecVector.KeyID {
		t.Fatalf("verified under key_id %q, want %q", keyIDUsed, v.SpecVector.KeyID)
	}
	if out.Count != 1 || out.Channel != relay.ChannelAPI || out.Limit != 1 {
		t.Fatalf("parsed body lost fields: %+v", out)
	}
}

// TestSpecVectorInternallyConsistent pins the vector file itself: the seed
// derives the committed pubkey, the key_id matches the §2.2 derivation, and
// the header is the exact §2.1 assembly of key_id and signature.
func TestSpecVectorInternallyConsistent(t *testing.T) {
	v := loadVectors(t)
	if v.SignatureHeader != RelaySignatureHeader {
		t.Fatalf("vectors name header %q, code uses %q", v.SignatureHeader, RelaySignatureHeader)
	}
	pub := vectorSigner(t, v).Public().(ed25519.PublicKey)
	if hex.EncodeToString(pub) != v.SpecVector.PubKeyHex {
		t.Fatalf("seed derives pubkey %x, vector says %s", pub, v.SpecVector.PubKeyHex)
	}
	if got := signingKeyID(pub); got != v.SpecVector.KeyID {
		t.Fatalf("key_id derivation: got %s, vector says %s", got, v.SpecVector.KeyID)
	}
	wantHeader := "ed25519;" + v.SpecVector.KeyID + ";" + v.SpecVector.SignatureB64
	if v.SpecVector.Header != wantHeader {
		t.Fatalf("vector header %q is not key_id+signature assembly %q", v.SpecVector.Header, wantHeader)
	}
}

// TestPinnedProductionKeysCIGuard is the §11 CI guard: each embedded pinned
// constant (active AND standby, in that order) must match the committed
// vectors file and verify its promotion vector. A truncated or typo'd pinned
// constant fails here, not on promotion day.
func TestPinnedProductionKeysCIGuard(t *testing.T) {
	v := loadVectors(t)
	if len(v.PinnedKeys) != 2 || len(pinnedRelayKeys) != 2 {
		t.Fatalf("want 2 pinned keys in vectors and code, got %d and %d", len(v.PinnedKeys), len(pinnedRelayKeys))
	}
	wantRoles := []string{"active", "standby"}
	for i, entry := range v.PinnedKeys {
		if entry.Role != wantRoles[i] {
			t.Fatalf("pinned_keys[%d] role %q, want %q (ordered active, standby)", i, entry.Role, wantRoles[i])
		}
		pinned := pinnedRelayKeys[i]
		if hex.EncodeToString(pinned.key) != entry.PubKeyHex {
			t.Fatalf("%s: embedded constant %x does not match vectors pubkey %s", entry.Role, []byte(pinned.key), entry.PubKeyHex)
		}
		if pinned.id != entry.KeyID || signingKeyID(pinned.key) != entry.KeyID {
			t.Fatalf("%s: key_id %s does not match vectors %s", entry.Role, pinned.id, entry.KeyID)
		}
		sig, err := base64.StdEncoding.DecodeString(entry.VectorSignatureB64)
		if err != nil {
			t.Fatalf("%s: vector signature base64: %v", entry.Role, err)
		}
		if !ed25519.Verify(pinned.key, []byte(entry.VectorMessage), sig) {
			t.Fatalf("%s: promotion vector does not verify under the embedded pinned key", entry.Role)
		}
	}
}

// TestVerifyRejectsNegativeCases exercises every §12 negative vector against
// the shared verifier. Each case must fail as a distinguishable
// "unsigned/invalid relay list" error — never panic, never accept.
func TestVerifyRejectsNegativeCases(t *testing.T) {
	v := loadVectors(t)
	keys := vectorPinnedKeys(t, v)
	signer := vectorSigner(t, v)
	body := []byte(v.SpecVector.Body)

	flippedBody := append([]byte(nil), body...)
	flippedBody[len(flippedBody)/2] ^= 0x01

	sig, err := base64.StdEncoding.DecodeString(v.SpecVector.SignatureB64)
	if err != nil {
		t.Fatalf("decode vector signature: %v", err)
	}
	flippedSig := append([]byte(nil), sig...)
	flippedSig[0] ^= 0x01
	flippedSigHeader := "ed25519;" + v.SpecVector.KeyID + ";" + base64.StdEncoding.EncodeToString(flippedSig)

	shortSigHeader := "ed25519;" + v.SpecVector.KeyID + ";" + base64.StdEncoding.EncodeToString(sig[:63])

	// A validly signed body whose channel says "mirror" must not feed an API
	// slot, and one without not_after must not pass freshness.
	mirrorBody := []byte(`{"count":1,"server_time":"2026-07-10T00:00:00Z","not_after":"2026-07-10T00:30:00Z","key_id":"` + v.SpecVector.KeyID + `","channel":"mirror","relays":[]}`)
	noNotAfterBody := []byte(`{"count":1,"server_time":"2026-07-10T00:00:00Z","key_id":"` + v.SpecVector.KeyID + `","channel":"api","limit":1,"relays":[]}`)

	cases := []struct {
		name           string
		keys           []pinnedKey
		header         string
		body           []byte
		requestedLimit int
		now            time.Time
		wantReason     string
	}{
		{"flipped body byte", keys, v.SpecVector.Header, flippedBody, 1, vectorNow, "does not verify under any pinned key"},
		{"flipped signature byte", keys, flippedSigHeader, body, 1, vectorNow, "does not verify under any pinned key"},
		{"unpinned key", pinnedRelayKeys, v.SpecVector.Header, body, 1, vectorNow, "does not verify under any pinned key"},
		{"missing header", keys, "", body, 1, vectorNow, "has not enabled relay-list signing"},
		{"truncated header", keys, "ed25519;" + v.SpecVector.KeyID, body, 1, vectorNow, "want 3 ';'-separated fields"},
		{"wrong algorithm", keys, "rsa;" + v.SpecVector.KeyID + ";" + v.SpecVector.SignatureB64, body, 1, vectorNow, "unsupported signature algorithm"},
		{"signature not base64", keys, "ed25519;" + v.SpecVector.KeyID + ";!!!", body, 1, vectorNow, "not valid standard base64"},
		{"signature wrong length", keys, shortSigHeader, body, 1, vectorNow, "63 bytes, want 64"},
		{"expired not_after", keys, v.SpecVector.Header, body, 1, time.Date(2026, 7, 10, 0, 35, 1, 0, time.UTC), "list expired"},
		{"limit mismatch", keys, v.SpecVector.Header, body, 2, vectorNow, "echoed limit 1 does not match requested limit 2"},
		{"channel mismatch", keys, signedHeader(signer, v.SpecVector.KeyID, mirrorBody), mirrorBody, 1, vectorNow, `channel "mirror" does not match`},
		{"missing not_after", keys, signedHeader(signer, v.SpecVector.KeyID, noNotAfterBody), noNotAfterBody, 1, vectorNow, "no not_after freshness bound"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := verifyRelayList(tc.keys, tc.header, tc.body, relay.ChannelAPI, tc.requestedLimit, tc.now)
			if err == nil {
				t.Fatal("verification must fail")
			}
			if !strings.Contains(err.Error(), "unsigned/invalid relay list") {
				t.Fatalf("error must carry the unsigned/invalid marker, got: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantReason) {
				t.Fatalf("error %q does not name the failed check %q", err, tc.wantReason)
			}
		})
	}
}

// TestVerifyNotAfterSkewBoundary pins the §5.2 freshness rule exactly:
// not_after ≥ now − 5 min accepts, one second past rejects.
func TestVerifyNotAfterSkewBoundary(t *testing.T) {
	v := loadVectors(t)
	keys := vectorPinnedKeys(t, v)
	body := []byte(v.SpecVector.Body) // not_after 2026-07-10T00:30:00Z

	atBoundary := time.Date(2026, 7, 10, 0, 35, 0, 0, time.UTC)
	if _, _, err := verifyRelayList(keys, v.SpecVector.Header, body, relay.ChannelAPI, 1, atBoundary); err != nil {
		t.Fatalf("not_after exactly now-5m must still verify: %v", err)
	}
	pastBoundary := atBoundary.Add(time.Second)
	if _, _, err := verifyRelayList(keys, v.SpecVector.Header, body, relay.ChannelAPI, 1, pastBoundary); err == nil {
		t.Fatal("not_after past the 5m skew allowance must fail")
	}
}

// TestVerifyKeyIDFallback covers §4.2: key_id is advisory routing only. A
// header naming an unknown key_id — or the WRONG pinned key — still verifies
// as long as the signature checks out under some pinned key.
func TestVerifyKeyIDFallback(t *testing.T) {
	v := loadVectors(t)
	body := []byte(v.SpecVector.Body)
	sigB64 := v.SpecVector.SignatureB64

	// A decoy key pinned AHEAD of the real one, so fallback has to walk past a
	// failed verify to succeed.
	decoyPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate decoy key: %v", err)
	}
	keys := mustPinnedKeys(hex.EncodeToString(decoyPub), v.SpecVector.PubKeyHex)

	cases := []struct {
		name   string
		header string
	}{
		{"unknown key_id", "ed25519;deadbeefdeadbeef;" + sigB64},
		{"decoy's key_id", "ed25519;" + signingKeyID(decoyPub) + ";" + sigB64},
		{"correct key_id routes past the decoy", "ed25519;" + v.SpecVector.KeyID + ";" + sigB64},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, keyIDUsed, err := verifyRelayList(keys, tc.header, body, relay.ChannelAPI, 1, vectorNow)
			if err != nil {
				t.Fatalf("advisory key_id must not break verification: %v", err)
			}
			if keyIDUsed != v.SpecVector.KeyID {
				t.Fatalf("verified key_id %q, want the real signer %q", keyIDUsed, v.SpecVector.KeyID)
			}
			if out.Count != 1 {
				t.Fatalf("parsed body lost fields: %+v", out)
			}
		})
	}
}

// TestReadVerifiedRelayListHeaderCaseInsensitive serves the signature header
// under a literally lowercase map key — what a hand-built (or exotic
// non-canonicalizing) transport would produce — and expects the fold-scan
// fallback to find it (§2.1: header names match case-insensitively).
func TestReadVerifiedRelayListHeaderCaseInsensitive(t *testing.T) {
	v := loadVectors(t)
	restore := PinRelayListKeysForTest(v.SpecVector.PubKeyHex)
	defer restore()

	now := time.Now().UTC()
	body, sigHeader := signedListBody(t, vectorSigner(t, v), v.SpecVector.KeyID, relay.ListResponse{
		Count:      0,
		ServerTime: now,
		NotAfter:   now.Add(30 * time.Minute),
		KeyID:      v.SpecVector.KeyID,
		Channel:    relay.ChannelAPI,
		Limit:      1,
	})
	header := make(http.Header)
	header["x-openrung-relays-signature"] = []string{sigHeader} // bypasses canonicalization on purpose
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(string(body))),
	}
	if _, err := ReadVerifiedRelayList(resp, "https://broker.example.com/api/v1/relays?limit=1", 1); err != nil {
		t.Fatalf("lowercase header name must be read case-insensitively: %v", err)
	}
}

// TestReadVerifiedRelayListLoopbackExemption is the §5.2 dev-flow allowance:
// an unsigned plain-JSON list from a loopback broker parses fine.
func TestReadVerifiedRelayListLoopbackExemption(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"count":1,"server_time":"2026-07-10T00:00:00Z","relays":[{"id":"r1","public_host":"1.2.3.4","public_port":443}]}`))
	}))
	defer srv.Close()

	out, err := BrokerClient{BaseURL: srv.URL}.ListRelays(context.Background(), 5, "", "")
	if err != nil {
		t.Fatalf("loopback broker must be exempt from signing: %v", err)
	}
	if len(out.Relays) != 1 || out.Relays[0].ID != "r1" {
		t.Fatalf("unexpected relays: %+v", out.Relays)
	}
}

// TestListRelaysRejectsUnsignedNonLoopback is the flip side: a non-loopback
// broker serving the same unsigned body fails with the distinguishable
// unsigned/invalid message, not a decode error.
func TestListRelaysRejectsUnsignedNonLoopback(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader(`{"count":0,"server_time":"2026-07-10T00:00:00Z","relays":[]}`)),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	})}

	_, err := BrokerClient{BaseURL: "https://broker.example.com", HTTPClient: httpClient}.ListRelays(context.Background(), 5, "", "")
	if err == nil {
		t.Fatal("unsigned non-loopback response must fail")
	}
	if !strings.Contains(err.Error(), "unsigned/invalid relay list") {
		t.Fatalf("want the unsigned/invalid marker, got: %v", err)
	}
}

// TestListRelaysVerifiesSignedResponse drives the full BrokerClient entry
// point against a signed response: raw bytes are read, verified against the
// (test-pinned) key, and the same buffer is parsed.
func TestListRelaysVerifiesSignedResponse(t *testing.T) {
	v := loadVectors(t)
	restore := PinRelayListKeysForTest(v.SpecVector.PubKeyHex)
	defer restore()
	signer := vectorSigner(t, v)

	now := time.Now().UTC()
	body, header := signedListBody(t, signer, v.SpecVector.KeyID, relay.ListResponse{
		Count:      1,
		ServerTime: now,
		NotAfter:   now.Add(30 * time.Minute),
		KeyID:      v.SpecVector.KeyID,
		Channel:    relay.ChannelAPI,
		Limit:      3,
		Relays:     []relay.Descriptor{validRelay(now)},
	})
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		h := make(http.Header)
		h.Set(RelaySignatureHeader, header)
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader(string(body))),
			Header:     h,
			Request:    r,
		}, nil
	})}

	out, err := BrokerClient{BaseURL: "https://broker.example.com", HTTPClient: httpClient}.ListRelays(context.Background(), 3, "", "")
	if err != nil {
		t.Fatalf("signed response must verify: %v", err)
	}
	if out.Count != 1 || len(out.Relays) != 1 {
		t.Fatalf("unexpected response: %+v", out)
	}
}

// TestListRelaysDefaultLimitEcho pins the normalization contract: a caller
// passing limit<1 requests the default limit, and the echoed-limit check must
// compare against that default, not the raw zero.
func TestListRelaysDefaultLimitEcho(t *testing.T) {
	v := loadVectors(t)
	restore := PinRelayListKeysForTest(v.SpecVector.PubKeyHex)
	defer restore()
	signer := vectorSigner(t, v)

	now := time.Now().UTC()
	body, header := signedListBody(t, signer, v.SpecVector.KeyID, relay.ListResponse{
		Count:      0,
		ServerTime: now,
		NotAfter:   now.Add(30 * time.Minute),
		KeyID:      v.SpecVector.KeyID,
		Channel:    relay.ChannelAPI,
		Limit:      defaultRelayLimit,
	})
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		h := make(http.Header)
		h.Set(RelaySignatureHeader, header)
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader(string(body))),
			Header:     h,
			Request:    r,
		}, nil
	})}

	if _, err := (BrokerClient{BaseURL: "https://broker.example.com", HTTPClient: httpClient}).ListRelays(context.Background(), 0, "", ""); err != nil {
		t.Fatalf("default-limit fetch must verify against the normalized limit: %v", err)
	}
}
