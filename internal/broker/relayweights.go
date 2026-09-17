package broker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"openrung/internal/relay"
)

// Operator ranking weights are the broker-side dial for shifting client load
// between relays without touching the relays themselves: a per-relay
// multiplier in [0, 1] applied last in relayScore. Weight 1 (the default) is
// the pure telemetry ranking; smaller values demote a relay in proportion; 0
// pins it to the bottom of every page without delisting it, which drains a
// relay while still letting a client with no other reachable candidate use it.
//
// Weights key on the identity-derived relay ID and live outside descriptor
// pruning, so they survive lease expiry and re-registration. They can be set
// through two doors that share one store: the token-gated operational API
// (fleet tooling, curl) and the cookie-gated telemetry dashboard (a human at
// the relays page). Both validate identically here.

// maxRankingWeightBodyBytes bounds a weight request body; the payload is one
// number, so anything near the cap is malformed or hostile.
const maxRankingWeightBodyBytes = 4 << 10

// relayRankingWeightStore is the slice of RelayStore the weight endpoints and
// the dashboard need. Narrow so the dashboard (which otherwise holds only a
// telemetry querier) can take it without the whole store, and so tests can
// fake it, following the relayDirectoryLister pattern.
type relayRankingWeightStore interface {
	RelayRankingWeights(context.Context) (map[string]float64, error)
	SetRelayRankingWeight(context.Context, string, float64) (float64, error)
	DeleteRelayRankingWeight(context.Context, string) (float64, error)
}

// rankingWeightRequest is the PUT body: {"weight": 0.5}. The pointer separates
// an absent field from an explicit 0 — 0 is a legitimate (drain) value and a
// body that omits the field must not silently mean it.
type rankingWeightRequest struct {
	Weight *float64 `json:"weight"`
}

// rankingWeightResponse answers both PUT and DELETE with the relay's new
// effective weight and the effective weight it replaced.
type rankingWeightResponse struct {
	RelayID        string  `json:"relay_id"`
	Weight         float64 `json:"weight"`
	PreviousWeight float64 `json:"previous_weight"`
}

type rankingWeightEntry struct {
	RelayID string  `json:"relay_id"`
	Weight  float64 `json:"weight"`
}

// rankingWeightsResponse lists every stored override in relay-ID order, so two
// listings diff cleanly, alongside the default that applies to every other relay.
type rankingWeightsResponse struct {
	GeneratedAt   time.Time            `json:"generated_at"`
	DefaultWeight float64              `json:"default_weight"`
	Count         int                  `json:"count"`
	Weights       []rankingWeightEntry `json:"weights"`
}

// relayWeightsListHandler serves GET /admin/api/relays/weights to the
// operational API token, mirroring relayInventoryHandler's posture: no-store
// on every path, credential before method, one shared limiter.
func relayWeightsListHandler(store relayRankingWeightStore, apiToken string) http.HandlerFunc {
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
		weights, err := store.RelayRankingWeights(r.Context())
		if err != nil {
			slog.Error("could not list relay ranking weights", "error", err)
			writeError(w, http.StatusServiceUnavailable, "could not list relay ranking weights")
			return
		}
		writeJSON(w, http.StatusOK, rankingWeightsResponseFrom(weights, time.Now().UTC()))
	}
}

func rankingWeightsResponseFrom(weights map[string]float64, now time.Time) rankingWeightsResponse {
	entries := make([]rankingWeightEntry, 0, len(weights))
	for id, weight := range weights {
		entries = append(entries, rankingWeightEntry{RelayID: id, Weight: weight})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].RelayID < entries[j].RelayID })
	return rankingWeightsResponse{GeneratedAt: now, DefaultWeight: defaultRankingWeight, Count: len(entries), Weights: entries}
}

