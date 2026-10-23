// SPDX-License-Identifier: GPL-3.0-or-later

package engine

import (
	"context"
	"fmt"
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

// A failed first ack is retried until it succeeds, and the relay goes online
// on the retry that lands — never earlier, never not at all.
func TestDirectSessionRetriesFailedFirstAckBeforeGoingOnline(t *testing.T) {
	shortRotation(t, time.Hour)
	shortLeaseTiming(t)
	broker := &fakeBroker{leaseCapable: true, failLeaseAcks: true}
	users := newRecordingUsers()
	eng, _ := startRotatingSession(t, broker, users)

	eventually(t, 5*time.Second, "first poll", func() bool { polls, _ := broker.leaseStats(); return polls >= 1 })
	time.Sleep(300 * time.Millisecond)
	if eng.Status().Phase == PhaseOnline {
		t.Fatal("online while every ack fails")
	}
	broker.mu.Lock()
	broker.failLeaseAcks = false
	broker.mu.Unlock()
	eventually(t, 5*time.Second, "online after the retried ack", func() bool { return eng.Status().Phase == PhaseOnline })
	if acked, ok := broker.lastAcked(); !ok || acked != 0 {
		t.Fatalf("acks = %v", broker.ackedRevisions())
	}

	// The same holds at a higher revision: a set installed under failing
	// acks is acked once acks work again, with no further set change.
	broker.mu.Lock()
	broker.failLeaseAcks = true
	broker.mu.Unlock()
	revision := broker.setLeases(leaseEntry("hhhh", "11111111-2222-4333-8444-555555555558", time.Hour))
	eventually(t, 3*time.Second, "installed", func() bool { return users.has("11111111-2222-4333-8444-555555555558") })
	time.Sleep(200 * time.Millisecond)
	broker.mu.Lock()
	broker.failLeaseAcks = false
	broker.mu.Unlock()
	eventually(t, 5*time.Second, "retried ack", func() bool { acked, _ := broker.lastAcked(); return acked == revision })
}

// A re-registration the lease channel never saw as a 404 (the heartbeat
// re-registered between polls) still restarts synchronisation on the new
// token: the next poll is a full fetch and its set is acked again, so the
// broker's reset applied revision is covered even at an unchanged revision.
func TestDirectSessionReRegistrationWithoutLease404RefetchesAndReacks(t *testing.T) {
	shortRotation(t, time.Hour)
	shortLeaseTiming(t)
	broker := &fakeBroker{leaseCapable: true}
	users := newRecordingUsers()
	eng, _ := startRotatingSession(t, broker, users)
	eventually(t, 5*time.Second, "online", func() bool { return eng.Status().Phase == PhaseOnline })
	revision := broker.setLeases(leaseEntry("iiii", "11111111-2222-4333-8444-555555555559", time.Hour))
	eventually(t, 3*time.Second, "installed", func() bool { acked, _ := broker.lastAcked(); return acked == revision })
	acksBefore := len(broker.ackedRevisions())
	pollsBefore := len(broker.pollAfters())

	// Only the heartbeat sees the 404; lease polls keep succeeding.
	broker.mu.Lock()
	broker.notFoundOnce = true
	broker.mu.Unlock()
	eventually(t, 5*time.Second, "re-registration", func() bool { return eng.Status().RelayID == "relay_2" })
	eventually(t, 5*time.Second, "full poll and ack on the new token", func() bool {
		afters, acks := broker.pollAfters(), broker.ackedRevisions()
		_, bearers := broker.leaseStats()
		fullOnNew := false
		for _, after := range afters[pollsBefore:] {
			fullOnNew = fullOnNew || after < 0
		}
		return fullOnNew && len(acks) > acksBefore && acks[len(acks)-1] == revision && bearers[len(bearers)-1] == "lease_relay_2"
	})
}

// Local expiry is not held up by a slow reconcile: with a management API
// that takes 150 ms per add and a batch of five new leases, an installed
// lease that expires during the batch is removed between operations rather
// than after the whole batch.
func TestDirectSessionExpiresLeasesBetweenSlowReconcileOperations(t *testing.T) {
	shortRotation(t, time.Hour)
	shortLeaseTiming(t)
	broker := &fakeBroker{leaseCapable: true}
	users := newRecordingUsers()
	eng, _ := startRotatingSession(t, broker, users)
	eventually(t, 5*time.Second, "online", func() bool { return eng.Status().Phase == PhaseOnline })

	// Thirty long-lived leases are installed first: the sweep must not wait
	// behind a slow install once per installed entry.
	short := leaseEntry("ssss", "11111111-2222-4333-8444-555555555560", 250*time.Millisecond)
	installed := []relay.LeaseEntry{short}
	for i := 0; i < 30; i++ {
		installed = append(installed, leaseEntry(fmt.Sprintf("l%03d", i), fmt.Sprintf("11111111-2222-4333-8444-5555555556%02d", i), time.Hour))
	}
	revision := broker.setLeases(installed...)
	eventually(t, 3*time.Second, "short lease installed", func() bool { acked, _ := broker.lastAcked(); return acked == revision })

	users.mu.Lock()
	users.addDelay = 150 * time.Millisecond
	users.mu.Unlock()
	batch := installed
	for i := 0; i < 5; i++ {
		batch = append(batch, leaseEntry(fmt.Sprintf("b%03d", i), fmt.Sprintf("11111111-2222-4333-8444-55555555556%d", i+1), time.Hour))
	}
	broker.setLeases(batch...)
	eventually(t, 2*time.Second, "short lease expired mid-batch", func() bool { return !users.has(short.Credential) })
	if adds, _ := users.count("lease-b"); adds >= 5 {
		t.Fatalf("the expiry waited for the whole batch (%d of 5 adds done)", adds)
	}
	eventually(t, 3*time.Second, "batch finished", func() bool { adds, _ := users.count("lease-b"); return adds == 5 })
}

// A withdrawn capability keeps the lease worker alive in drain mode: a
// removal that fails is retried until it succeeds, and the expiry sweep
// keeps running, so no credential is left installed with nothing enforcing
// its end. Re-confirming the capability resumes synchronisation.
func TestDirectSessionDrainsWithRetriesWhenCapabilityWithdrawn(t *testing.T) {
	shortRotation(t, time.Hour)
	shortLeaseTiming(t)
	broker := &fakeBroker{leaseCapable: true}
	users := newRecordingUsers()
	eng, _ := startRotatingSession(t, broker, users)
	eventually(t, 5*time.Second, "online", func() bool { return eng.Status().Phase == PhaseOnline })
	long := leaseEntry("kkkk", "11111111-2222-4333-8444-555555555580", time.Hour)
	short := leaseEntry("mmmm", "11111111-2222-4333-8444-555555555581", 400*time.Millisecond)
	revision := broker.setLeases(long, short)
	eventually(t, 3*time.Second, "installed", func() bool { acked, _ := broker.lastAcked(); return acked == revision })

	users.mu.Lock()
	users.failRemoves = true
	users.mu.Unlock()
	broker.mu.Lock()
	broker.leaseCapable = false
	broker.notFoundOnce = true
	broker.mu.Unlock()
	eventually(t, 5*time.Second, "re-registration without the capability", func() bool { return eng.Status().RelayID == "relay_2" })
	time.Sleep(600 * time.Millisecond)
	if !users.has(long.Credential) || !users.has(short.Credential) {
		t.Fatalf("removals failed yet leases disappeared: %v", users.acceptedIDs())
	}
	polls, _ := broker.leaseStats()

	users.mu.Lock()
	users.failRemoves = false
	users.mu.Unlock()
	eventually(t, 3*time.Second, "drain retried until empty", func() bool { return len(users.leaseEmails()) == 0 })
	time.Sleep(200 * time.Millisecond)
	if after, _ := broker.leaseStats(); after != polls {
		t.Fatalf("drain mode polled the broker (%d → %d)", polls, after)
	}

	// Re-confirmation resumes: a full fetch on the new token, acked.
	acksBefore := len(broker.ackedRevisions())
	broker.mu.Lock()
	broker.leaseCapable = true
	broker.notFoundOnce = true
	broker.mu.Unlock()
	eventually(t, 5*time.Second, "resumed on relay_3", func() bool {
		_, bearers := broker.leaseStats()
		return eng.Status().RelayID == "relay_3" && len(broker.ackedRevisions()) > acksBefore && bearers[len(bearers)-1] == "lease_relay_3"
	})
	eventually(t, 3*time.Second, "set re-installed", func() bool { return users.has(long.Credential) })
}

// A broker upgraded mid-session starts echoing the capability on the next
// re-registration; the syncer starts then, without a relay restart.
func TestDirectSessionStartsLeaseSyncWhenReRegistrationGainsCapability(t *testing.T) {
	shortRotation(t, time.Hour)
	shortLeaseTiming(t)
	broker := &fakeBroker{}
	users := newRecordingUsers()
	eng, _ := startRotatingSession(t, broker, users)
	eventually(t, 5*time.Second, "online", func() bool { return eng.Status().Phase == PhaseOnline })
	time.Sleep(100 * time.Millisecond)
	if polls, _ := broker.leaseStats(); polls != 0 {
		t.Fatalf("polled %d times before the broker understood leases", polls)
	}
	onlineAt := eng.Status().StartedAtMs

	broker.mu.Lock()
	broker.leaseCapable = true
	broker.notFoundOnce = true
	broker.mu.Unlock()
	eventually(t, 5*time.Second, "re-registration", func() bool { return eng.Status().RelayID == "relay_2" })
	eventually(t, 5*time.Second, "lease sync started on the new session", func() bool {
		_, bearers := broker.leaseStats()
		acked, ok := broker.lastAcked()
		return ok && acked == 0 && len(bearers) > 0 && bearers[0] == "lease_relay_2"
	})
	revision := broker.setLeases(leaseEntry("jjjj", "11111111-2222-4333-8444-555555555570", time.Hour))
	eventually(t, 3*time.Second, "lease installed", func() bool {
		acked, _ := broker.lastAcked()
		return acked == revision && users.has("11111111-2222-4333-8444-555555555570")
	})
	if eng.Status().StartedAtMs != onlineAt {
		t.Fatal("a later syncer start reset the online time")
	}

	// And a broker that stops echoing it ends the syncer, which removes the
	// leases it installed.
	broker.mu.Lock()
	broker.leaseCapable = false
	broker.notFoundOnce = true
	broker.mu.Unlock()
	eventually(t, 5*time.Second, "re-registration without the capability", func() bool { return eng.Status().RelayID == "relay_3" })
	eventually(t, 3*time.Second, "leases removed", func() bool { return len(users.leaseEmails()) == 0 })
	polls, _ := broker.leaseStats()
	time.Sleep(300 * time.Millisecond)
	if after, _ := broker.leaseStats(); after != polls {
		t.Fatalf("the syncer kept polling after the capability was withdrawn (%d → %d)", polls, after)
	}
}

// A drain whose removals keep failing backs off even when the session has
// moved past the generation the syncer last polled with (a re-registration
// while leases are off): the session-change shortcut is for polling only.
func TestFailingDrainKeepsItsBackoffAcrossSessionChanges(t *testing.T) {
	minBackoff, maxBackoff, expiry := leaseSyncMinBackoff, leaseSyncMaxBackoff, leaseExpiryInterval
	leaseSyncMinBackoff, leaseSyncMaxBackoff, leaseExpiryInterval = 200*time.Millisecond, time.Second, time.Hour
	t.Cleanup(func() { leaseSyncMinBackoff, leaseSyncMaxBackoff, leaseExpiryInterval = minBackoff, maxBackoff, expiry })

	users := newRecordingUsers()
	users.failRemoves = true
	session := &leaseSession{}
	session.set("relay_1", "lease_relay_1") // generation 1; the syncer never polled it
	syncer := newLeaseSync(&relayruntime.BrokerClient{BaseURL: "http://127.0.0.1:1"}, session, users, time.Hour, func(string, ...any) {})
	syncer.installed["0123456789abcdef0123456789abcdef"] = installedLease{credential: "44444444-2222-4333-8444-555555555555", expiresAt: time.Now().Add(time.Hour)}
	// Inactive from the start: the worker drains, and every removal fails.

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); syncer.run(ctx) }()
	time.Sleep(500 * time.Millisecond)
	session.set("relay_1", "lease_relay_2") // a re-registration while draining
	time.Sleep(500 * time.Millisecond)
	users.mu.Lock()
	attempts := users.removeAttempts
	users.mu.Unlock()
	cancel()
	<-done
	// Backoff 200 ms doubling to 1 s over one second allows a handful of
	// attempts; a loop that skips the backoff makes thousands.
	if attempts > 10 {
		t.Fatalf("%d removal attempts in one second: the failing drain spun without backoff", attempts)
	}
	if attempts == 0 {
		t.Fatal("the drain never tried to remove the lease")
	}
}
