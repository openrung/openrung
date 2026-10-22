package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
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
		applyUserStatus(w, r, setStatus, clientIP(r), "operational api")
	}
}

// applyUserStatus is the one implementation behind both doors — the
// operational API and the dashboard's users page: it vets the user ID from
// the {id} path segment and the body, applies the decision, logs it, and
// answers with the updated view. via names the door for the log line.
func applyUserStatus(w http.ResponseWriter, r *http.Request, setStatus userStatusSetter, clientIP, via string) {
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
	slog.Info("user status changed", "user_id", id, "status", user.Status, "reason", user.DisabledReason, "client_ip", clientIP, "via", via)
	writeJSON(w, http.StatusOK, userViewOf(user))
}

// userListRow is one directory row: the operator view plus the lease
// counters. Like userView it never carries the public key.
type userListRow struct {
	userView
	LiveLeases      int `json:"live_leases"`
	LeasesIssued24h int `json:"leases_issued_24h"`
	Relays24h       int `json:"relays_24h"`
}

func userListRowOf(entry UserListEntry) userListRow {
	return userListRow{
		userView: userViewOf(User{
			ID: entry.ID, Status: entry.Status, CreatedAt: entry.CreatedAt, LastAuthAt: entry.LastAuthAt,
			DisabledAt: entry.DisabledAt, DisabledReason: entry.DisabledReason,
		}),
		LiveLeases:      entry.LiveLeases,
		LeasesIssued24h: entry.LeasesIssued24h,
		Relays24h:       entry.Relays24h,
	}
}

// userListResponse is one page of the directory and the filter's total.
type userListResponse struct {
	GeneratedAt time.Time     `json:"generated_at"`
	Status      string        `json:"status,omitempty"`
	Sort        string        `json:"sort"`
	Total       int           `json:"total"`
	Offset      int           `json:"offset"`
	Limit       int           `json:"limit"`
	Users       []userListRow `json:"users"`
}

// parseUserListQuery reads ?status=&sort=&offset=&limit=; the message names
// the offending parameter.
func parseUserListQuery(values url.Values) (UserListQuery, string) {
	q := UserListQuery{Status: strings.ToLower(strings.TrimSpace(values.Get("status"))), Sort: values.Get("sort")}
	if q.Status != "" && !validUserStatus(q.Status) {
		return UserListQuery{}, "status must be enabled or disabled"
	}
	if q.Sort == "" {
		q.Sort = UserSortLastAuth
	}
	if !validUserSort(q.Sort) {
		return UserListQuery{}, "sort must be last_auth, leases_24h or enrolled"
	}
	var err error
	if q.Offset, err = queryInt(values.Get("offset"), 0); err != nil || q.Offset < 0 {
		return UserListQuery{}, "offset must be a non-negative integer"
	}
	if q.Limit, err = queryInt(values.Get("limit"), userListDefaultLimit); err != nil || q.Limit < 1 || q.Limit > userListMaxLimit {
		return UserListQuery{}, fmt.Sprintf("limit must be between 1 and %d", userListMaxLimit)
	}
	return q, ""
}

// serveUserList is the one list implementation behind both doors.
func serveUserList(w http.ResponseWriter, r *http.Request, directory userDirectory, now time.Time) {
	q, problem := parseUserListQuery(r.URL.Query())
	if problem != "" {
		writeError(w, http.StatusBadRequest, problem)
		return
	}
	page, err := directory.ListUsers(r.Context(), q, now)
	if err != nil {
		slog.Error("could not list users", "error", err)
		writeError(w, http.StatusServiceUnavailable, "could not list users")
		return
	}
	response := userListResponse{GeneratedAt: now, Status: q.Status, Sort: q.Sort, Total: page.Total, Offset: q.Offset, Limit: q.Limit, Users: make([]userListRow, 0, len(page.Users))}
	for _, entry := range page.Users {
		response.Users = append(response.Users, userListRowOf(entry))
	}
	writeJSON(w, http.StatusOK, response)
}

// adminUserListHandler serves GET /admin/api/users to the operational token.
func adminUserListHandler(directory userDirectory, apiToken string) http.HandlerFunc {
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
		serveUserList(w, r, directory, time.Now().UTC())
	}
}