// relayWeightHandler serves PUT and DELETE /admin/api/relays/{id}/weight to
// the operational API token. Like the inventory route it is registered without
// a method so the handler screens the method itself after the credential:
// an anonymous prober sees 401 whatever it sends, never a 405 that confirms
// the route exists.
func relayWeightHandler(store relayRankingWeightStore, apiToken string, clientIP func(*http.Request) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !bearerMatches(r, apiToken) {
			writeError(w, http.StatusUnauthorized, "missing or invalid operational API token")
			return
		}
		switch r.Method {
		case http.MethodPut:
			applyRankingWeight(w, r, store, true, clientIP(r), "operational api")
		case http.MethodDelete:
			applyRankingWeight(w, r, store, false, clientIP(r), "operational api")
		default:
			w.Header().Set("Allow", "PUT, DELETE")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

// applyRankingWeight is the one implementation behind both doors: it resolves
// and vets the relay ID from the {id} path segment, decodes the PUT body when
// set is true, writes through the store, logs the change, and answers with the
// new and previous effective weights. via names the door for the log line.
func applyRankingWeight(w http.ResponseWriter, r *http.Request, store relayRankingWeightStore, set bool, clientIP, via string) {
	id := r.PathValue("id")
	// Every real relay ID is broker-minted and well-formed. Anything else
	// cannot name a relay — and, unescaped from the path, might not even be
	// storable — so it is refused before it reaches a backend.
	if !relay.WellFormedRelayID(id) {
		writeError(w, http.StatusBadRequest, "malformed relay id")
		return
	}

	var (
		previous, weight float64
		err              error
	)
	if set {
		weight, err = decodeRankingWeightBody(w, r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		previous, err = store.SetRelayRankingWeight(r.Context(), id, weight)
	} else {
		weight = defaultRankingWeight
		previous, err = store.DeleteRelayRankingWeight(r.Context(), id)
	}
	if errors.Is(err, ErrInvalidRankingWeight) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		slog.Error("could not update relay ranking weight", "relay_id", id, "error", err)
		writeError(w, http.StatusServiceUnavailable, "could not update relay ranking weight")
		return
	}
	// Every change is logged, no-ops included: an operator reading the log
	// after a load shift needs the complete sequence of dial movements, and
	// "who set this relay to 0 and when" must always be answerable.
	slog.Info("relay ranking weight changed",
		"relay_id", id,
		"old_weight", previous,
		"new_weight", weight,
		"restored_default", !set,
		"client_ip", clientIP,
		"via", via,
	)
	writeJSON(w, http.StatusOK, rankingWeightResponse{RelayID: id, Weight: weight, PreviousWeight: previous})
}

// decodeRankingWeightBody reads exactly one JSON object {"weight": n} with n in
// [0, 1]. Anything else — no body, a non-object, unknown fields, a missing or
// non-numeric weight, trailing data, an out-of-range value — is an error the
// caller answers with 400.
func decodeRankingWeightBody(w http.ResponseWriter, r *http.Request) (float64, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRankingWeightBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var req rankingWeightRequest
	if err := decoder.Decode(&req); err != nil {
		return 0, errors.New("request body must be a JSON object like {\"weight\": 0.5}")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return 0, errors.New("request body must contain exactly one JSON object")
	}
	if req.Weight == nil {
		return 0, errors.New("weight is required")
	}
	if err := validateRankingWeight(*req.Weight); err != nil {
		return 0, err
	}
	return *req.Weight, nil
}

// Dashboard door. The relays page authenticates with the admin session cookie,
// not the operational token, so it gets its own routes behind requireAuth. The
// cookie is SameSite=Strict, which already keeps a cross-site page from riding
// the session; these two checks make the mutation endpoints hold that line on
// their own — a browser that ignores SameSite, or a future cookie change, must
// not turn a visited page into a lever on the fleet's ranking.

// dashboardMutationAllowed vets a state-changing dashboard request: it must
// declare a JSON body (an HTML form cannot send application/json, so a
// cross-site form post is refused on content type alone) and it must come from
// the dashboard's own origin.
//
// Same-origin is judged from Sec-Fetch-Site first. The browser computes that
// header itself, pages cannot set it (it is a forbidden header name), and no
// proxy hop rewrites it — unlike Host, which the CDN front in production
// replaces with the origin hostname, so an Origin-vs-Host comparison there
// refuses every legitimate request. Only when the header is absent (older
// browsers) does the check fall back to comparing the Origin header's host
// with Host; a request with neither header is tolerated, since the cookie's
// SameSite=Strict still fences the session. Rejections name the reason so an
// operator debugging a proxy misconfiguration is not left guessing.
func dashboardMutationAllowed(r *http.Request) (int, string) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return http.StatusUnsupportedMediaType, "Content-Type must be application/json"
	}
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))) {
	case "same-origin":
		return 0, ""
	case "":
		// No fetch metadata: fall back to the Origin header below.
	default:
		// cross-site, same-site (a sibling subdomain), or none (a navigation,
		// which never carries a JSON PUT from this page).
		return http.StatusForbidden, "cross-origin dashboard requests are not allowed"
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return 0, ""
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || !strings.EqualFold(parsed.Host, r.Host) {
		return http.StatusForbidden, "cross-origin dashboard requests are not allowed"
	}
	return 0, ""
}

func (d *dashboardServer) setRelayWeight(w http.ResponseWriter, r *http.Request) {
	d.relayWeight(w, r, true)
}

func (d *dashboardServer) deleteRelayWeight(w http.ResponseWriter, r *http.Request) {
	d.relayWeight(w, r, false)
}

func (d *dashboardServer) relayWeight(w http.ResponseWriter, r *http.Request, set bool) {
	if status, reason := dashboardMutationAllowed(r); status != 0 {
		writeError(w, status, reason)
		return
	}
	if d.relayWeights == nil {
		writeError(w, http.StatusServiceUnavailable, "relay ranking weights are not available")
		return
	}
	applyRankingWeight(w, r, d.relayWeights, set, d.clientIP(r), "dashboard")
}
