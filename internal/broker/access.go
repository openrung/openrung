// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"openrung/internal/relay"
)

// accessService owns lease issuance and relay lease synchronisation: the
// store writes, the in-process wakeups that make long polls and ack waits
// prompt, and the HTTP handlers relays and operators call. It exists whenever
// the relay store implements AccessStore; the client-facing access endpoint
// that issues leases on behalf of users is layered on it separately.
type accessService struct {
	store    AccessStore
	notifier *leaseNotifier
	leaseTTL time.Duration
	// maxLeasesPerUser / maxLeasesPerRelay are the live-lease caps.
	maxLeasesPerUser  int
	maxLeasesPerRelay int
	// pollHold bounds a long poll with ?after=N.
	pollHold time.Duration
	now      func() time.Time
}

// newAccessService returns nil when the store has no access support, in
// which case no lease route is registered (an old-style relay-only store
// answers 404 on them, like every other optional surface).
func newAccessService(store RelayStore, cfg Config) *accessService {
	access, ok := store.(AccessStore)
	if !ok {
		return nil
	}
	ttl := cfg.AccessLeaseTTL
	if ttl == 0 {
		ttl = defaultAccessLeaseTTL
	}
	if ttl < minAccessLeaseTTL || ttl > maxAccessLeaseTTL {
		panic(fmt.Sprintf("broker: access lease TTL must be within [%s, %s]", minAccessLeaseTTL, maxAccessLeaseTTL))
	}
	perUser, perRelay := cfg.MaxLeasesPerUser, cfg.MaxLeasesPerRelay
	if perUser == 0 {
		perUser = defaultMaxLeasesPerUser
	}
	if perRelay == 0 {
		perRelay = defaultMaxLeasesPerRelay
	}
	if perUser < 1 || perRelay < 1 {
		panic("broker: lease caps must be positive")
	}
	return &accessService{
		store:             access,
		notifier:          newLeaseNotifier(),
		leaseTTL:          ttl,
		maxLeasesPerUser:  perUser,
		maxLeasesPerRelay: perRelay,
		pollHold:          leasePollHold,
		now:               time.Now,
	}
}

// issueLeases issues or extends the user's leases on relayIDs and wakes every
// relay whose desired set changed.
func (a *accessService) issueLeases(ctx context.Context, userID string, relayIDs []string) ([]Lease, error) {
	result, err := a.store.IssueLeases(ctx, LeaseIssueRequest{
		UserID: userID, RelayIDs: relayIDs, TTL: a.leaseTTL,
		MaxPerUser: a.maxLeasesPerUser, MaxPerRelay: a.maxLeasesPerRelay,
	}, a.now().UTC())
	if err != nil {
		return nil, err
	}
	a.notifier.notifyChanged(result.ChangedRelayIDs...)
	return result.Leases, nil
}

// setUserStatus applies an operator decision and wakes the relays whose
// leases a disable revoked.
func (a *accessService) setUserStatus(ctx context.Context, id, status, reason string) (User, error) {
	user, changed, err := a.store.SetUserStatus(ctx, id, status, reason, a.now().UTC())
	if err != nil {
		return User{}, err
	}
	a.notifier.notifyChanged(changed...)
	return user, nil
}

// leaseBearer extracts the relay lease token presented as a bearer.
func leaseBearer(r *http.Request) string {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(token)
}

// leasePollHandler serves GET /api/v1/relays/{id}/leases[?after=N].
//
// Without `after` the relay wants the full set now (first poll of a session,
// or after a 404 re-registration): answer immediately, even an empty set at
// revision 0, so the relay can ack and go online. With `after=N` the relay
// already applied revision N: hold until the revision passes N, the hold
// elapses (then answer with the unchanged set — the relay re-polls, so an
// empty set never busy-loops), or the request ends. Wrong token, unknown
// relay and legacy identityless rows are all 404 "relay not found" so the
// relay's existing re-registration path takes over.
func (a *accessService) leasePollHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		id := r.PathValue("id")
		token := leaseBearer(r)
		if !relay.WellFormedRelayID(id) || token == "" {
			writeError(w, http.StatusNotFound, "relay not found")
			return
		}
		var after int64
		hasAfter := false
		if raw := r.URL.Query().Get("after"); raw != "" {
			parsed, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || parsed < 0 {
				writeError(w, http.StatusBadRequest, "after must be a non-negative integer revision")
				return
			}
			after, hasAfter = parsed, true
		}

		// readSet is one authenticated snapshot; it writes the error response
		// itself and reports whether the handler should return.
		readSet := func() (RelayLeaseSet, time.Time, bool) {
			now := a.now().UTC()
			set, err := a.store.RelayLeaseSet(r.Context(), id, token, now)
			if errors.Is(err, ErrRelayNotFound) {
				writeError(w, http.StatusNotFound, "relay not found")
				return RelayLeaseSet{}, now, false
			}
			if err != nil {
				slog.Error("could not read relay lease set", "relay_id", id, "error", err)
				writeError(w, http.StatusServiceUnavailable, "could not read relay leases")
				return RelayLeaseSet{}, now, false
			}
			return set, now, true
		}

		// The first read authenticates the request before any waiter state
		// is allocated, so unknown relays and wrong tokens retain nothing.
		set, now, ok := readSet()
		if !ok {
			return
		}
		deadline := time.Now().Add(a.pollHold)
		for {
			if !hasAfter || set.Revision > after || !time.Now().Before(deadline) {
				writeJSON(w, http.StatusOK, leaseSyncResponse(set, now))
				return
			}
			// Subscribe, then re-read: a change landing between the read above
			// and the subscription shows up in this read, and one landing after
			// the subscription closes the channel we wait on. The subscription
			// is released on every exit so an abandoned hold leaves no entry.
			wake := a.notifier.changedC(id)
			set, now, ok = readSet()
			if !ok {
				wake.release()
				return
			}
			if set.Revision > after {
				wake.release()
				continue
			}
			hold := time.NewTimer(time.Until(deadline))
			select {
			case <-wake.C:
				hold.Stop()
			case <-hold.C:
			case <-r.Context().Done():
				hold.Stop()
				wake.release()
				return
			}
			wake.release()
			set, now, ok = readSet()
			if !ok {
				return
			}
		}
	}
}

