package relayruntime

import (
	"crypto/ed25519"
	"regexp"
	"testing"
	"time"
)

var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func testIdentityKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	return ed25519.NewKeyFromSeed(seed)
}

// The same seed and epoch must derive the same key across restarts, and the
// epoch must change every derived credential without touching the identity.
func TestDeriveCredentialKeyIsDeterministicPerEpoch(t *testing.T) {
	identity := testIdentityKey(t)
	first, err := DeriveCredentialKey(identity, "")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	again, err := DeriveCredentialKey(identity, "")
	if err != nil {
		t.Fatalf("derive again: %v", err)
	}
	if string(first) != string(again) {
		t.Fatal("the same seed and epoch derived different keys")
	}
	if len(first) != credentialKeyLength {
		t.Fatalf("key length = %d, want %d", len(first), credentialKeyLength)
	}
	rotated, err := DeriveCredentialKey(identity, "2")
	if err != nil {
		t.Fatalf("derive with epoch: %v", err)
	}
	if string(rotated) == string(first) {
		t.Fatal("changing the epoch did not change the key")
	}
	if _, err := DeriveCredentialKey(ed25519.PrivateKey("short"), ""); err == nil {
		t.Fatal("a malformed identity key must be rejected")
	}
}

func TestCredentialScheduleDerivesStableVersion4UUIDs(t *testing.T) {
	key, err := DeriveCredentialKey(testIdentityKey(t), "")
	if err != nil {
		t.Fatalf("derive key: %v", err)
	}
	schedule, err := NewCredentialSchedule(key, 0)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if schedule.Period() != CredentialRotationPeriod {
		t.Fatalf("period = %s, want %s", schedule.Period(), CredentialRotationPeriod)
	}

	now := time.Date(2026, 9, 17, 14, 37, 12, 0, time.FixedZone("plus-nine", 9*3600))
	current := schedule.Current(now)
	if want := time.Date(2026, 9, 17, 5, 0, 0, 0, time.UTC); !current.Bucket.Equal(want) {
		t.Fatalf("bucket = %s, want the UTC hour %s", current.Bucket, want)
	}
	if !uuidV4Pattern.MatchString(current.ID) {
		t.Fatalf("credential %q is not a version-4 UUID", current.ID)
	}
	if current.Email != "cred-20260917T050000Z" {
		t.Fatalf("email = %q", current.Email)
	}
	if again := schedule.Current(now.Add(20 * time.Minute)); again != current {
		t.Fatalf("the same bucket derived a different credential: %+v vs %+v", again, current)
	}
	previous := schedule.Previous(now)
	if previous.ID == current.ID || !previous.Bucket.Equal(current.Bucket.Add(-time.Hour)) {
		t.Fatalf("previous = %+v, want the hour before %+v", previous, current)
	}
	if next := schedule.NextRotation(now); !next.Equal(current.Bucket.Add(time.Hour)) {
		t.Fatalf("next rotation = %s, want %s", next, current.Bucket.Add(time.Hour))
	}
	if following := schedule.Current(schedule.NextRotation(now)); following.ID == current.ID {
		t.Fatal("the next bucket derived the same credential")
	}

	otherKey, err := DeriveCredentialKey(testIdentityKey(t), "other")
	if err != nil {
		t.Fatalf("derive other key: %v", err)
	}
	other, err := NewCredentialSchedule(otherKey, 0)
	if err != nil {
		t.Fatalf("other schedule: %v", err)
	}
	if other.Current(now).ID == current.ID {
		t.Fatal("different keys derived the same credential for one bucket")
	}
}

func TestNewCredentialScheduleRejectsWeakInputs(t *testing.T) {
	if _, err := NewCredentialSchedule(make([]byte, 8), 0); err == nil {
		t.Fatal("a short key must be rejected")
	}
	if _, err := NewCredentialSchedule(make([]byte, 32), 100*time.Millisecond); err == nil {
		t.Fatal("a sub-second period must be rejected")
	}
	random, err := RandomCredentialKey()
	if err != nil {
		t.Fatalf("random key: %v", err)
	}
	if _, err := NewCredentialSchedule(random, 2*time.Second); err != nil {
		t.Fatalf("random key schedule: %v", err)
	}
}
