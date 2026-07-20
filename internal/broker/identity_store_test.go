package broker

import (
	"errors"
	"strings"
	"testing"
	"time"

	"openrung/internal/relay"
)

// The stable-identity store tests run against both backends through this
// harness: the in-memory store always, Postgres when OPENRUNG_TEST_POSTGRES_URL
// is set (mirroring the existing postgres-gated conventions).
func runIdentityStoreTest(t *testing.T, test func(t *testing.T, store RelayStore)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) {
		test(t, NewStore())
	})
	t.Run("postgres", func(t *testing.T) {
		test(t, newTestPostgresStore(t, RankingModeGlobal))
	})
}

func signedIdentityRequest(t *testing.T, seed string, mutate func(*relay.RegisterRequest), now time.Time) relay.RegisterRequest {
	t.Helper()
	priv, err := relay.ParseIdentitySeed(seed)
	if err != nil {
		t.Fatalf("parse seed: %v", err)
	}
	req := validRegisterRequest()
	if mutate != nil {
		mutate(&req)
	}
	req.IdentityPublicKey, req.IdentityProof, req.IdentityExpiresAt =
		relay.SignIdentity(priv, req, now.Add(relay.IdentityProofTTLDirect))
	return req
}

const (
	identityStoreSeedA = "QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI="
	identityStoreSeedB = "Q0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0M="
)

func TestStoreIdentityRegistrationKeepsRelayIDAcrossReRegistration(t *testing.T) {
	runIdentityStoreTest(t, func(t *testing.T, store RelayStore) {
		now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

		first, err := store.Register(signedIdentityRequest(t, identityStoreSeedA, nil, now), now, 3*time.Minute)
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		if !strings.HasPrefix(first.ID, "relay_") || len(first.ID) != len("relay_")+32 {
			t.Fatalf("derived ID has the wrong shape: %s", first.ID)
		}

		// Same identity, later re-registration (e.g. after the broker forgot
		// the lease): same relay ID, fresh lease.
		later := now.Add(10 * time.Minute)
		second, err := store.Register(signedIdentityRequest(t, identityStoreSeedA, nil, later), later, 3*time.Minute)
		if err != nil {
			t.Fatalf("re-register: %v", err)
		}
		if second.ID != first.ID {
			t.Fatalf("relay ID churned across re-registration: %s -> %s", first.ID, second.ID)
		}
		if !second.ExpiresAt.After(second.RegisteredAt) {
			t.Fatalf("stale lease on re-registration: %+v", second)
		}

		// A different identity at a different endpoint must derive a different ID.
		other, err := store.Register(signedIdentityRequest(t, identityStoreSeedB, func(r *relay.RegisterRequest) {
			r.PublicHost = "198.51.100.44"
		}, later), later, 3*time.Minute)
		if err != nil {
			t.Fatalf("register other identity: %v", err)
		}
		if other.ID == first.ID {
			t.Fatal("distinct identities derived the same relay ID")
		}

		// Heartbeat works against the derived ID like any other.
		if _, err := store.Heartbeat(first.ID, relay.NodeClassVolunteer, later.Add(time.Minute), 3*time.Minute); err != nil {
			t.Fatalf("heartbeat derived ID: %v", err)
		}
	})
}

func TestStoreIdentityEndpointMoveAbandonsOldRow(t *testing.T) {
	runIdentityStoreTest(t, func(t *testing.T, store RelayStore) {
		now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

		first, err := store.Register(signedIdentityRequest(t, identityStoreSeedA, nil, now), now, 3*time.Minute)
		if err != nil {
			t.Fatalf("register: %v", err)
		}

		// The same identity re-registers from a new endpoint (relay moved
		// hosts). Same ID, exactly one row: without the same-ID eviction, the
		// Postgres id PRIMARY KEY would reject this insert outright.
		moved, err := store.Register(signedIdentityRequest(t, identityStoreSeedA, func(r *relay.RegisterRequest) {
			r.PublicHost = "198.51.100.77"
		}, now.Add(time.Minute)), now.Add(time.Minute), 3*time.Minute)
		if err != nil {
			t.Fatalf("register moved endpoint: %v", err)
		}
		if moved.ID != first.ID {
			t.Fatalf("relay ID churned on endpoint move: %s -> %s", first.ID, moved.ID)
		}
		listed, err := store.List(now.Add(time.Minute), 20)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(listed) != 1 {
			t.Fatalf("expected exactly one row after the move, got %d: %+v", len(listed), listed)
		}
		if listed[0].PublicHost != "198.51.100.77" {
			t.Fatalf("listing still shows the old endpoint: %+v", listed[0])
		}
	})
}

