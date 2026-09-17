package engine

import (
	"context"
	"errors"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"openrung/internal/relayruntime"
)

// recordingUsers stands in for xray's management API: it keeps the accepted
// credential set and every add/remove in order, and can fail adds on demand.
type recordingUsers struct {
	mu       sync.Mutex
	accepted map[string]relayruntime.Credential
	adds     []string
	removes  []string
	failAdds bool
}

func newRecordingUsers() *recordingUsers {
	return &recordingUsers{accepted: map[string]relayruntime.Credential{}}
}

func (r *recordingUsers) AddUser(_ context.Context, credential relayruntime.Credential) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failAdds {
		return errors.New("management API unavailable")
	}
	r.accepted[credential.Email] = credential
	r.adds = append(r.adds, credential.Email)
	return nil
}

func (r *recordingUsers) RemoveUser(_ context.Context, email string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.accepted, email)
	r.removes = append(r.removes, email)
	return nil
}

func (r *recordingUsers) acceptedIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.accepted))
	for _, credential := range r.accepted {
		ids = append(ids, credential.ID)
	}
	sort.Strings(ids)
	return ids
}

func (r *recordingUsers) has(id string) bool {
	for _, accepted := range r.acceptedIDs() {
		if accepted == id {
			return true
		}
	}
	return false
}

func (r *recordingUsers) removed() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.removes)
}

// shortRotation runs whole rotation buckets in seconds for the test's lifetime.
func shortRotation(t *testing.T, period time.Duration) {
	t.Helper()
	previous := credentialRotationPeriod
	credentialRotationPeriod = period
	t.Cleanup(func() { credentialRotationPeriod = previous })
}

func startRotatingSession(t *testing.T, broker *fakeBroker, users *recordingUsers) (*Engine, context.CancelFunc) {
	t.Helper()
	ts := httptest.NewServer(broker.handler())
	t.Cleanup(ts.Close)
	eng := New(Config{
		BrokerURL:   ts.URL,
		Mode:        ModeDirect,
		Label:       "rotating-relay",
		ListenPort:  freePort(t),
		Identity:    testIdentity,
		DisableXray: true,
		ConfigDir:   t.TempDir(),
	}, Events{})
	eng.cfg.HeartbeatInterval = 50 * time.Millisecond
	eng.newXrayUsers = func(Config, string) relayruntime.XrayUserManager { return users }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = eng.runDirectSession(ctx, &relayruntime.BrokerClient{BaseURL: ts.URL}, eng.cfg, "rotating-relay", testIdentity, "127.0.0.1", directOnlyListenHost)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return eng, cancel
}

// A rotating session registers with a derived credential (never the static
// identity's), keeps the current and previous bucket accepted, announces each
// new bucket on the heartbeat, and retires a bucket only after the broker
// confirms it serves a newer one — so the directory never hands out a
// credential xray has already dropped.
func TestDirectSessionRotatesCredentials(t *testing.T) {
	shortRotation(t, time.Second)
	broker := &fakeBroker{}
	users := newRecordingUsers()
	eng, _ := startRotatingSession(t, broker, users)

	eventually(t, 5*time.Second, "online", func() bool { return eng.Status().Phase == PhaseOnline })
	_, _, registered := broker.stats()
	if registered.ClientID == "" || registered.ClientID == testIdentity.ClientID {
		t.Fatalf("registered client_id = %q, want a derived credential rather than the static %q", registered.ClientID, testIdentity.ClientID)
	}
	if !users.has(registered.ClientID) {
		t.Fatalf("xray does not accept the registered credential %q; accepted %v", registered.ClientID, users.acceptedIDs())
	}
	if got := len(users.acceptedIDs()); got != 2 {
		t.Fatalf("accepted credentials at start = %d, want current + previous", got)
	}

	// Two rotations later the broker serves a newer credential and the first
	// bucket has been retired; through it all the served credential is one
	// xray accepts.
	eventually(t, 6*time.Second, "two rotations", func() bool {
		return users.removed() >= 2
	})
	broker.mu.Lock()
	served := broker.servedClientID
	broker.mu.Unlock()
	if served == registered.ClientID || served == "" {
		t.Fatalf("broker still serves the registration credential after two rotations")
	}
	if !users.has(served) {
		t.Fatalf("broker serves %q, which xray no longer accepts: %v", served, users.acceptedIDs())
	}
	if got := len(users.acceptedIDs()); got > 3 {
		t.Fatalf("accepted credentials grew to %d; want at most current + previous + confirmed", got)
	}
	if users.has(registered.ClientID) {
		t.Fatalf("the registration credential was never retired: %v", users.acceptedIDs())
	}
}

