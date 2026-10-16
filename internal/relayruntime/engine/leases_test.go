// SPDX-License-Identifier: GPL-3.0-or-later

package engine

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"openrung/internal/relay"
	"openrung/internal/relayruntime"
)

// shortLeaseTiming runs the lease syncer's backoff and expiry sweep in
// milliseconds for the test's lifetime.
func shortLeaseTiming(t *testing.T) {
	t.Helper()
	minBackoff, maxBackoff, expiry := leaseSyncMinBackoff, leaseSyncMaxBackoff, leaseExpiryInterval
	leaseSyncMinBackoff, leaseSyncMaxBackoff, leaseExpiryInterval = 20*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { leaseSyncMinBackoff, leaseSyncMaxBackoff, leaseExpiryInterval = minBackoff, maxBackoff, expiry })
}

// leaseEmails returns the lease-* labels xray currently accepts.
func (r *recordingUsers) leaseEmails() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var emails []string
	for email := range r.accepted {
		if strings.HasPrefix(email, relay.LeaseEmailPrefix) {
			emails = append(emails, email)
		}
	}
	return emails
}

// count returns how many adds and removes touched labels with prefix.
func (r *recordingUsers) count(prefix string) (adds, removes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, email := range r.adds {
		if strings.HasPrefix(email, prefix) {
			adds++
		}
	}
	for _, email := range r.removes {
		if strings.HasPrefix(email, prefix) {
			removes++
		}
	}
	return adds, removes
}

func leaseEntry(id, credential string, expiresIn time.Duration) relay.LeaseEntry {
	return relay.LeaseEntry{ID: id, Credential: credential, ExpiresAt: time.Now().UTC().Add(expiresIn)}
}

// A lease-capable session declares the capability, installs the broker's
// set under lease-<id> labels, acknowledges each revision only after xray
// accepted it, and is online only once the first (empty) set is acked.
func TestDirectSessionInstallsAndAcksLeases(t *testing.T) {
	shortRotation(t, time.Hour)
	shortLeaseTiming(t)
	broker := &fakeBroker{leaseCapable: true}
	users := newRecordingUsers()
	eng, _ := startRotatingSession(t, broker, users)

	eventually(t, 5*time.Second, "online", func() bool { return eng.Status().Phase == PhaseOnline })
	_, _, registered := broker.stats()
	if !registered.AccessLeases {
		t.Fatal("a rotating session did not declare access_leases at registration")
	}
	if acked, ok := broker.lastAcked(); !ok || acked != 0 {
		t.Fatalf("online before the first ack: acks %v", broker.ackedRevisions())
	}
	if len(users.leaseEmails()) != 0 {
		t.Fatalf("leases installed from an empty set: %v", users.leaseEmails())
	}

	revision := broker.setLeases(leaseEntry("aaaa", "11111111-2222-4333-8444-555555555551", time.Hour))
	eventually(t, 3*time.Second, "lease installed and acked", func() bool {
		acked, ok := broker.lastAcked()
		return ok && acked == revision
	})
	if !users.has("11111111-2222-4333-8444-555555555551") || len(users.leaseEmails()) != 1 || users.leaseEmails()[0] != "lease-aaaa" {
		t.Fatalf("accepted after install: %v / %v", users.acceptedIDs(), users.leaseEmails())
	}
	// The shared rotating credentials are untouched by lease traffic.
	if _, removes := users.count("cred-"); removes != 0 {
		t.Fatalf("lease sync retired %d shared credentials", removes)
	}
}

// Revocation (a lease absent from the next set) removes exactly that user;
// an expiry-only change updates the local window with no xray call.
func TestDirectSessionReconcilesRevocationsAndExpiryOnlyChanges(t *testing.T) {
	shortRotation(t, time.Hour)
	shortLeaseTiming(t)
	broker := &fakeBroker{leaseCapable: true}
	users := newRecordingUsers()
	eng, _ := startRotatingSession(t, broker, users)
	eventually(t, 5*time.Second, "online", func() bool { return eng.Status().Phase == PhaseOnline })

	a := leaseEntry("aaaa", "11111111-2222-4333-8444-555555555551", time.Hour)
	b := leaseEntry("bbbb", "11111111-2222-4333-8444-555555555552", time.Hour)
	revision := broker.setLeases(a, b)
	eventually(t, 3*time.Second, "both installed", func() bool {
		acked, ok := broker.lastAcked()
		return ok && acked == revision
	})
	if adds, _ := users.count("lease-"); adds != 2 {
		t.Fatalf("lease adds = %d, want 2", adds)
	}

	// Expiry-only change: same credentials, later expiry.
	a2 := a
	a2.ExpiresAt = a.ExpiresAt.Add(time.Hour)
	revision = broker.setLeases(a2, b)
	eventually(t, 3*time.Second, "extension acked", func() bool {
		acked, ok := broker.lastAcked()
		return ok && acked == revision
	})
	if adds, removes := users.count("lease-"); adds != 2 || removes != 0 {
		t.Fatalf("an expiry-only change touched xray: adds %d removes %d", adds, removes)
	}

	// Revocation of b.
	revision = broker.setLeases(a2)
	eventually(t, 3*time.Second, "revocation acked", func() bool {
		acked, ok := broker.lastAcked()
		return ok && acked == revision
	})
	emails := users.leaseEmails()
	if len(emails) != 1 || emails[0] != "lease-aaaa" || users.has(b.Credential) {
		t.Fatalf("after revocation xray accepts %v", users.acceptedIDs())
	}
	if adds, removes := users.count("lease-"); adds != 2 || removes != 1 {
		t.Fatalf("revocation adds %d removes %d, want 2 / 1", adds, removes)
	}
	// Acks are monotonic and each revision was acked at most once.
	acks := broker.ackedRevisions()
	for i := 1; i < len(acks); i++ {
		if acks[i] <= acks[i-1] {
			t.Fatalf("acks not strictly increasing: %v", acks)
		}
	}
}

