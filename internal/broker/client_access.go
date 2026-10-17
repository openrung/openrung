// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
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
// extends the same credentials). A renewal is the same call naming the
// connected relay: the lease is extended and keeps its credential.

const (
	maxAccessRequestBytes = 8 << 10

	// defaultAccessApplyTimeout bounds the wait for relays to install issued
	// leases. A lease-capable relay holds a long poll open, so it normally
	// acknowledges within one round trip.
	defaultAccessApplyTimeout = 5 * time.Second

	// accessRetryAfterSeconds is what a client waits after an unapplied
	// issuance before asking again.
	accessRetryAfterSeconds = 2
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
}

func newClientAccessHandler(store RelayStore, users *userAuthService, access *accessService, s signer) *clientAccessHandler {
	if users == nil || access == nil {
		return nil
	}
	return &clientAccessHandler{store: store, users: users, access: access, signer: s, applyTimeout: defaultAccessApplyTimeout}
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
		page := reserveWSSCandidate(accessCandidates(relays, relayID, country), limit, weights)
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

		applied, err := h.access.awaitApplied(r.Context(), leases, h.applyTimeout)
		if err != nil {
			if r.Context().Err() != nil {
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
		var expiresAt time.Time
		for _, desc := range page {
			lease, ok := byRelay[desc.ID]
			if !ok {
				continue
			}
			desc.ClientID = lease.Credential
			out = append(out, desc)
			if expiresAt.IsZero() || lease.ExpiresAt.Before(expiresAt) {
				expiresAt = lease.ExpiresAt
			}
		}
		h.writeAccess(w, h.access.now().UTC(), out, expiresAt)
	}
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
	h.signer.writeSignedJSON(w, resp)
}

// accessCandidates keeps the ranked relays that can carry a lease — an
// identity-bearing, lease-capable, direct-transport registration — narrowed
// to the target: an exact relay ID, or a country code (case-insensitive, as
// clients match it). Order is preserved; it is the ranking.
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
// revision at or past the lease's, and reports which relays did. Each round
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
