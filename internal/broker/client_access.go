// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openrung/openrung/brokerapi"

	"openrung/internal/relay"
)

// Client access: POST /api/v1/access (ADR-006).
//
// An authenticated user asks for relays. The broker picks them exactly as the
// public list does (ranking, the WSS reservation), restricted to relays that
// can install per-client credentials and to the request's target, issues or
// extends one lease per relay, waits until each relay has acknowledged the
// revision carrying its lease, and answers with a signed relay list on the
// access channel whose client_id values are the lease credentials. Relays that
// do not acknowledge in time are left out; when none did, the answer is 503
// with Retry-After so the client retries (the leases persist, so the retry
// extends the same credentials) — unless a relay that has not acknowledged
// is still heartbeating, in which case the wait is extended once first (see
// awaitIssued). A renewal is the same call naming the connected relay: the
// lease is extended and keeps its credential; a connected relay that has left
// the directory yields an empty 200 so the client reconnects at once.

const (
	maxAccessRequestBytes = 8 << 10

	// defaultAccessApplyTimeout bounds the wait for relays to install issued
	// leases. A lease-capable relay holds a long poll open, so it normally
	// acknowledges within one round trip. When nothing acknowledged but a
	// relay is demonstrably alive the wait runs once more, so a request
	// takes at most twice this — well inside the fronts' origin timeouts.
	defaultAccessApplyTimeout = 5 * time.Second

	// accessRetryAfterSeconds is what a client waits after an unapplied
	// issuance before asking again.
	accessRetryAfterSeconds = 2

	// relayHeartbeatInterval is the direct-mode relay's default broker
	// heartbeat cadence (internal/relayruntime/engine Config.HeartbeatInterval).
	// A relay whose last heartbeat is older than this has missed one: it is
	// treated as gone rather than slow, and nothing waits for it.
	relayHeartbeatInterval = 30 * time.Second

	// accessSummaryInterval spaces the periodic INFO summary of access
	// answers, so access health is readable from the broker log after the
	// short-lived origin access log has rolled over.
	accessSummaryInterval = 10 * time.Minute
)

// accessResponse is the access-channel body: the public relay-list envelope
// plus the lease expiry shared by every returned credential. The embedded
// ListResponse has no marshaler of its own, so its fields are promoted and
// each descriptor still marshals through relay.Descriptor's.
type accessResponse struct {
	relay.ListResponse
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
}

// clientAccessHandler holds what POST /api/v1/access needs: the token codec
// from the user service, the lease service and the relay-list signer.
type clientAccessHandler struct {
	store        RelayStore
	users        *userAuthService
	access       *accessService
	signer       signer
	applyTimeout time.Duration
	stats        *accessStats
	// withholdUnreachable is whether reachability probing is enabled, and so
	// whether a stored failed probe excludes a relay (see reachability.go).
	withholdUnreachable bool
	// clientIP resolves the address the access request came from (through a
	// trusted front, the viewer's). Nil, like an unparseable address, counts
	// as IPv4, so no relay's access host is handed out.
	clientIP func(*http.Request) string
}

func newClientAccessHandler(store RelayStore, users *userAuthService, access *accessService, s signer) *clientAccessHandler {
	if users == nil || access == nil {
		return nil
	}
	return &clientAccessHandler{
		store: store, users: users, access: access, signer: s,
		applyTimeout: defaultAccessApplyTimeout,
		stats:        users.stats,
	}
}

// accessStats counts access answers by outcome, and enrollments refused by
// source classification, and emits one INFO summary line per interval.
// Counting is driven by the requests themselves (the summary is written by
// the first counted request after the interval has elapsed, covering the
// window since the previous one), so it needs no goroutine and a broker with
// no such traffic logs nothing. The counts are aggregates only: no user,
// relay, address or network appears in the line.
type accessStats struct {
	every time.Duration

	mu    sync.Mutex
	since time.Time
	accessCounts
}

// accessCounts are one window's counters. granted / empty split the 200
// answers by whether they carried relays; the rest are keyed by status.
// extended counts answers that waited a second time for a slow relay
// (whatever they then answered), cancelled the requests the client abandoned
// before an answer was written. enrollDatacenter and enrollUnclassified
// count user authentications that refused a new identity by its source
// network (403) or for want of classification data (503).
type accessCounts struct {
	granted, empty, badRequest, unauthorized, denied, rateLimited int
	unapplied, failed, other, cancelled, extended                 int
	enrollDatacenter, enrollUnclassified                          int
}