// Through a broker outage leases keep working until their local expiry,
// which is computed against the broker's server_time so a clock offset
// between the two never shifts the window; then they are removed with no
// broker contact.
func TestDirectSessionExpiresLeasesLocallyDuringOutage(t *testing.T) {
	shortRotation(t, time.Hour)
	shortLeaseTiming(t)
	broker := &fakeBroker{leaseCapable: true}
	users := newRecordingUsers()
	eng, _ := startRotatingSession(t, broker, users)
	eventually(t, 5*time.Second, "online", func() bool { return eng.Status().Phase == PhaseOnline })

	// The fake stamps server_time with its own clock, so an entry expiring
	// 400 ms after now expires 400 ms after the relay saw it.
	short := leaseEntry("cccc", "11111111-2222-4333-8444-555555555553", 400*time.Millisecond)
	long := leaseEntry("dddd", "11111111-2222-4333-8444-555555555554", time.Hour)
	revision := broker.setLeases(short, long)
	eventually(t, 3*time.Second, "installed", func() bool {
		acked, ok := broker.lastAcked()
		return ok && acked == revision
	})
	broker.mu.Lock()
	broker.failLeasePolls = true
	broker.failHeartbeats = true
	broker.mu.Unlock()
	eventually(t, 3*time.Second, "short lease expired locally", func() bool { return !users.has(short.Credential) })
	if !users.has(long.Credential) {
		t.Fatal("the long lease was removed with it")
	}
	if acked, _ := broker.lastAcked(); acked != revision {
		t.Fatalf("local expiry produced an ack: %v", broker.ackedRevisions())
	}
}

// A 404 on the lease channel means the registration is gone: the syncer
// waits for the heartbeat to re-register and continues with the new lease
// token, re-fetching the full set and re-acking it.
func TestDirectSessionLeaseSyncFollowsReRegistration(t *testing.T) {
	shortRotation(t, time.Hour)
	shortLeaseTiming(t)
	broker := &fakeBroker{leaseCapable: true}
	users := newRecordingUsers()
	eng, _ := startRotatingSession(t, broker, users)
	eventually(t, 5*time.Second, "online", func() bool { return eng.Status().Phase == PhaseOnline })
	revision := broker.setLeases(leaseEntry("eeee", "11111111-2222-4333-8444-555555555555", time.Hour))
	eventually(t, 3*time.Second, "installed", func() bool {
		acked, ok := broker.lastAcked()
		return ok && acked == revision
	})

	broker.mu.Lock()
	broker.notFoundOnce = true
	broker.leaseNotFoundOnce = true
	broker.mu.Unlock()
	eventually(t, 5*time.Second, "re-registration", func() bool { return eng.Status().RelayID == "relay_2" })
	eventually(t, 5*time.Second, "lease sync on the new token", func() bool {
		_, bearers := broker.leaseStats()
		return len(bearers) > 0 && bearers[len(bearers)-1] == "lease_relay_2"
	})
	// The full set is re-fetched (no `after`) and re-acked on the new session.
	eventually(t, 5*time.Second, "re-ack after re-registration", func() bool {
		acks := broker.ackedRevisions()
		return len(acks) >= 2 && acks[len(acks)-1] == revision
	})
	if !users.has("11111111-2222-4333-8444-555555555555") {
		t.Fatalf("lease dropped across re-registration: %v", users.acceptedIDs())
	}
}

