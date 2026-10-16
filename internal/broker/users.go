package broker

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openrung/openrung/brokerapi"
)

// Client users (ADR-006 "Client enrollment and relay authorization").
//
// A user is one client installation's P-256 key. The broker mints a random,
// stable user ID for a key the first time that key authenticates and returns
// the same ID forever after, so enrollment is not a separate state; a user
// can be disabled by an operator, which stops every later authorization while
// keeping the row (and the ID) so the decision sticks across reinstalls that
// keep the key. The statement, signature form and canonical key spelling are
// shared with clients through brokerapi so both sides sign and verify
// identical bytes.
//
// Nothing here is linked to telemetry: user IDs are never written with the
// X-OpenRung-Client-ID header, public keys are never logged, and the admin
// view omits the key.

const (
	userIDPrefix = "user_"
	userIDBytes  = 16

	// userChallengeTTL is how long an issued challenge may be signed. It only
	// needs to cover one network round trip through a CDN front; a client that
	// misses it asks for another.
	userChallengeTTL = time.Minute
	// userChallengeMaxEntries bounds outstanding challenges; past it the table
	// refuses to issue (503) rather than grow, since the per-IP limiter already
	// bounds what one source can request.
	userChallengeMaxEntries = 100_000
	userChallengeBytes      = 32

	maxUserAuthBodyBytes = 8 << 10

	// Access tokens outlive the connect they authorize so a connect costs one
	// request, not a challenge exchange. Status is re-checked on every lease
	// issuance, so a long TTL never extends a disabled user's access.
	defaultAccessTokenTTL = 24 * time.Hour
	minAccessTokenTTL     = time.Minute
	maxAccessTokenTTL     = 7 * 24 * time.Hour
	accessTokenKeyBytes   = 32
	accessTokenVersion    = 1
	accessTokenNonceBytes = 12

	// Enrollment budget defaults, per source (IPv4 address or IPv6 /64). They
	// only govern NEW identities; an existing key authenticates freely. Sized
	// for a household or small office behind one address, not a datacenter.
	defaultEnrollmentBurstLimit  = 3
	defaultEnrollmentBurstWindow = time.Hour
	defaultEnrollmentDailyLimit  = 10
	defaultEnrollmentDailyWindow = 24 * time.Hour

	maxUserStatusReasonBytes = 256
)

var (
	ErrUserNotFound = errors.New("user not found")
	// ErrInvalidUserStatus rejects any status other than the two the schema
	// allows, so a typo cannot park a user in an undefined state.
	ErrInvalidUserStatus = errors.New("user status must be enabled or disabled")
)

// User is the broker's record of one installation key.
type User struct {
	ID string
	// PublicKey is the canonical spelling (brokerapi.CanonicalUserPublicKey);
	// stores enforce its uniqueness.
	PublicKey  string
	Status     string
	CreatedAt  time.Time
	LastAuthAt time.Time
	// DisabledAt is zero while the user is enabled.
	DisabledAt     time.Time
	DisabledReason string
}

// EnrollmentWindow is one fixed-window budget on new identities per source.
type EnrollmentWindow struct {
	Name   string
	Limit  int
	Window time.Duration
}

// EnrollmentBudget configures the per-source limits on new identities. Zero
// limits apply the defaults; a negative limit disables that window.
type EnrollmentBudget struct {
	BurstLimit  int
	BurstWindow time.Duration
	DailyLimit  int
	DailyWindow time.Duration
}

// windows resolves the configured budget to the windows a store applies.
func (b EnrollmentBudget) windows() []EnrollmentWindow {
	burst := EnrollmentWindow{Name: "burst", Limit: b.BurstLimit, Window: b.BurstWindow}
	if burst.Limit == 0 {
		burst.Limit = defaultEnrollmentBurstLimit
	}
	if burst.Window <= 0 {
		burst.Window = defaultEnrollmentBurstWindow
	}
	daily := EnrollmentWindow{Name: "day", Limit: b.DailyLimit, Window: b.DailyWindow}
	if daily.Limit == 0 {
		daily.Limit = defaultEnrollmentDailyLimit
	}
	if daily.Window <= 0 {
		daily.Window = defaultEnrollmentDailyWindow
	}
	var windows []EnrollmentWindow
	for _, w := range []EnrollmentWindow{burst, daily} {
		if w.Limit > 0 {
			windows = append(windows, w)
		}
	}
	return windows
}