func TestStoreIdentityCannotSeizeLiveFoundationEndpoint(t *testing.T) {
	runIdentityStoreTest(t, func(t *testing.T, store RelayStore) {
		now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

		foundationReq := validRegisterRequest()
		foundationReq.NodeClass = relay.NodeClassFoundation
		foundation, err := store.Register(foundationReq, now, 3*time.Minute)
		if err != nil {
			t.Fatalf("register foundation relay: %v", err)
		}

		// A volunteer-class identity registration at the same endpoint must be
		// refused exactly like a legacy one — a valid possession proof for a
		// DIFFERENT key grants no authority over the endpoint.
		_, err = store.Register(signedIdentityRequest(t, identityStoreSeedA, nil, now.Add(time.Minute)), now.Add(time.Minute), 3*time.Minute)
		if !errors.Is(err, ErrNodeClassForbidden) {
			t.Fatalf("expected ErrNodeClassForbidden, got %v", err)
		}
		if _, err := store.Heartbeat(foundation.ID, relay.NodeClassFoundation, now.Add(time.Minute), 3*time.Minute); err != nil {
			t.Fatalf("foundation relay lost its row to a refused registration: %v", err)
		}
	})
}

func TestStoreIdentitySeizureRollbackKeepsOldRow(t *testing.T) {
	runIdentityStoreTest(t, func(t *testing.T, store RelayStore) {
		now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

		// The identity registers legitimately at endpoint A...
		first, err := store.Register(signedIdentityRequest(t, identityStoreSeedA, nil, now), now, 3*time.Minute)
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		// ...a foundation relay holds endpoint B...
		foundationReq := validRegisterRequest()
		foundationReq.NodeClass = relay.NodeClassFoundation
		foundationReq.PublicHost = "198.51.100.88"
		if _, err := store.Register(foundationReq, now, 3*time.Minute); err != nil {
			t.Fatalf("register foundation relay: %v", err)
		}
		// ...and the identity then tries to move onto endpoint B as a
		// volunteer. The registration is refused, and the refusal must not
		// have evicted the identity's own row at endpoint A (the Postgres
		// implementation deletes it inside the same transaction — a rollback
		// must restore it).
		_, err = store.Register(signedIdentityRequest(t, identityStoreSeedA, func(r *relay.RegisterRequest) {
			r.PublicHost = "198.51.100.88"
		}, now.Add(time.Minute)), now.Add(time.Minute), 3*time.Minute)
		if !errors.Is(err, ErrNodeClassForbidden) {
			t.Fatalf("expected ErrNodeClassForbidden, got %v", err)
		}
		if _, err := store.Heartbeat(first.ID, relay.NodeClassVolunteer, now.Add(time.Minute), 3*time.Minute); err != nil {
			t.Fatalf("refused move evicted the identity's own row: %v", err)
		}
	})
}

func TestStoreIdentityInvalidProofRejected(t *testing.T) {
	runIdentityStoreTest(t, func(t *testing.T, store RelayStore) {
		now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

		// Tampered content after signing.
		req := signedIdentityRequest(t, identityStoreSeedA, nil, now)
		req.Label = "evil-otter"
		if _, err := store.Register(req, now, 3*time.Minute); !errors.Is(err, relay.ErrIdentityProofInvalid) {
			t.Fatalf("expected ErrIdentityProofInvalid, got %v", err)
		}

		// Expired proof.
		expired := signedIdentityRequest(t, identityStoreSeedA, nil, now.Add(-2*relay.IdentityProofTTLDirect))
		if _, err := store.Register(expired, now, 3*time.Minute); !errors.Is(err, relay.ErrIdentityProofExpired) {
			t.Fatalf("expected ErrIdentityProofExpired, got %v", err)
		}

		// Legacy registration is untouched by all of this: random ID minted.
		legacy, err := store.Register(validRegisterRequest(), now, 3*time.Minute)
		if err != nil {
			t.Fatalf("legacy register: %v", err)
		}
		if legacy.ID == "" || legacy.IdentityPublicKey != "" {
			t.Fatalf("legacy registration gained identity state: %+v", legacy)
		}
	})
}