// A broker that does not echo the capability — or a session that disabled
// leases, or one with no managed xray — never polls, and goes online right
// after registration as before.
func TestDirectSessionSkipsLeasesWithoutCapability(t *testing.T) {
	shortRotation(t, time.Hour)
	t.Run("broker without leases", func(t *testing.T) {
		broker := &fakeBroker{}
		users := newRecordingUsers()
		eng, _ := startRotatingSession(t, broker, users)
		eventually(t, 5*time.Second, "online", func() bool { return eng.Status().Phase == PhaseOnline })
		_, _, registered := broker.stats()
		if !registered.AccessLeases {
			t.Fatal("the session did not offer the capability")
		}
		time.Sleep(100 * time.Millisecond)
		if polls, _ := broker.leaseStats(); polls != 0 {
			t.Fatalf("polled %d times with no capability echo", polls)
		}
	})
	t.Run("leases disabled", func(t *testing.T) {
		broker := &fakeBroker{leaseCapable: true}
		users := newRecordingUsers()
		ts := httptest.NewServer(broker.handler())
		t.Cleanup(ts.Close)
		eng := New(Config{BrokerURL: ts.URL, Mode: ModeDirect, ListenPort: freePort(t), Identity: testIdentity, DisableXray: true, DisableAccessLeases: true, ConfigDir: t.TempDir()}, Events{})
		eng.cfg.HeartbeatInterval = 50 * time.Millisecond
		eng.newXrayUsers = func(Config, string) relayruntime.XrayUserManager { return users }
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = eng.runDirectSession(ctx, &relayruntime.BrokerClient{BaseURL: ts.URL}, eng.cfg, "no-leases", testIdentity, "127.0.0.1", directOnlyListenHost)
		}()
		t.Cleanup(func() { cancel(); <-done })
		eventually(t, 5*time.Second, "online", func() bool { return eng.Status().Phase == PhaseOnline })
		_, _, registered := broker.stats()
		if registered.AccessLeases {
			t.Fatal("a session with leases disabled declared the capability")
		}
		if polls, _ := broker.leaseStats(); polls != 0 {
			t.Fatalf("polled %d times with leases disabled", polls)
		}
	})
	t.Run("static credential", func(t *testing.T) {
		broker := &fakeBroker{leaseCapable: true}
		ts := httptest.NewServer(broker.handler())
		t.Cleanup(ts.Close)
		eng := New(Config{BrokerURL: ts.URL, Mode: ModeDirect, ListenPort: freePort(t), Identity: testIdentity, DisableXray: true, DisableCredentialRotation: true, ConfigDir: t.TempDir()}, Events{})
		eng.cfg.HeartbeatInterval = 50 * time.Millisecond
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = eng.runDirectSession(ctx, &relayruntime.BrokerClient{BaseURL: ts.URL}, eng.cfg, "static", testIdentity, "127.0.0.1", directOnlyListenHost)
		}()
		t.Cleanup(func() { cancel(); <-done })
		eventually(t, 5*time.Second, "online", func() bool { return eng.Status().Phase == PhaseOnline })
		_, _, registered := broker.stats()
		if registered.AccessLeases {
			t.Fatal("a static session declared the capability it cannot honour")
		}
	})
}

// Ending the session removes every installed lease user, so a stopped relay
// leaves no per-client credential behind in a still-running xray.
func TestDirectSessionRemovesLeasesOnStop(t *testing.T) {
	shortRotation(t, time.Hour)
	shortLeaseTiming(t)
	broker := &fakeBroker{leaseCapable: true}
	users := newRecordingUsers()
	eng, cancel := startRotatingSession(t, broker, users)
	eventually(t, 5*time.Second, "online", func() bool { return eng.Status().Phase == PhaseOnline })
	revision := broker.setLeases(leaseEntry("ffff", "11111111-2222-4333-8444-555555555556", time.Hour))
	eventually(t, 3*time.Second, "installed", func() bool {
		acked, ok := broker.lastAcked()
		return ok && acked == revision
	})
	cancel()
	eventually(t, 3*time.Second, "lease removed at stop", func() bool { return len(users.leaseEmails()) == 0 })
}

// A partial xray failure is never acked: the syncer retries with a full poll
// until every user is installed, then acks once.
func TestDirectSessionDoesNotAckPartialInstalls(t *testing.T) {
	shortRotation(t, time.Hour)
	shortLeaseTiming(t)
	broker := &fakeBroker{leaseCapable: true}
	users := newRecordingUsers()
	eng, _ := startRotatingSession(t, broker, users)
	eventually(t, 5*time.Second, "online", func() bool { return eng.Status().Phase == PhaseOnline })

	users.mu.Lock()
	users.failAdds = true
	users.mu.Unlock()
	revision := broker.setLeases(leaseEntry("gggg", "11111111-2222-4333-8444-555555555557", time.Hour))
	time.Sleep(300 * time.Millisecond)
	if acked, _ := broker.lastAcked(); acked == revision {
		t.Fatal("a revision was acked while xray refused the install")
	}
	users.mu.Lock()
	users.failAdds = false
	users.mu.Unlock()
	eventually(t, 3*time.Second, "install retried and acked", func() bool {
		acked, ok := broker.lastAcked()
		return ok && acked == revision && users.has("11111111-2222-4333-8444-555555555557")
	})
}