// A broker that predates rotation never echoes client_id. The relay then keeps
// the credential it registered with accepted for the whole session — that is
// the only one such a broker will ever serve — while still deriving new
// buckets, so the accepted set stays bounded.
func TestDirectSessionKeepsRegistrationCredentialForLegacyBroker(t *testing.T) {
	shortRotation(t, time.Second)
	broker := &fakeBroker{legacyHeartbeat: true}
	users := newRecordingUsers()
	eng, _ := startRotatingSession(t, broker, users)

	eventually(t, 5*time.Second, "online", func() bool { return eng.Status().Phase == PhaseOnline })
	_, _, registered := broker.stats()
	eventually(t, 6*time.Second, "two rotations", func() bool { return users.removed() >= 2 })
	if !users.has(registered.ClientID) {
		t.Fatalf("the registration credential %q was retired although the broker never confirmed a successor; accepted %v", registered.ClientID, users.acceptedIDs())
	}
	if got := len(users.acceptedIDs()); got != 3 {
		t.Fatalf("accepted credentials = %d, want current + previous + the registration credential", got)
	}
}

// Re-registration after a pruned lease carries the newest accepted
// credential, and the broker's answer becomes the confirmed one.
func TestDirectSessionReRegistersWithCurrentCredential(t *testing.T) {
	shortRotation(t, time.Hour)
	broker := &fakeBroker{}
	users := newRecordingUsers()
	eng, _ := startRotatingSession(t, broker, users)
	eventually(t, 5*time.Second, "online", func() bool { return eng.Status().Phase == PhaseOnline })

	broker.mu.Lock()
	broker.notFoundOnce = true
	broker.mu.Unlock()
	eventually(t, 5*time.Second, "re-registration", func() bool { return eng.Status().RelayID == "relay_2" })
	_, _, reRegistered := broker.stats()
	if !users.has(reRegistered.ClientID) {
		t.Fatalf("re-registered with %q, which xray does not accept: %v", reRegistered.ClientID, users.acceptedIDs())
	}
	broker.mu.Lock()
	last := broker.lastHeartbeat
	broker.mu.Unlock()
	if last.ClientID == "" || !users.has(last.ClientID) {
		t.Fatalf("heartbeat announced %q, want an accepted credential", last.ClientID)
	}
}

// With rotation disabled the session is exactly the pre-rotation relay: the
// static identity credential is registered and heartbeats carry none.
func TestDirectSessionStaticCredentialWhenRotationDisabled(t *testing.T) {
	broker := &fakeBroker{}
	users := newRecordingUsers()
	ts := httptest.NewServer(broker.handler())
	defer ts.Close()
	eng := New(Config{
		BrokerURL:                 ts.URL,
		Mode:                      ModeDirect,
		Label:                     "static-relay",
		ListenPort:                freePort(t),
		Identity:                  testIdentity,
		DisableXray:               true,
		DisableCredentialRotation: true,
		ConfigDir:                 t.TempDir(),
	}, Events{})
	eng.cfg.HeartbeatInterval = 50 * time.Millisecond
	eng.newXrayUsers = func(Config, string) relayruntime.XrayUserManager { return users }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = eng.runDirectSession(ctx, &relayruntime.BrokerClient{BaseURL: ts.URL}, eng.cfg, "static-relay", testIdentity, "127.0.0.1", directOnlyListenHost)
	}()
	eventually(t, 5*time.Second, "online and heartbeating", func() bool {
		_, hb, _ := broker.stats()
		return eng.Status().Phase == PhaseOnline && hb >= 1
	})
	_, _, registered := broker.stats()
	if registered.ClientID != testIdentity.ClientID {
		t.Fatalf("registered client_id = %q, want the static %q", registered.ClientID, testIdentity.ClientID)
	}
	broker.mu.Lock()
	last := broker.lastHeartbeat
	broker.mu.Unlock()
	if last.ClientID != "" {
		t.Fatalf("a static session announced client_id %q on heartbeat", last.ClientID)
	}
	if len(users.adds) != 0 {
		t.Fatalf("a static session touched the management API: %v", users.adds)
	}
}

// A session whose management API never answers fails instead of registering
// a credential nobody can use.
func TestDirectSessionFailsWhenCredentialsCannotBeInstalled(t *testing.T) {
	previous := xrayAPIStartupTimeout
	xrayAPIStartupTimeout = 300 * time.Millisecond
	t.Cleanup(func() { xrayAPIStartupTimeout = previous })

	broker := &fakeBroker{}
	users := newRecordingUsers()
	users.failAdds = true
	ts := httptest.NewServer(broker.handler())
	defer ts.Close()
	eng := New(Config{
		BrokerURL:   ts.URL,
		Mode:        ModeDirect,
		ListenPort:  freePort(t),
		Identity:    testIdentity,
		DisableXray: true,
		ConfigDir:   t.TempDir(),
	}, Events{})
	eng.newXrayUsers = func(Config, string) relayruntime.XrayUserManager { return users }
	err := eng.runDirectSession(context.Background(), &relayruntime.BrokerClient{BaseURL: ts.URL}, eng.cfg, "failing-relay", testIdentity, "127.0.0.1", directOnlyListenHost)
	if err == nil || !errors.Is(err, errCredentialInstall) {
		t.Fatalf("runDirectSession() error = %v, want a credential install failure", err)
	}
	if regs, _, _ := broker.stats(); regs != 0 {
		t.Fatalf("registered %d times with no usable credential", regs)
	}
}