func (s *accessStats) add(now time.Time, bump func(*accessStats)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.since.IsZero() {
		s.since = now
	}
	bump(s)
	if now.Sub(s.since) < s.every {
		return
	}
	slog.Info("access requests",
		"window", now.Sub(s.since).Round(time.Second).String(),
		"granted", s.granted, "empty", s.empty, "unapplied", s.unapplied, "extended", s.extended,
		"denied", s.denied, "unauthorized", s.unauthorized, "rate_limited", s.rateLimited,
		"bad_request", s.badRequest, "cancelled", s.cancelled, "error", s.failed, "other", s.other,
		"enroll_refused_datacenter", s.enrollDatacenter, "enroll_unclassified", s.enrollUnclassified)
	s.accessCounts, s.since = accessCounts{}, now
}

// statusRecorder remembers the status a handler wrote; zero means it wrote
// nothing (the client went away first).
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// counted wraps the complete access route (rate limiter included) so every
// non-200 answer is counted by status, 200s being counted where they are
// written (writeAccess knows whether relays were returned).
func (h *clientAccessHandler) counted(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		next(rec, r)
		h.stats.add(h.access.now(), func(s *accessStats) {
			switch {
			case rec.status == 0:
				s.cancelled++
			case rec.status == http.StatusOK:
			case rec.status == http.StatusBadRequest:
				s.badRequest++
			case rec.status == http.StatusUnauthorized:
				s.unauthorized++
			case rec.status == http.StatusForbidden:
				s.denied++
			case rec.status == http.StatusTooManyRequests:
				s.rateLimited++
			case rec.status == http.StatusServiceUnavailable && rec.Header().Get("Retry-After") != "":
				s.unapplied++
			case rec.status >= 500:
				s.failed++
			default:
				s.other++
			}
		})
	}
}

// requestCancelled reports whether an access request failed because the
// client abandoned it: the request context is done, or the failure wraps the
// cancellation. Such a failure is the client's doing, not the broker's, and
// is logged at INFO so the error log keeps meaning store failures.
func requestCancelled(r *http.Request, err error) bool {
	return r.Context().Err() != nil || errors.Is(err, context.Canceled)
}

// rateKey budgets per client address and user: users behind one carrier-
// grade NAT address do not share a bucket, while a stolen token still buys
// only one user's budget per address. An invalid token falls back to the
// address alone. The user ID is hashed so the limiter retains no identifier.
func (h *clientAccessHandler) rateKey(clientIP *clientIPResolver) func(*http.Request) string {
	return func(r *http.Request) string {
		source := clientIP.clientIP(r)
		userID, ok := h.users.tokens.verify(leaseBearer(r), h.access.now())
		if !ok {
			return source
		}
		digest := sha256.Sum256([]byte(userID))
		return source + "\x00" + hex.EncodeToString(digest[:16])
	}
}