// EnrollmentLimitedError reports an exhausted enrollment budget and when the
// earliest exceeded window resets.
type EnrollmentLimitedError struct {
	RetryAfter time.Duration
}

func (e *EnrollmentLimitedError) Error() string {
	return fmt.Sprintf("enrollment budget exhausted for this source (retry after %s)", e.RetryAfter.Round(time.Second))
}

// AccessStore is the slice of the store the user endpoints need. It is a
// separate interface from RelayStore, asserted at server construction, so the
// relay-only fakes in tests keep compiling and a store can grow user support
// independently of the directory.
type AccessStore interface {
	// AuthenticateUser returns the user for a canonical public key, creating
	// it on first sight. A new identity is charged against every window of
	// the source's enrollment budget first and refused with
	// *EnrollmentLimitedError when any is exhausted — in which case nothing
	// is stored and nothing is charged. An existing key is never charged.
	// created reports whether this call minted the user.
	AuthenticateUser(ctx context.Context, canonicalPublicKey, sourceKey string, windows []EnrollmentWindow, now time.Time) (user User, created bool, err error)
	UserByID(ctx context.Context, id string) (User, error)
	// SetUserStatus records an operator decision. Disabling stamps
	// DisabledAt and the reason and, in the same transaction, revokes every
	// live lease the user holds — the returned relay IDs (sorted) are the
	// relays whose desired set changed and must be woken; enabling clears
	// both fields and revokes nothing. ErrUserNotFound for an unknown ID,
	// ErrInvalidUserStatus for anything but the two states.
	SetUserStatus(ctx context.Context, id, status, reason string, now time.Time) (User, []string, error)
	LeaseStore
}

func validUserStatus(status string) bool {
	return status == brokerapi.UserStatusEnabled || status == brokerapi.UserStatusDisabled
}

