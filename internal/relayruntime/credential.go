package relayruntime

import (
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Rotating VLESS credentials.
//
// The relay's directory entry publishes the VLESS UUID that admits a client,
// so a copied entry is a permanent credential unless the UUID changes. The
// relay therefore derives a fresh UUID for every CredentialRotationPeriod
// bucket, registers the current and previous buckets with xray, announces the
// current one to the broker on its heartbeat, and drops a bucket once it is
// two behind and the broker has moved on. A copied credential dies within two
// periods; a legitimate directory snapshot (bounded by the list's not_after,
// which is far shorter than one period) never holds a credential the relay
// has already retired.
//
// Derivation is relay-local: the key comes from the relay's identity seed, so
// a restart re-derives the same schedule and nothing new is persisted, and
// the broker only ever learns the values the relay announces.

// CredentialRotationPeriod is how long one derived credential is the current
// one. The relay keeps the previous period's credential registered too, so a
// credential is accepted for two periods after it is first served.
const CredentialRotationPeriod = time.Hour

const (
	credentialKeyLabel  = "openrung-cred-v1"
	credentialKeyLength = 32
	credentialEmailTime = "20060102T150405Z"
)

// Credential is one derived VLESS user: the UUID clients present and the
// xray email label that names it for runtime add/remove. The label encodes
// the bucket start, so any abuse report that quotes a credential attributes
// to an epoch.
type Credential struct {
	ID    string
	Email string
	// Bucket is the UTC start of the rotation period this credential covers.
	Bucket time.Time
}

// DeriveCredentialKey derives the credential key from the relay's identity
// seed with HKDF-SHA256. epoch salts the derivation: an operator who suspects
// the key leaked changes it (OPENRUNG_CREDENTIAL_EPOCH) and every derived
// credential changes without touching the identity itself.
func DeriveCredentialKey(identityKey ed25519.PrivateKey, epoch string) ([]byte, error) {
	if len(identityKey) != ed25519.PrivateKeySize {
		return nil, errors.New("derive credential key: identity key must be an Ed25519 private key")
	}
	return hkdf.Key(sha256.New, identityKey.Seed(), nil, credentialKeyLabel+"|"+epoch, credentialKeyLength)
}

// RandomCredentialKey is the key for a relay with no identity seed: the
// schedule then survives only the current process, exactly like the
// generated static UUID it replaces.
func RandomCredentialKey() ([]byte, error) {
	key := make([]byte, credentialKeyLength)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate credential key: %w", err)
	}
	return key, nil
}

// CredentialSchedule derives the credential for any rotation bucket.
type CredentialSchedule struct {
	key    []byte
	period time.Duration
}

// NewCredentialSchedule binds a derivation key to a rotation period. A zero
// period means CredentialRotationPeriod; anything shorter than a second is
// rejected so a misconfiguration cannot churn credentials faster than the
// broker heartbeat can announce them.
func NewCredentialSchedule(key []byte, period time.Duration) (CredentialSchedule, error) {
	if len(key) < 16 {
		return CredentialSchedule{}, errors.New("credential schedule: key must be at least 16 bytes")
	}
	if period == 0 {
		period = CredentialRotationPeriod
	}
	if period < time.Second {
		return CredentialSchedule{}, errors.New("credential schedule: period must be at least one second")
	}
	return CredentialSchedule{key: append([]byte(nil), key...), period: period}, nil
}

// Period is the schedule's rotation period.
func (s CredentialSchedule) Period() time.Duration { return s.period }

// Bucket is the UTC start of the rotation period containing now.
func (s CredentialSchedule) Bucket(now time.Time) time.Time {
	return now.UTC().Truncate(s.period)
}

// Current is the credential for the bucket containing now.
func (s CredentialSchedule) Current(now time.Time) Credential {
	return s.Credential(s.Bucket(now))
}

// Previous is the credential for the bucket before the one containing now.
func (s CredentialSchedule) Previous(now time.Time) Credential {
	return s.Credential(s.Bucket(now).Add(-s.period))
}

// NextRotation is when the bucket containing now ends.
func (s CredentialSchedule) NextRotation(now time.Time) time.Time {
	return s.Bucket(now).Add(s.period)
}

// Credential derives the credential for an exact bucket start: the first 16
// bytes of HMAC-SHA256(key, label|bucket_unix) with the UUID version-4 bits
// set, so it is indistinguishable from the random UUIDs relays minted before
// rotation existed.
func (s CredentialSchedule) Credential(bucket time.Time) Credential {
	bucket = bucket.UTC()
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(credentialKeyLabel + "|" + strconv.FormatInt(bucket.Unix(), 10)))
	sum := mac.Sum(nil)
	var b [16]byte
	copy(b[:], sum[:16])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return Credential{
		ID:     fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]),
		Email:  CredentialEmail(bucket),
		Bucket: bucket,
	}
}

// CredentialEmail is the xray user label for a bucket start.
func CredentialEmail(bucket time.Time) string {
	return "cred-" + bucket.UTC().Format(credentialEmailTime)
}