func (h *clientAccessHandler) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		now := h.access.now().UTC()
		userID, ok := h.users.tokens.verify(leaseBearer(r), now)
		if !ok {
			writeError(w, http.StatusUnauthorized, "missing, invalid or expired access token")
			return
		}

		var request brokerapi.AccessRequest
		r.Body = http.MaxBytesReader(w, r.Body, maxAccessRequestBytes)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid access request JSON")
			return
		}
		if err := requireJSONEOF(decoder); err != nil {
			writeError(w, http.StatusBadRequest, "access request must contain one JSON object")
			return
		}
		limit := request.Limit
		if limit == 0 {
			limit = brokerapi.MaxAccessRelays
		}
		if limit < 1 || limit > brokerapi.MaxAccessRelays {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and "+strconv.Itoa(brokerapi.MaxAccessRelays))
			return
		}
		// One request never asks for more leases than the user may hold.
		limit = min(limit, h.access.maxLeasesPerUser)
		relayID := strings.TrimSpace(request.RelayID)
		country := strings.TrimSpace(request.Country)
		if relayID != "" && country != "" {
			writeError(w, http.StatusBadRequest, "relay_id and country are mutually exclusive")
			return
		}
		if country != "" && (len(country) != 2 || !asciiLetters(country)) {
			writeError(w, http.StatusBadRequest, "country must be a two-letter country code")
			return
		}

		user, err := h.users.store.UserByID(r.Context(), userID)
		if errors.Is(err, ErrUserNotFound) {
			writeError(w, http.StatusUnauthorized, "missing, invalid or expired access token")
			return
		}
		if err != nil {
			slog.Error("could not load user for access", "error", err)
			writeError(w, http.StatusServiceUnavailable, "could not authorize access")
			return
		}
		if user.Status != brokerapi.UserStatusEnabled {
			writeError(w, http.StatusForbidden, "user is disabled")
			return
		}

		relays, weights, err := h.store.ListRanked(now, 0)
		if err != nil {
			slog.Error("could not list relays for access", "error", err)
			writeError(w, http.StatusServiceUnavailable, "could not list relays")
			return
		}
		page := reserveWSSCandidate(accessCandidates(withoutUnreachable(relays, h.withholdUnreachable), relayID, country), limit, weights)
		if len(page) == 0 || (relayID != "" && !relay.WellFormedRelayID(relayID)) {
			h.writeAccess(w, now, nil, time.Time{})
			return
		}

		ids := make([]string, len(page))
		for i, desc := range page {
			ids[i] = desc.ID
		}
		leases, err := h.access.issueLeases(r.Context(), userID, ids)
		switch {
		case errors.Is(err, ErrUserDisabled):
			writeError(w, http.StatusForbidden, "user is disabled")
			return
		case errors.Is(err, ErrUserNotFound):
			writeError(w, http.StatusUnauthorized, "missing, invalid or expired access token")
			return
		case requestCancelled(r, err):
			// The client's doing, not a store failure. A caller that is
			// somehow still connected gets the ordinary retryable refusal.
			slog.Info("access request cancelled by the client before issuance", "error", err)
			if r.Context().Err() == nil {
				writeError(w, http.StatusServiceUnavailable, "could not issue access")
			}
			return
		case err != nil:
			slog.Error("could not issue access leases", "user_id", userID, "error", err)
			writeError(w, http.StatusServiceUnavailable, "could not issue access")
			return
		}
		if len(leases) == 0 {
			// Every candidate relay was at its lease capacity.
			h.writeAccess(w, now, nil, time.Time{})
			return
		}

		applied, err := h.awaitIssued(r.Context(), leases)
		if err != nil {
			if requestCancelled(r, err) {
				slog.Info("access request cancelled by the client before relays acknowledged", "error", err)
				if r.Context().Err() == nil {
					writeError(w, http.StatusServiceUnavailable, "could not confirm access")
				}
				return
			}
			slog.Error("could not read relay lease acknowledgements", "error", err)
			writeError(w, http.StatusServiceUnavailable, "could not confirm access")
			return
		}
		byRelay := make(map[string]Lease, len(leases))
		for _, lease := range leases {
			if applied[lease.RelayID] {
				byRelay[lease.RelayID] = lease
			}
		}
		if len(byRelay) == 0 {
			w.Header().Set("Retry-After", strconv.Itoa(accessRetryAfterSeconds))
			writeError(w, http.StatusServiceUnavailable, "relays have not installed the issued access yet, retry shortly")
			return
		}
		out := make([]relay.Descriptor, 0, len(byRelay))
		overIPv6 := requestOverIPv6(h.clientIP, r)
		var expiresAt time.Time
		for _, desc := range page {
			lease, ok := byRelay[desc.ID]
			if !ok {
				continue
			}
			desc.ClientID = lease.Credential
			// A relay with an access address is reached there by enrolled
			// clients (ADR-006.1 Part A); the public directory keeps naming
			// its public host. Nothing else in the descriptor changes. The
			// access address is IPv6, so it is handed only to a client whose
			// request arrived over IPv6; any other client keeps the public
			// host it can reach.
			if desc.AccessHost != "" && overIPv6 {
				desc.PublicHost = desc.AccessHost
			}
			out = append(out, desc)
			if expiresAt.IsZero() || lease.ExpiresAt.Before(expiresAt) {
				expiresAt = lease.ExpiresAt
			}
		}
		h.writeAccess(w, h.access.now().UTC(), out, expiresAt)
	}
}

// requestOverIPv6 reports whether the request came from an IPv6 address.
// An IPv4-mapped address is IPv4, and an unknown resolver or unparseable
// address counts as IPv4.
func requestOverIPv6(clientIP func(*http.Request) string, r *http.Request) bool {
	if clientIP == nil {
		return false
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(clientIP(r)))
	return err == nil && addr.Unmap().Is6()
}

// writeAccess signs and writes the access body. Timestamps are narrowed to
// whole seconds like every signed relay list; the lease expiry is truncated
// too, which only ever makes it earlier than the broker's record.
func (h *clientAccessHandler) writeAccess(w http.ResponseWriter, now time.Time, relays []relay.Descriptor, leaseExpiresAt time.Time) {
	if relays == nil {
		relays = []relay.Descriptor{}
	}
	resp := accessResponse{ListResponse: normalizeSignedRelayListTimes(relay.ListResponse{
		Count:      len(relays),
		ServerTime: now,
		NotAfter:   now.Add(apiNotAfterWindow),
		KeyID:      h.signer.keyID,
		Channel:    brokerapi.ChannelAccess,
		Relays:     relays,
	})}
	if !leaseExpiresAt.IsZero() {
		expiry := leaseExpiresAt.UTC().Truncate(time.Second)
		resp.LeaseExpiresAt = &expiry
	}
	h.stats.add(now, func(s *accessStats) {
		if len(relays) == 0 {
			s.empty++
		} else {
			s.granted++
		}
	})
	h.signer.writeSignedJSON(w, resp)
}