func newUserID() (string, error) {
	var b [userIDBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return userIDPrefix + hex.EncodeToString(b[:]), nil
}

// wellFormedUserID reports whether id has the shape newUserID mints, so path
// segments that cannot name a user are refused before any store lookup.
func wellFormedUserID(id string) bool {
	rest, ok := strings.CutPrefix(id, userIDPrefix)
	if !ok || len(rest) != 2*userIDBytes {
		return false
	}
	for _, c := range []byte(rest) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// challengeTable holds outstanding challenges in memory. Challenges are
// consumed on first use, so each buys exactly one verification attempt. The
// table is process-local on purpose: a broker restart forgets in-flight
// challenges, which a client handles by requesting another, and no durable
// state can be replayed after the restart.
type challengeTable struct {
	ttl        time.Duration
	maxEntries int

	mu      sync.Mutex
	entries map[string]time.Time
}

func newChallengeTable(ttl time.Duration, maxEntries int) *challengeTable {
	return &challengeTable{ttl: ttl, maxEntries: maxEntries, entries: make(map[string]time.Time)}
}

// issue mints a challenge expiring at now+ttl, or reports the table full.
func (t *challengeTable) issue(now time.Time) (string, time.Time, bool) {
	var raw [userChallengeBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", time.Time{}, false
	}
	challenge := base64.RawURLEncoding.EncodeToString(raw[:])
	expiresAt := now.Add(t.ttl)

	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.entries) >= t.maxEntries {
		for key, expiry := range t.entries {
			if !expiry.After(now) {
				delete(t.entries, key)
			}
		}
		if len(t.entries) >= t.maxEntries {
			return "", time.Time{}, false
		}
	}
	t.entries[challenge] = expiresAt
	return challenge, expiresAt, true
}

// consume removes the challenge and reports whether it was outstanding and
// unexpired. A second call for the same value always fails.
func (t *challengeTable) consume(challenge string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	expiry, ok := t.entries[challenge]
	if !ok {
		return false
	}
	delete(t.entries, challenge)
	return expiry.After(now)
}

// accessTokenCodec mints and verifies bearer access tokens. Only the broker
// ever verifies them, so an HMAC is sufficient and cheaper than a signature:
// token = base64url(version ‖ len ‖ user_id ‖ exp ‖ nonce ‖ HMAC-SHA256(key, …)).
// Rotating the key invalidates every token at once; clients re-authenticate.
type accessTokenCodec struct {
	key []byte
	ttl time.Duration
}

func newAccessTokenCodec(key []byte, ttl time.Duration) (accessTokenCodec, error) {
	if len(key) != accessTokenKeyBytes {
		return accessTokenCodec{}, fmt.Errorf("access token key must be %d bytes", accessTokenKeyBytes)
	}
	if ttl == 0 {
		ttl = defaultAccessTokenTTL
	}
	if ttl < minAccessTokenTTL || ttl > maxAccessTokenTTL {
		return accessTokenCodec{}, fmt.Errorf("access token TTL must be within [%s, %s]", minAccessTokenTTL, maxAccessTokenTTL)
	}
	return accessTokenCodec{key: append([]byte(nil), key...), ttl: ttl}, nil
}

func (c accessTokenCodec) issue(userID string, now time.Time) (string, time.Time, error) {
	if len(userID) == 0 || len(userID) > 255 {
		return "", time.Time{}, errors.New("access token user id must be 1..255 bytes")
	}
	expiresAt := now.Add(c.ttl).UTC().Truncate(time.Second)
	payload := make([]byte, 0, 2+len(userID)+8+accessTokenNonceBytes+sha256.Size)
	payload = append(payload, accessTokenVersion, byte(len(userID)))
	payload = append(payload, userID...)
	payload = binary.BigEndian.AppendUint64(payload, uint64(expiresAt.Unix()))
	var nonce [accessTokenNonceBytes]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", time.Time{}, err
	}
	payload = append(payload, nonce[:]...)
	mac := hmac.New(sha256.New, c.key)
	mac.Write(payload)
	payload = mac.Sum(payload)
	return base64.RawURLEncoding.EncodeToString(payload), expiresAt, nil
}

// verify returns the user ID a valid, unexpired token names.
func (c accessTokenCodec) verify(token string, now time.Time) (string, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) < 2+8+accessTokenNonceBytes+sha256.Size {
		return "", false
	}
	if raw[0] != accessTokenVersion {
		return "", false
	}
	idLen := int(raw[1])
	body := len(raw) - sha256.Size
	if body != 2+idLen+8+accessTokenNonceBytes {
		return "", false
	}
	mac := hmac.New(sha256.New, c.key)
	mac.Write(raw[:body])
	if subtle.ConstantTimeCompare(mac.Sum(nil), raw[body:]) != 1 {
		return "", false
	}
	userID := string(raw[2 : 2+idLen])
	expiresAt := time.Unix(int64(binary.BigEndian.Uint64(raw[2+idLen:2+idLen+8])), 0)
	if !expiresAt.After(now) {
		return "", false
	}
	return userID, true
}

// userAuthService wires the two public user endpoints. It exists only when
// the broker is configured with an access token key; without one no user
// route is registered, like the WSS ticket endpoint without its seed.
type userAuthService struct {
	store      AccessStore
	challenges *challengeTable
	tokens     accessTokenCodec
	windows    []EnrollmentWindow
	clientIP   *clientIPResolver
	now        func() time.Time
}