// While the broker is unreachable no retirement can be confirmed, so the last
// confirmed credential is kept — but only for brokerSilenceGrace. Past it the
// lease and every directory snapshot are long expired, and the relay retires
// it like any stale bucket, so an outage cannot extend a copied credential's
// life or let accepted credentials pile up.
func TestDirectSessionRetiresConfirmedCredentialAfterBrokerSilence(t *testing.T) {
	shortRotation(t, time.Second)
	previousGrace := brokerSilenceGrace
	brokerSilenceGrace = 1500 * time.Millisecond
	t.Cleanup(func() { brokerSilenceGrace = previousGrace })

	broker := &fakeBroker{}
	users := newRecordingUsers()
	eng, _ := startRotatingSession(t, broker, users)
	eventually(t, 5*time.Second, "online", func() bool { return eng.Status().Phase == PhaseOnline })
	_, _, registered := broker.stats()

	broker.mu.Lock()
	broker.failHeartbeats = true
	broker.mu.Unlock()

	eventually(t, 8*time.Second, "registration credential retired after the grace", func() bool {
		return !users.has(registered.ClientID)
	})
	eventually(t, 3*time.Second, "accepted set bounded to current + previous", func() bool {
		return len(users.acceptedIDs()) == 2
	})
}

// -skip-xray-run means an xray this relay does not manage will serve it, so a
// derived credential would be advertised with nothing accepting it: such a
// session serves the static client ID that -print-config-only renders.
func TestDirectSessionServesStaticCredentialWhenXrayIsNotRun(t *testing.T) {
	broker := &fakeBroker{}
	ts := httptest.NewServer(broker.handler())
	defer ts.Close()
	eng := New(Config{
		BrokerURL:   ts.URL,
		Mode:        ModeDirect,
		ListenPort:  freePort(t),
		Identity:    testIdentity,
		DisableXray: true,
		ConfigDir:   t.TempDir(),
	}, Events{})
	eng.cfg.HeartbeatInterval = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = eng.runDirectSession(ctx, &relayruntime.BrokerClient{BaseURL: ts.URL}, eng.cfg, "external-xray", testIdentity, "127.0.0.1", directOnlyListenHost)
	}()
	eventually(t, 5*time.Second, "online and heartbeating", func() bool {
		_, hb, _ := broker.stats()
		return eng.Status().Phase == PhaseOnline && hb >= 1
	})
	_, _, registered := broker.stats()
	if registered.ClientID != testIdentity.ClientID {
		t.Fatalf("registered client_id = %q, want the static %q", registered.ClientID, testIdentity.ClientID)
	}
	broker.mu.Lock()
	last := broker.lastHeartbeat
	broker.mu.Unlock()
	if last.ClientID != "" {
		t.Fatalf("heartbeat announced %q with no managed xray", last.ClientID)
	}
}

// The broker answering 404 (lease gone) and then refusing every
// re-registration is the other outage shape: the re-register failure exits
// must retire like any failed heartbeat, so the confirmed credential still
// dies at the grace and the accepted set stays bounded.
func TestDirectSessionRetiresThroughFailingReRegistration(t *testing.T) {
	shortRotation(t, time.Second)
	previousGrace := brokerSilenceGrace
	brokerSilenceGrace = 1500 * time.Millisecond
	t.Cleanup(func() { brokerSilenceGrace = previousGrace })

	broker := &fakeBroker{}
	users := newRecordingUsers()
	eng, _ := startRotatingSession(t, broker, users)
	eventually(t, 5*time.Second, "online", func() bool { return eng.Status().Phase == PhaseOnline })
	_, _, registered := broker.stats()

	// Every heartbeat from here on is a 404 and every re-registration fails.
	broker.mu.Lock()
	broker.failRegisters = true
	broker.mu.Unlock()
	go func() {
		for {
			broker.mu.Lock()
			broker.notFoundOnce = true
			broker.mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			if eng.Status().Phase == PhaseIdle {
				return
			}
		}
	}()

	eventually(t, 8*time.Second, "registration credential retired after the grace", func() bool {
		return !users.has(registered.ClientID)
	})
	eventually(t, 3*time.Second, "accepted set bounded to current + previous", func() bool {
		return len(users.acceptedIDs()) == 2
	})
	if regs, _, _ := broker.stats(); regs != 1 {
		t.Fatalf("registers = %d, want the initial one only (every re-registration failed)", regs)
	}
}
