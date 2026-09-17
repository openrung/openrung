package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"openrung/internal/relayruntime"
)

// credentialRotationPeriod is the rotation bucket length. A package variable
// so tests can run whole rotations in seconds.
var credentialRotationPeriod = relayruntime.CredentialRotationPeriod

// xrayAPIStartupTimeout bounds how long the first credential install waits
// for xray to open its management inbound after the process starts. A
// variable so tests can fail fast.
var xrayAPIStartupTimeout = 15 * time.Second

// errCredentialInstall wraps a session failure to make its rotating
// credentials accepted before registering.
var errCredentialInstall = errors.New("install rotating credentials")

// brokerSilenceGrace bounds how long a credential the broker last confirmed
// serving stays accepted without any successful broker contact. Past it the
// relay's lease (3 min) has long expired and every directory snapshot that
// could carry the credential (30 min not_after plus skew) is unusable, so
// nobody legitimate can still hold it and keeping it would only extend a
// copied credential's life for as long as the outage lasts. A variable so
// tests can shorten it.
var brokerSilenceGrace = time.Hour

// credentialRotation drives the relay's rotating VLESS credentials for one
// direct session: which derived credentials xray currently accepts, and which
// one the broker has confirmed it serves.
//
// Invariants the session loop maintains through this type:
//   - the current and previous bucket credentials are registered with xray;
//   - the credential announced to the broker is always one xray accepts;
//   - a credential the broker last confirmed serving is kept while the broker
//     is reachable, so a broker that has not (or cannot — it predates the
//     field) moved on keeps working, and anything the directory may still hand
//     out stays valid; after brokerSilenceGrace without contact it goes too.
type credentialRotation struct {
	schedule   relayruntime.CredentialSchedule
	users      relayruntime.XrayUserManager
	registered map[string]relayruntime.Credential // by email
	confirmed  string
	// lastContact is the last successful registration or heartbeat.
	lastContact time.Time
	logf        func(string, ...any)
}

func newCredentialRotation(identity Identity, epoch string, users relayruntime.XrayUserManager, logf func(string, ...any)) (*credentialRotation, error) {
	var key []byte
	if identityKey, err := identity.identityKey(); err == nil {
		// Derived from the seed: a restart re-derives the same schedule.
		key, err = relayruntime.DeriveCredentialKey(identityKey, epoch)
		if err != nil {
			return nil, err
		}
	} else {
		// No usable seed (prepareIdentity always provides one, so this is a
		// direct caller): the schedule lives as long as the process, like the
		// generated static UUID did.
		key, err = relayruntime.RandomCredentialKey()
		if err != nil {
			return nil, err
		}
	}
	schedule, err := relayruntime.NewCredentialSchedule(key, credentialRotationPeriod)
	if err != nil {
		return nil, err
	}
	return &credentialRotation{
		schedule:   schedule,
		users:      users,
		registered: map[string]relayruntime.Credential{},
		logf:       logf,
	}, nil
}

// install makes the previous and current credentials live, retrying while
// xray's management inbound comes up after process start.
func (c *credentialRotation) install(ctx context.Context, now time.Time) error {
	deadline := time.Now().Add(xrayAPIStartupTimeout)
	for {
		err := c.ensure(ctx, c.schedule.Previous(now))
		if err == nil {
			err = c.ensure(ctx, c.schedule.Current(now))
		}
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// ensure registers credential with xray unless it already is.
func (c *credentialRotation) ensure(ctx context.Context, credential relayruntime.Credential) error {
	if _, ok := c.registered[credential.Email]; ok {
		return nil
	}
	if err := c.users.AddUser(ctx, credential); err != nil {
		return err
	}
	c.registered[credential.Email] = credential
	c.logf("credential %s is now accepted", credential.Email)
	return nil
}

// announce returns the credential the broker should serve: the current bucket's
// once xray accepts it, else the previous bucket's (a boundary add that failed
// is retried on the next heartbeat rather than advertised), else nothing.
// The bucket boundary is the only moment the current credential can be
// missing, so a failed add there is logged once per heartbeat until it heals.
func (c *credentialRotation) announce(ctx context.Context, now time.Time) string {
	current := c.schedule.Current(now)
	if err := c.ensure(ctx, current); err != nil {
		c.logf("could not make credential %s accepted (will retry): %v", current.Email, err)
	}
	if registered, ok := c.registered[current.Email]; ok {
		return registered.ID
	}
	if registered, ok := c.registered[c.schedule.Previous(now).Email]; ok {
		return registered.ID
	}
	return ""
}

// confirm records a successful broker exchange at now and the credential the
// broker reports serving. Empty (a broker that predates the field) keeps the
// last confirmation.
func (c *credentialRotation) confirm(now time.Time, clientID string) {
	c.lastContact = now
	if clientID != "" {
		c.confirmed = clientID
	}
}

// retire removes every registered credential that is neither the current nor
// the previous bucket's nor — while the broker has been reachable within
// brokerSilenceGrace — the one it last confirmed serving. It runs after every
// heartbeat attempt, successful or not, so an outage never lets credentials
// accumulate or outlive the directory snapshots that could carry them.
func (c *credentialRotation) retire(ctx context.Context, now time.Time) {
	current, previous := c.schedule.Current(now), c.schedule.Previous(now)
	keepConfirmed := !c.lastContact.IsZero() && now.Sub(c.lastContact) <= brokerSilenceGrace
	for email, credential := range c.registered {
		if email == current.Email || email == previous.Email || (keepConfirmed && credential.ID == c.confirmed) {
			continue
		}
		if err := c.users.RemoveUser(ctx, email); err != nil {
			c.logf("could not retire credential %s (will retry): %v", email, err)
			continue
		}
		delete(c.registered, email)
		c.logf("credential %s retired", email)
	}
}

// nextRotation is when the current bucket ends.
func (c *credentialRotation) nextRotation(now time.Time) time.Time {
	return c.schedule.NextRotation(now)
}

// accepted lists the credentials xray currently accepts (tests).
func (c *credentialRotation) accepted() []relayruntime.Credential {
	out := make([]relayruntime.Credential, 0, len(c.registered))
	for _, credential := range c.registered {
		out = append(out, credential)
	}
	return out
}

// reserveIPv4LoopbackPort reserves a port on 127.0.0.1 for xray's management
// inbound, which BuildXrayConfig binds there regardless of the relay's own
// loopback family.
func reserveIPv4LoopbackPort() (int, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("reserve xray management port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return 0, err
	}
	if port == 0 {
		return 0, errors.New("reserve xray management port: no port assigned")
	}
	return port, nil
}

// defaultXrayUsers is the production XrayUserManager: the bundled binary's
// `xray api` against the session's management inbound.
func defaultXrayUsers(cfg Config, apiAddr string) relayruntime.XrayUserManager {
	dir := cfg.ConfigDir
	if cfg.ConfigPath != "" {
		dir = ""
	}
	return &relayruntime.XrayAPI{Path: cfg.XrayPath, Addr: apiAddr, Flow: relayFlow, Dir: dir}
}