func newUserAuthService(store RelayStore, cfg Config, clientIP *clientIPResolver) *userAuthService {
	if len(cfg.UserTokenKey) == 0 {
		return nil
	}
	access, ok := store.(AccessStore)
	if !ok {
		panic("broker: an access token key is configured but the relay store does not implement AccessStore")
	}
	tokens, err := newAccessTokenCodec(cfg.UserTokenKey, cfg.AccessTokenTTL)
	if err != nil {
		panic("broker: " + err.Error())
	}
	return &userAuthService{
		store:      access,
		challenges: newChallengeTable(userChallengeTTL, userChallengeMaxEntries),
		tokens:     tokens,
		windows:    cfg.EnrollmentBudget.windows(),
		clientIP:   clientIP,
		now:        time.Now,
	}
}

// challengeHandler serves POST /api/v1/users/challenge. The body is ignored.
func (s *userAuthService) challengeHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		challenge, expiresAt, ok := s.challenges.issue(s.now().UTC())
		if !ok {
			w.Header().Set("Retry-After", "5")
			writeError(w, http.StatusServiceUnavailable, "too many outstanding challenges, retry shortly")
			return
		}
		writeJSON(w, http.StatusCreated, brokerapi.UserChallenge{Challenge: challenge, ExpiresAt: expiresAt})
	}
}

// authHandler serves POST /api/v1/users/auth. The challenge is consumed
// before the signature is checked, so one challenge buys one attempt and a
// failed attempt can never be retried against the same challenge.
func (s *userAuthService) authHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		var request brokerapi.UserAuthRequest
		r.Body = http.MaxBytesReader(w, r.Body, maxUserAuthBodyBytes)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid user authentication JSON")
			return
		}
		if err := requireJSONEOF(decoder); err != nil {
			writeError(w, http.StatusBadRequest, "user authentication request must contain one JSON object")
			return
		}
		request.Challenge = strings.TrimSpace(request.Challenge)
		if !brokerapi.ValidUserChallenge(request.Challenge) {
			writeError(w, http.StatusBadRequest, "challenge is required and must be printable text")
			return
		}
		publicKey, err := brokerapi.ParseUserPublicKey(request.PublicKey)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		canonical, err := brokerapi.CanonicalUserPublicKey(publicKey)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		now := s.now().UTC()
		if !s.challenges.consume(request.Challenge, now) {
			writeError(w, http.StatusUnauthorized, "challenge unknown or expired")
			return
		}
		if !brokerapi.VerifyUserAuth(publicKey, request.Challenge, request.Signature) {
			writeError(w, http.StatusUnauthorized, "invalid signature")
			return
		}

		user, created, err := s.store.AuthenticateUser(r.Context(), canonical, sourceBucketKey(s.clientIP.clientIP(r)), s.windows, now)
		var limited *EnrollmentLimitedError
		if errors.As(err, &limited) {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(limited.RetryAfter)))
			writeError(w, http.StatusTooManyRequests, "too many new enrollments from this network, retry later")
			return
		}
		if err != nil {
			slog.Error("could not authenticate user", "error", err)
			writeError(w, http.StatusServiceUnavailable, "could not authenticate user")
			return
		}
		if created {
			slog.Info("user enrolled", "user_id", user.ID)
		}
		if user.Status != brokerapi.UserStatusEnabled {
			// No token for a disabled user; the status tells the client why.
			writeJSON(w, http.StatusForbidden, brokerapi.UserAuthResponse{UserID: user.ID, Status: user.Status})
			return
		}
		token, expiresAt, err := s.tokens.issue(user.ID, now)
		if err != nil {
			slog.Error("could not issue access token", "user_id", user.ID, "error", err)
			writeError(w, http.StatusInternalServerError, "could not issue access token")
			return
		}
		writeJSON(w, http.StatusOK, brokerapi.UserAuthResponse{
			UserID: user.ID, Status: user.Status, AccessToken: token, TokenExpiresAt: expiresAt,
		})
	}
}

// retryAfterSeconds rounds a wait up to whole seconds, never below one, so a
// Retry-After header can never tell a client to retry immediately.
func retryAfterSeconds(wait time.Duration) int {
	seconds := int((wait + time.Second - 1) / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}