func leaseSyncResponse(set RelayLeaseSet, now time.Time) relay.LeaseSyncResponse {
	entries := make([]relay.LeaseEntry, 0, len(set.Leases))
	for _, lease := range set.Leases {
		entries = append(entries, relay.LeaseEntry{ID: lease.ID, Credential: lease.Credential, ExpiresAt: lease.ExpiresAt})
	}
	return relay.LeaseSyncResponse{ServerTime: now, Revision: set.Revision, Leases: entries}
}

// leaseAckHandler serves POST /api/v1/relays/{id}/leases/ack {revision}.
func (a *accessService) leaseAckHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		id := r.PathValue("id")
		token := leaseBearer(r)
		if !relay.WellFormedRelayID(id) || token == "" {
			writeError(w, http.StatusNotFound, "relay not found")
			return
		}
		var ack relay.LeaseAckRequest
		r.Body = http.MaxBytesReader(w, r.Body, maxRegisterBodyBytes)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&ack); err != nil {
			writeError(w, http.StatusBadRequest, "request body must be a JSON object like {\"revision\": 7}")
			return
		}
		if err := requireJSONEOF(decoder); err != nil {
			writeError(w, http.StatusBadRequest, "request body must contain exactly one JSON object")
			return
		}
		if ack.Revision < 0 {
			writeError(w, http.StatusBadRequest, "revision must be a non-negative integer")
			return
		}
		err := a.store.AckLeaseRevision(r.Context(), id, token, ack.Revision, a.now().UTC())
		if errors.Is(err, ErrRelayNotFound) {
			writeError(w, http.StatusNotFound, "relay not found")
			return
		}
		if err != nil {
			slog.Error("could not record relay lease ack", "relay_id", id, "error", err)
			writeError(w, http.StatusServiceUnavailable, "could not record lease ack")
			return
		}
		a.notifier.notifyApplied(id)
		w.WriteHeader(http.StatusNoContent)
	}
}

// leaseView is the operator-facing lease record: enough to go from a relay
// log label to the user and the window, never the credential.
type leaseView struct {
	LeaseID   string     `json:"lease_id"`
	Label     string     `json:"label"`
	UserID    string     `json:"user_id"`
	RelayID   string     `json:"relay_id"`
	Status    string     `json:"status"`
	CreatedAt time.Time  `json:"created_at"`
	RenewedAt time.Time  `json:"renewed_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	Revision  int64      `json:"revision"`
}

func leaseViewOf(lease Lease, now time.Time) leaseView {
	view := leaseView{
		LeaseID: lease.ID, Label: lease.Email(), UserID: lease.UserID, RelayID: lease.RelayID,
		CreatedAt: lease.CreatedAt, RenewedAt: lease.RenewedAt, ExpiresAt: lease.ExpiresAt, Revision: lease.Revision,
	}
	switch {
	case !lease.RevokedAt.IsZero():
		revokedAt := lease.RevokedAt
		view.RevokedAt = &revokedAt
		view.Status = "revoked"
	case !lease.ExpiresAt.After(now):
		view.Status = "expired"
	default:
		view.Status = "live"
	}
	return view
}

// adminLeaseHandler serves GET /admin/api/leases/{id}; id may be the bare
// lease ID or the relay-log label ("lease-<id>").
func (a *accessService) adminLeaseHandler(apiToken string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !bearerMatches(r, apiToken) {
			writeError(w, http.StatusUnauthorized, "missing or invalid operational API token")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		id := strings.TrimPrefix(r.PathValue("id"), relay.LeaseEmailPrefix)
		if !wellFormedLeaseID(id) {
			writeError(w, http.StatusNotFound, "lease not found")
			return
		}
		lease, err := a.store.LeaseByID(r.Context(), id)
		if errors.Is(err, ErrLeaseNotFound) {
			writeError(w, http.StatusNotFound, "lease not found")
			return
		}
		if err != nil {
			slog.Error("could not load lease", "lease_id", id, "error", err)
			writeError(w, http.StatusServiceUnavailable, "could not load lease")
			return
		}
		writeJSON(w, http.StatusOK, leaseViewOf(lease, a.now().UTC()))
	}
}