// awaitIssued waits for the relays holding the issued leases to acknowledge
// them and reports which did. Whatever subset acknowledged within the apply
// timeout is the answer. When none did, the relays are re-read: one whose
// heartbeat is still current is alive and merely slow (a long poll in flight,
// a busy xray), so the wait runs once more — bounded to one extension — while
// a relay that has missed a heartbeat (or has expired from the directory
// meanwhile) is most likely dead, and waiting for it would only delay the
// client's retry or fallback. A lookup that fails for any reason other than
// the relay being gone is a store failure and is returned as such: the caller
// logs it and answers the generic 503, never a quiet "unapplied".
func (h *clientAccessHandler) awaitIssued(ctx context.Context, leases []Lease) (map[string]bool, error) {
	applied, err := h.access.awaitApplied(ctx, leases, h.applyTimeout)
	if err != nil || len(applied) > 0 {
		return applied, err
	}
	now := h.access.now().UTC()
	alive, err := h.anyRelayAlive(leases, now)
	if err != nil || !alive {
		return applied, err
	}
	h.stats.add(now, func(s *accessStats) { s.extended++ })
	return h.access.awaitApplied(ctx, leases, h.applyTimeout)
}

// anyRelayAlive reports whether any of the leases' relays heartbeated within
// the last heartbeat interval. A relay gone from the directory is simply not
// alive; any other lookup failure is returned, so the first failing lookup
// ends the check rather than each relay waiting out its own store timeout.
func (h *clientAccessHandler) anyRelayAlive(leases []Lease, now time.Time) (bool, error) {
	for _, lease := range leases {
		desc, err := h.store.RelayByID(lease.RelayID, now)
		if errors.Is(err, ErrRelayNotFound) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("read relay %s for the acknowledgement wait: %w", lease.RelayID, err)
		}
		if now.Sub(desc.LastHeartbeatAt) <= relayHeartbeatInterval {
			return true, nil
		}
	}
	return false, nil
}

// accessCandidates keeps the ranked relays that can carry a lease — an
// identity-bearing, lease-capable, direct-transport registration — narrowed
// to the target: an exact relay ID, or a country code (case-insensitive, as
// clients match it). Order is preserved; it is the ranking. The caller has
// already applied withoutUnreachable.
func accessCandidates(relays []relay.Descriptor, relayID, country string) []relay.Descriptor {
	out := make([]relay.Descriptor, 0, len(relays))
	for _, desc := range relays {
		if !desc.LeaseCapable || desc.IdentityPublicKey == "" || desc.Transport != relay.TransportDirect {
			continue
		}
		if relayID != "" && desc.ID != relayID {
			continue
		}
		if country != "" && !strings.EqualFold(strings.TrimSpace(desc.CountryCode), country) {
			continue
		}
		out = append(out, desc)
	}
	return out
}

func asciiLetters(value string) bool {
	for _, c := range []byte(value) {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

// awaitApplied waits up to timeout for each lease's relay to acknowledge a
// revision at or past the lease's, and reports which relays did (the map
// holds only those; a relay that did not acknowledge is absent). Each round
// subscribes before reading so an ack landing between the read and the wait
// still wakes it; every subscription is released on the way out.
func (a *accessService) awaitApplied(ctx context.Context, leases []Lease, timeout time.Duration) (map[string]bool, error) {
	want := make(map[string]int64, len(leases))
	for _, lease := range leases {
		if lease.Revision > want[lease.RelayID] {
			want[lease.RelayID] = lease.Revision
		}
	}
	applied := make(map[string]bool, len(want))
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	// final is set once the deadline fires: one last read still counts an
	// ack that landed just before it.
	final := false
	for {
		pending := make([]string, 0, len(want))
		for id := range want {
			if !applied[id] {
				pending = append(pending, id)
			}
		}
		subs := make([]leaseSubscription, len(pending))
		for i, id := range pending {
			subs[i] = a.notifier.appliedC(id)
		}
		release := func() {
			for _, sub := range subs {
				sub.release()
			}
		}
		revisions, err := a.store.AppliedLeaseRevisions(ctx, pending)
		if err != nil {
			release()
			return nil, err
		}
		waiting := false
		for _, id := range pending {
			if revisions[id] >= want[id] {
				applied[id] = true
			} else {
				waiting = true
			}
		}
		if !waiting || final {
			release()
			return applied, nil
		}
		wake := make(chan struct{}, 1)
		stop := make(chan struct{})
		for _, sub := range subs {
			go func(c <-chan struct{}) {
				select {
				case <-c:
					select {
					case wake <- struct{}{}:
					default:
					}
				case <-stop:
				}
			}(sub.C)
		}
		select {
		case <-wake:
		case <-deadline.C:
			final = true
		case <-ctx.Done():
			close(stop)
			release()
			return nil, ctx.Err()
		}
		close(stop)
		release()
	}
}
