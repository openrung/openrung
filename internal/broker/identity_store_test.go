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
		if _, err := store.Heartbeat(first.ID, second.LeaseToken, relay.NodeClassVolunteer, later.Add(time.Minute), 3*time.Minute); err != nil {
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

func TestStoreIdentityHeartbeatCannotRenewDifferentRegistration(t *testing.T) {
	runIdentityStoreTest(t, func(t *testing.T, store RelayStore) {
		now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
		originalReq := signedIdentityRequest(t, identityStoreSeedA, func(r *relay.RegisterRequest) {
			r.Transport = relay.TransportTunnel
			r.PublicHost = "hub-a.example"
			r.PublicPort = 20001
		}, now)

		legitimate, err := store.Register(originalReq, now, 3*time.Minute)
		if err != nil {
			t.Fatalf("register legitimate tunnel: %v", err)
		}
		if legitimate.LeaseToken == "" {
			t.Fatal("stable registration returned an empty lease token")
		}

		// A tunnel proof intentionally does not bind the hub-assigned endpoint,
		// so model a captured proof (or stale hub session) moving the stable ID.
		replayedReq := originalReq
		replayedReq.PublicHost = "hub-b.example"
		replayedReq.PublicPort = 20002
		replayed, err := store.Register(replayedReq, now.Add(time.Minute), 3*time.Minute)
		if err != nil {
			t.Fatalf("replay registration: %v", err)
		}
		if replayed.ID != legitimate.ID || replayed.LeaseToken == legitimate.LeaseToken {
			t.Fatalf("registration incarnation did not rotate cleanly: first=%+v replayed=%+v", legitimate, replayed)
		}

		// The displaced session must receive not-found and must not extend the
		// endpoint written by the replay. That response drives re-registration.
		if _, err := store.Heartbeat(legitimate.ID, legitimate.LeaseToken, relay.NodeClassVolunteer, now.Add(2*time.Minute), 3*time.Minute); !errors.Is(err, ErrRelayNotFound) {
			t.Fatalf("displaced heartbeat error = %v, want ErrRelayNotFound", err)
		}
		listed, err := store.List(now.Add(2*time.Minute), 20)
		if err != nil {
			t.Fatalf("list replayed relay: %v", err)
		}
		if len(listed) != 1 || listed[0].PublicHost != replayedReq.PublicHost || !listed[0].ExpiresAt.Equal(replayed.ExpiresAt) {
			t.Fatalf("displaced heartbeat changed replayed registration: %+v", listed)
		}

		recovered, err := store.Register(originalReq, now.Add(2*time.Minute), 3*time.Minute)
		if err != nil {
			t.Fatalf("legitimate re-registration: %v", err)
		}
		if recovered.LeaseToken == replayed.LeaseToken {
			t.Fatal("legitimate re-registration reused replay's lease token")
		}
		if _, err := store.Heartbeat(replayed.ID, replayed.LeaseToken, relay.NodeClassVolunteer, now.Add(3*time.Minute), 3*time.Minute); !errors.Is(err, ErrRelayNotFound) {
			t.Fatalf("stale replay heartbeat error = %v, want ErrRelayNotFound", err)
		}
		if _, err := store.Heartbeat(recovered.ID, recovered.LeaseToken, relay.NodeClassVolunteer, now.Add(3*time.Minute), 3*time.Minute); err != nil {
			t.Fatalf("recovered registration heartbeat: %v", err)
		}

		listed, err = store.List(now.Add(3*time.Minute), 20)
		if err != nil {
			t.Fatalf("list recovered relay: %v", err)
		}
		if len(listed) != 1 || listed[0].PublicHost != originalReq.PublicHost || listed[0].PublicPort != originalReq.PublicPort {
			t.Fatalf("stale heartbeat displaced recovered endpoint: %+v", listed)
		}
	})
}

func TestStoreStableFoundationHeartbeatChecksClassBeforeLease(t *testing.T) {
	runIdentityStoreTest(t, func(t *testing.T, store RelayStore) {
		now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
		req := signedIdentityRequest(t, identityStoreSeedA, func(r *relay.RegisterRequest) {
			r.NodeClass = relay.NodeClassFoundation
		}, now)
		desc, err := store.Register(req, now, 3*time.Minute)
		if err != nil {
			t.Fatalf("register stable foundation relay: %v", err)
		}
		if _, err := store.Heartbeat(desc.ID, "wrong-token", relay.NodeClassVolunteer, now.Add(time.Minute), 3*time.Minute); !errors.Is(err, ErrNodeClassForbidden) {
			t.Fatalf("unprivileged wrong-token heartbeat = %v, want ErrNodeClassForbidden", err)
		}
		if _, err := store.Heartbeat(desc.ID, "wrong-token", relay.NodeClassFoundation, now.Add(time.Minute), 3*time.Minute); !errors.Is(err, ErrRelayNotFound) {
			t.Fatalf("authorized wrong-token heartbeat = %v, want ErrRelayNotFound", err)
		}
	})
}

func TestStoreIdentityStaleGeoCannotUpdateReplacement(t *testing.T) {
	runIdentityStoreTest(t, func(t *testing.T, store RelayStore) {
		now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
		firstReq := signedIdentityRequest(t, identityStoreSeedA, func(r *relay.RegisterRequest) {
			r.Transport = relay.TransportTunnel
			r.PublicHost = "hub-a.example"
			r.PublicPort = 20001
		}, now)
		first, err := store.Register(firstReq, now, 3*time.Minute)
		if err != nil {
			t.Fatalf("register first endpoint: %v", err)
		}

		secondReq := firstReq
		secondReq.PublicHost = "hub-b.example"
		secondReq.PublicPort = 20002
		second, err := store.Register(secondReq, now.Add(time.Minute), 3*time.Minute)
		if err != nil {
			t.Fatalf("register replacement endpoint: %v", err)
		}

		staleGeo := relay.GeoLocation{City: "Old City", Country: "Old Country", CountryCode: "OC"}
		if err := store.UpdateGeo(first.ID, first.LeaseToken, staleGeo); !errors.Is(err, ErrRelayNotFound) {
			t.Fatalf("stale geo update = %v, want ErrRelayNotFound", err)
		}
		listed, err := store.List(now.Add(time.Minute), 20)
		if err != nil {
			t.Fatalf("list replacement after stale geo: %v", err)
		}
		if len(listed) != 1 || listed[0].PublicHost != secondReq.PublicHost || listed[0].GeoLocation != (relay.GeoLocation{}) {
			t.Fatalf("stale geo contaminated replacement: %+v", listed)
		}

		freshGeo := relay.GeoLocation{City: "New City", Country: "New Country", CountryCode: "NC"}
		if err := store.UpdateGeo(second.ID, second.LeaseToken, freshGeo); err != nil {
			t.Fatalf("current geo update: %v", err)
		}
		listed, err = store.List(now.Add(time.Minute), 20)
		if err != nil {
			t.Fatalf("list replacement after current geo: %v", err)
		}
		if len(listed) != 1 || listed[0].GeoLocation != freshGeo {
			t.Fatalf("current geo was not stored: %+v", listed)
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
		if _, err := store.Heartbeat(foundation.ID, foundation.LeaseToken, relay.NodeClassFoundation, now.Add(time.Minute), 3*time.Minute); err != nil {
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
		if _, err := store.Heartbeat(first.ID, first.LeaseToken, relay.NodeClassVolunteer, now.Add(time.Minute), 3*time.Minute); err != nil {
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
