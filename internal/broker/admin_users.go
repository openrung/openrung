package broker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/openrung/openrung/brokerapi"
)

// Operational user endpoints. Same posture as the ranking-weight routes:
// registered without a method, credential checked before anything else, one
// shared operational limiter. The view never includes the public key — the
// operator needs the ID, the state, and the timeline, not key material.

const maxUserStatusBodyBytes = 4 << 10

// userView is the operator-facing record.
type userView struct {
	UserID         string     `json:"user_id"`
	Status         string     `json:"status"`
	CreatedAt      time.Time  `json:"created_at"`
	LastAuthAt     time.Time  `json:"last_auth_at"`
	DisabledAt     *time.Time `json:"disabled_at,omitempty"`
	DisabledReason string     `json:"disabled_reason,omitempty"`
}

func userViewOf(user User) userView {
	view := userView{
		UserID:         user.ID,
		Status:         user.Status,
		CreatedAt:      user.CreatedAt,
		LastAuthAt:     user.LastAuthAt,
		DisabledReason: user.DisabledReason,
	}
	if !user.DisabledAt.IsZero() {
		disabledAt := user.DisabledAt
		view.DisabledAt = &disabledAt
	}
	return view
}

// userStatusRequest is the PUT body: {"status": "disabled", "reason": "…"}.
type userStatusRequest struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// adminUserHandler serves GET /admin/api/users/{id}.
func adminUserHandler(store AccessStore, apiToken string) http.HandlerFunc {
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
		id := r.PathValue("id")
		if !wellFormedUserID(id) {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		user, err := store.UserByID(r.Context(), id)
		if errors.Is(err, ErrUserNotFound) {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		if err != nil {
			slog.Error("could not load user", "user_id", id, "error", err)
			writeError(w, http.StatusServiceUnavailable, "could not load user")
			return
		}
		writeJSON(w, http.StatusOK, userViewOf(user))
	}
}

// userStatusSetter applies an operator decision; accessService.setUserStatus
// is the production implementation, which also wakes the relays whose
// leases a disable revoked.
type userStatusSetter func(ctx context.Context, id, status, reason string) (User, error)

// adminUserStatusHandler serves PUT /admin/api/users/{id}/status. Disabling
// is the revocation lever: a disabled user authenticates but gets no token,
// every later authorization check refuses it, and its live leases are
// revoked so relays drop the credentials on their next sync.
func adminUserStatusHandler(setStatus userStatusSetter, apiToken string, clientIP func(*http.Request) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !bearerMatches(r, apiToken) {
			writeError(w, http.StatusUnauthorized, "missing or invalid operational API token")
			return
		}
		if r.Method != http.MethodPut {
			w.Header().Set("Allow", "PUT")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		id := r.PathValue("id")
		if !wellFormedUserID(id) {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxUserStatusBodyBytes)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var request userStatusRequest
		if err := decoder.Decode(&request); err != nil {
			writeError(w, http.StatusBadRequest, "request body must be a JSON object like {\"status\": \"disabled\", \"reason\": \"…\"}")
			return
		}
		if err := requireJSONEOF(decoder); err != nil {
			writeError(w, http.StatusBadRequest, "request body must contain exactly one JSON object")
			return
		}
		request.Status = strings.ToLower(strings.TrimSpace(request.Status))
		if !validUserStatus(request.Status) {
			writeError(w, http.StatusBadRequest, ErrInvalidUserStatus.Error())
			return
		}
		request.Reason = strings.TrimSpace(request.Reason)
		if len(request.Reason) > maxUserStatusReasonBytes || !storableField(request.Reason) {
			writeError(w, http.StatusBadRequest, "reason must be at most 256 characters of printable text")
			return
		}
		if request.Status == brokerapi.UserStatusEnabled {
			request.Reason = ""
		}

		user, err := setStatus(r.Context(), id, request.Status, request.Reason)
		if errors.Is(err, ErrUserNotFound) {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		if errors.Is(err, ErrInvalidUserStatus) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err != nil {
			slog.Error("could not update user status", "user_id", id, "error", err)
			writeError(w, http.StatusServiceUnavailable, "could not update user status")
			return
		}
		// Every decision is logged, including re-applying the current state:
		// "who disabled this user and when" must always be answerable.
		slog.Info("user status changed", "user_id", id, "status", user.Status, "reason", user.DisabledReason, "client_ip", clientIP(r), "via", "operational api")
		writeJSON(w, http.StatusOK, userViewOf(user))
	}
}
