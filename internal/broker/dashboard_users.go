// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	_ "embed"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Dashboard users page. It reads the same userDirectory as GET
// /admin/api/users and changes status through the same applyUserStatus as
// PUT /admin/api/users/{id}/status; only the credential differs (the session
// cookie, plus dashboardMutationAllowed on the mutation).

//go:embed users.html
var usersHTML []byte

// userLeaseRow is one lease on the user detail: the operational lease view
// plus the relay's label while the relay is registered. Never the credential.
type userLeaseRow struct {
	leaseView
	RelayLabel string `json:"relay_label,omitempty"`
}

// userDetailResponse is the detail panel: the user's directory row (status
// timeline and counters), its most recent retained leases, and how many are
// retained in all.
type userDetailResponse struct {
	GeneratedAt time.Time   `json:"generated_at"`
	User        userListRow `json:"user"`
	// MatchedLease is the lease ID the lookup resolved through, when the
	// search named a lease rather than the user.
	MatchedLease       string         `json:"matched_lease,omitempty"`
	Leases             []userLeaseRow `json:"leases"`
	LeasesTotal        int            `json:"leases_total"`
	LeaseLimit         int            `json:"lease_limit"`
	LeaseRetentionDays int            `json:"lease_retention_days"`
}

func (d *dashboardServer) listUsers(w http.ResponseWriter, r *http.Request) {
	if d.users == nil {
		writeError(w, http.StatusServiceUnavailable, "client users are not configured")
		return
	}
	serveUserList(w, r, d.users, d.now().UTC())
}

// userDetail serves GET /admin/api/dashboard/users/{ref}; ref is a user ID or
// a lease label (see resolveUserRef).
func (d *dashboardServer) userDetail(w http.ResponseWriter, r *http.Request) {
	if d.users == nil {
		writeError(w, http.StatusServiceUnavailable, "client users are not configured")
		return
	}
	now := d.now().UTC()
	userID, leaseID, err := resolveUserRef(r.Context(), d.users, r.PathValue("ref"))
	if errors.Is(err, ErrUserNotFound) {
		writeError(w, http.StatusNotFound, "no user or retained lease matches")
		return
	}
	if err != nil {
		slog.Error("could not resolve user lookup", "error", err)
		writeError(w, http.StatusServiceUnavailable, "could not load user")
		return
	}
	page, err := d.users.ListUsers(r.Context(), UserListQuery{UserID: userID, Sort: UserSortLastAuth, Limit: 1}, now)
	if err != nil {
		slog.Error("could not load user summary", "error", err)
		writeError(w, http.StatusServiceUnavailable, "could not load user")
		return
	}
	if len(page.Users) == 0 {
		writeError(w, http.StatusNotFound, "no user or retained lease matches")
		return
	}
	leases, total, err := d.users.UserLeases(r.Context(), userID, userLeaseHistoryLimit)
	if err != nil {
		slog.Error("could not load user leases", "error", err)
		writeError(w, http.StatusServiceUnavailable, "could not load user leases")
		return
	}
	var displays map[string]relayDisplay
	if d.relayDisplays != nil {
		displays = d.relayDisplays()
	}
	response := userDetailResponse{
		GeneratedAt: now, User: userListRowOf(page.Users[0]), MatchedLease: leaseID,
		Leases: make([]userLeaseRow, 0, len(leases)), LeasesTotal: total,
		LeaseLimit: userLeaseHistoryLimit, LeaseRetentionDays: int(leaseRetention / (24 * time.Hour)),
	}
	for _, lease := range leases {
		response.Leases = append(response.Leases, userLeaseRow{leaseView: leaseViewOf(lease, now), RelayLabel: displays[lease.RelayID].Label})
	}
	writeJSON(w, http.StatusOK, response)
}

// putUserStatus serves PUT /admin/api/dashboard/users/{id}/status.
func (d *dashboardServer) putUserStatus(w http.ResponseWriter, r *http.Request) {
	if status, reason := dashboardMutationAllowed(r); status != 0 {
		writeError(w, status, reason)
		return
	}
	if d.userStatus == nil {
		writeError(w, http.StatusServiceUnavailable, "client users are not configured")
		return
	}
	applyUserStatus(w, r, d.userStatus, d.clientIP(r), "dashboard")
}
