package discovery

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"openrung/desktop/config"
	"openrung/internal/client"
	"openrung/internal/relay"
)

// relayBody is unsigned: every httptest server here is a loopback host, which
// the shared verification shim (client.ReadVerifiedRelayList) exempts from the
// relay-list signature requirement — the same dev-flow allowance as
// EnforceSecureBrokerURL's loopback http. The signed path is exercised against
// a non-loopback stub in TestListRelaysSignatureNonLoopback.
const relayBody = `{"count":1,"server_time":"2026-07-06T00:00:00Z","relays":[{"id":"r1","public_host":"1.2.3.4","public_port":443}]}`

// noOverride wraps urls as a pure-race candidate list — what
// config.BrokerCandidates builds when no genuine user override is set.
func noOverride(urls ...string) config.Candidates {
	return config.Candidates{URLs: urls}
}

// withOverride wraps urls as a candidate list whose FIRST entry is a genuine
// user override, matching what config.BrokerCandidates builds for one.
func withOverride(urls ...string) config.Candidates {
	return config.Candidates{URLs: urls, OverrideFirst: true}
}

func TestListRelaysSuccess(t *testing.T) {
	var gotVersion, gotPlatform string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.Header.Get("X-OpenRung-App-Version")
		gotPlatform = r.Header.Get("X-OpenRung-Desktop")
		w.Write([]byte(relayBody))
	}))
	defer srv.Close()

	resp, err := ListRelays(context.Background(), srv.URL, Options{Limit: 20})
	if err != nil {
		t.Fatalf("ListRelays: %v", err)
	}
	if len(resp.Relays) != 1 || resp.Relays[0].ID != "r1" {
		t.Fatalf("unexpected relays: %+v", resp.Relays)
	}
	if gotVersion == "" {
		t.Error("X-OpenRung-App-Version header not sent")
	}
	if gotPlatform == "" {
		t.Error("X-OpenRung-Desktop header not sent")
	}
}

// TestListRelaysSignatureNonLoopback drives this package's own entry point —
// it wraps the shared shim with its own request/429 handling, so it needs its
// own coverage — against a non-loopback broker: an unsigned list must fail
// with the distinguishable "unsigned/invalid relay list" error, and the same
// body signed under a (test-)pinned key must verify.
func TestListRelaysSignatureNonLoopback(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	restore := client.PinRelayListKeysForTest(hex.EncodeToString(pub))
	defer restore()

	now := time.Now().UTC()
	body, err := json.Marshal(relay.ListResponse{
		Count:      1,
		ServerTime: now,
		NotAfter:   now.Add(30 * time.Minute),
		Channel:    relay.ChannelAPI,
		Limit:      20,
		Relays:     []relay.Descriptor{{ID: "r1", PublicHost: "1.2.3.4", PublicPort: 443}},
	})
	if err != nil {
		t.Fatalf("marshal relay list: %v", err)
	}
	sigHeader := "ed25519;anyadvisoryid;" + base64.StdEncoding.EncodeToString(ed25519.Sign(priv, body))

	stub := func(signed bool) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			h := make(http.Header)
			if signed {
				h.Set(client.RelaySignatureHeader, sigHeader)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body:       io.NopCloser(strings.NewReader(string(body))),
				Header:     h,
				Request:    r,
			}, nil
		})}
	}

	t.Run("unsigned fails distinguishably", func(t *testing.T) {
		_, err := ListRelays(context.Background(), "https://broker.example.com", Options{Limit: 20, HTTPClient: stub(false)})
		if err == nil {
			t.Fatal("unsigned non-loopback response must fail")
		}
		if !strings.Contains(err.Error(), "unsigned/invalid relay list") {
			t.Fatalf("want the unsigned/invalid marker, got: %v", err)
		}
	})

	t.Run("signed verifies", func(t *testing.T) {
		resp, err := ListRelays(context.Background(), "https://broker.example.com", Options{Limit: 20, HTTPClient: stub(true)})
		if err != nil {
			t.Fatalf("signed response must verify: %v", err)
		}
		if len(resp.Relays) != 1 || resp.Relays[0].ID != "r1" {
			t.Fatalf("unexpected relays: %+v", resp.Relays)
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestListRelaysRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	_, err := ListRelays(context.Background(), srv.URL, Options{})
	var rl *RateLimitedError
	if !errors.As(err, &rl) {
		t.Fatalf("want *RateLimitedError, got %v", err)
	}
	if rl.RetryAfter != 12*time.Second {
		t.Fatalf("RetryAfter = %v, want 12s", rl.RetryAfter)
	}
}

func TestFirstReachableFailsOverToSecond(t *testing.T) {
	// First candidate 429s, second serves relays. The race must let the second
	// candidate win after its staggered start, so a rate-limited primary never
	// takes the map offline.
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer limited.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(relayBody))
	}))
	defer good.Close()

	fetch, err := FirstReachable(context.Background(), noOverride(limited.URL, good.URL), Options{Stagger: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("FirstReachable: %v", err)
	}
	if fetch.BrokerURL != good.URL {
		t.Fatalf("served by %q, want %q", fetch.BrokerURL, good.URL)
	}
	if len(fetch.Response.Relays) != 1 {
		t.Fatalf("unexpected relays: %+v", fetch.Response.Relays)
	}
}

func TestFirstReachableHealthyPrimaryWinsWithoutStartingFallback(t *testing.T) {
	// A healthy primary answers well inside the stagger, so the fallback must
	// never see a request — neither before the winner returns nor from a stray
	// timer afterwards.
	const stagger = 500 * time.Millisecond

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(relayBody))
	}))
	defer primary.Close()
	var fallbackHits atomic.Int64
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits.Add(1)
		w.Write([]byte(relayBody))
	}))
	defer fallback.Close()

	fetch, err := FirstReachable(context.Background(), noOverride(primary.URL, fallback.URL), Options{Stagger: stagger})
	if err != nil {
		t.Fatalf("FirstReachable: %v", err)
	}
	if fetch.BrokerURL != primary.URL {
		t.Fatalf("served by %q, want primary %q", fetch.BrokerURL, primary.URL)
	}
	if hits := fallbackHits.Load(); hits != 0 {
		t.Fatalf("fallback received %d request(s) before the stagger elapsed, want 0", hits)
	}
	// The race ended with the primary's success; the stagger timer must be dead.
	time.Sleep(2 * stagger)
	if hits := fallbackHits.Load(); hits != 0 {
		t.Fatalf("fallback received %d request(s) after the race ended, want 0", hits)
	}
}

func TestFirstReachableHangingPrimaryLosesToSecondAfterStagger(t *testing.T) {
	// The primary accepts the request and hangs. The race must start the second
	// candidate one stagger later and return its success without waiting out
	// the primary's request timeout.
	const stagger = 300 * time.Millisecond

	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // hang until the race aborts this attempt
	}))
	defer hung.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(relayBody))
	}))
	defer good.Close()

	begun := time.Now()
	fetch, err := FirstReachable(context.Background(), noOverride(hung.URL, good.URL), Options{Stagger: stagger})
	elapsed := time.Since(begun)
	if err != nil {
		t.Fatalf("FirstReachable: %v", err)
	}
	if fetch.BrokerURL != good.URL {
		t.Fatalf("served by %q, want %q", fetch.BrokerURL, good.URL)
	}
	if elapsed < stagger {
		t.Fatalf("second candidate won after %v, before the %v stagger", elapsed, stagger)
	}
	if elapsed > 10*stagger {
		t.Fatalf("second candidate won after %v, want shortly after the %v stagger", elapsed, stagger)
	}
}

func TestFirstReachableAllFailReturnsPrimaryError(t *testing.T) {
	// Both candidates fail, the secondary strictly after the primary (it starts
	// one stagger later). The primary's error is the meaningful diagnostic, so
	// it must win over the last-observed one.
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"primary exploded"}`))
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer secondary.Close()

	_, err := FirstReachable(context.Background(), noOverride(primary.URL, secondary.URL), Options{Stagger: 50 * time.Millisecond})
	if err == nil {
		t.Fatal("want error when every candidate fails")
	}
	var rl *RateLimitedError
	if errors.As(err, &rl) {
		t.Fatalf("got the secondary's rate-limit error %v, want the primary's error", err)
	}
	var statusErr *client.BrokerStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want the primary's 503, got %v", err)
	}
}

func TestFirstReachableSingleCandidateBehavesSequentially(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		var hits atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Write([]byte(relayBody))
		}))
		defer srv.Close()

		fetch, err := FirstReachable(context.Background(), noOverride(srv.URL), Options{})
		if err != nil {
			t.Fatalf("FirstReachable: %v", err)
		}
		if fetch.BrokerURL != srv.URL || len(fetch.Response.Relays) != 1 {
			t.Fatalf("unexpected fetch: %+v", fetch)
		}
		if got := hits.Load(); got != 1 {
			t.Fatalf("candidate received %d requests, want exactly 1", got)
		}
	})

	t.Run("failure propagates unchanged without stagger wait", func(t *testing.T) {
		var hits atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer srv.Close()

		begun := time.Now()
		// Default stagger on purpose: a lone failing candidate must return
		// immediately, not wait out config.DiscoveryStagger.
		_, err := FirstReachable(context.Background(), noOverride(srv.URL), Options{})
		elapsed := time.Since(begun)

		var rl *RateLimitedError
		if !errors.As(err, &rl) {
			t.Fatalf("want *RateLimitedError, got %v", err)
		}
		if rl.BrokerURL != srv.URL || rl.RetryAfter != 7*time.Second {
			t.Fatalf("error lost detail: %+v", rl)
		}
		if got := hits.Load(); got != 1 {
			t.Fatalf("candidate received %d requests, want exactly 1", got)
		}
		if elapsed >= 2*time.Second {
			t.Fatalf("single-candidate failure took %v, want an immediate return", elapsed)
		}
	})
}

func TestFirstReachableCancelsLosersOnceWinnerReturns(t *testing.T) {
	// The hanging loser's request must be aborted (observed server-side via the
	// request context) as soon as the winner's response is in.
	const stagger = 200 * time.Millisecond

	loserCanceled := make(chan struct{})
	loser := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(loserCanceled)
	}))
	defer loser.Close()
	winner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(relayBody))
	}))
	defer winner.Close()

	fetch, err := FirstReachable(context.Background(), noOverride(loser.URL, winner.URL), Options{Stagger: stagger})
	if err != nil {
		t.Fatalf("FirstReachable: %v", err)
	}
	if fetch.BrokerURL != winner.URL {
		t.Fatalf("served by %q, want %q", fetch.BrokerURL, winner.URL)
	}
	select {
	case <-loserCanceled:
		// The losing attempt's HTTP request was aborted.
	case <-time.After(5 * time.Second):
		t.Fatal("losing attempt was not canceled after the winner returned")
	}
}

func TestFirstReachableParentCancelDrainsStartedAttempts(t *testing.T) {
	// Both candidates hang. Once BOTH attempts are in flight the caller cancels;
	// the ctx.Done() drain must reap both aborted attempts and return promptly
	// with the primary's context.Canceled — not deadlock, and not wait out the
	// 15s per-attempt request timeout.
	const stagger = 100 * time.Millisecond

	primaryStarted := make(chan struct{})
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(primaryStarted)
		<-r.Context().Done() // hang until the aborted attempt lands
	}))
	defer primary.Close()
	secondaryStarted := make(chan struct{})
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(secondaryStarted)
		<-r.Context().Done()
	}))
	defer secondary.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		fetch Fetch
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		fetch, err := FirstReachable(ctx, noOverride(primary.URL, secondary.URL), Options{Stagger: stagger})
		done <- outcome{fetch: fetch, err: err}
	}()

	// Only cancel once both attempts have reached their servers, so the drain
	// branch runs with two in-flight attempts to reap.
	select {
	case <-primaryStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("primary attempt never reached its server")
	}
	select {
	case <-secondaryStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("secondary attempt never reached its server")
	}
	cancel()

	select {
	case res := <-done:
		if !errors.Is(res.err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v (fetch %+v)", res.err, res.fetch)
		}
	case <-time.After(5 * time.Second):
		// Well under the 15s per-attempt timeout: a return only after that
		// timeout would mean cancellation did not propagate.
		t.Fatal("FirstReachable did not return within 5s of parent cancellation")
	}
}

func TestFirstReachableParentCancelBeforeStaggerSkipsUnstartedCandidate(t *testing.T) {
	// The caller cancels while only candidate[0] is in flight (the stagger is
	// far from elapsing). The drain must reap exactly the one started attempt —
	// not block waiting on results from candidates that never started — and the
	// fallback must never see a request.
	const stagger = time.Minute // never elapses within the test's lifetime

	primaryStarted := make(chan struct{})
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(primaryStarted)
		<-r.Context().Done()
	}))
	defer primary.Close()
	var fallbackHits atomic.Int64
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits.Add(1)
		w.Write([]byte(relayBody))
	}))
	defer fallback.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		fetch Fetch
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		fetch, err := FirstReachable(ctx, noOverride(primary.URL, fallback.URL), Options{Stagger: stagger})
		done <- outcome{fetch: fetch, err: err}
	}()

	select {
	case <-primaryStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("primary attempt never reached its server")
	}
	cancel()

	select {
	case res := <-done:
		if !errors.Is(res.err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v (fetch %+v)", res.err, res.fetch)
		}
	case <-time.After(5 * time.Second):
		// A hang here would mean the drain waited for the never-started
		// fallback's result, which can never arrive.
		t.Fatal("FirstReachable did not return within 5s of parent cancellation")
	}
	if hits := fallbackHits.Load(); hits != 0 {
		t.Fatalf("fallback received %d request(s), want 0 — it must never start after cancellation", hits)
	}
}

func TestFirstReachableOverrideSlowerThanStaggerStillWins(t *testing.T) {
	// A GENUINE user override that is merely slower than the stagger must not
	// be outrun by a default front: the override phase runs alone with its full
	// per-attempt timeout, so the default is never contacted — neither while
	// the override is pending nor after it wins.
	const stagger = 100 * time.Millisecond

	override := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(4 * stagger) // slower than the stagger, well inside the 15s attempt timeout
		w.Write([]byte(relayBody))
	}))
	defer override.Close()
	var defaultHits atomic.Int64
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defaultHits.Add(1)
		w.Write([]byte(relayBody))
	}))
	defer fallback.Close()

	fetch, err := FirstReachable(context.Background(), withOverride(override.URL, fallback.URL), Options{Stagger: stagger})
	if err != nil {
		t.Fatalf("FirstReachable: %v", err)
	}
	if fetch.BrokerURL != override.URL {
		t.Fatalf("served by %q, want the override %q", fetch.BrokerURL, override.URL)
	}
	if hits := defaultHits.Load(); hits != 0 {
		t.Fatalf("default front received %d request(s) while the override was pending, want 0", hits)
	}
}

func TestFirstReachableOverrideFailureRacesRemainingDefaults(t *testing.T) {
	// Once the override FAILS, the staggered race starts over the remaining
	// candidates with the usual semantics: the first default immediately, the
	// next one stagger later, first success wins.
	const stagger = 200 * time.Millisecond

	override := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer override.Close()
	hangingDefault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // hang until the race aborts this attempt
	}))
	defer hangingDefault.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(relayBody))
	}))
	defer good.Close()

	fetch, err := FirstReachable(context.Background(), withOverride(override.URL, hangingDefault.URL, good.URL), Options{Stagger: stagger})
	if err != nil {
		t.Fatalf("FirstReachable: %v", err)
	}
	if fetch.BrokerURL != good.URL {
		t.Fatalf("served by %q, want %q", fetch.BrokerURL, good.URL)
	}
}

func TestFirstReachableOverrideAllFailSurfacesOverrideError(t *testing.T) {
	// The override is candidates[0]: when it and every default fail, ITS error
	// is the surfaced diagnostic — the user configured that broker, so its
	// failure is what they need to see, not a default front's.
	override := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"override exploded"}`))
	}))
	defer override.Close()
	rateLimited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer rateLimited.Close()

	_, err := FirstReachable(context.Background(), withOverride(override.URL, rateLimited.URL), Options{Stagger: 50 * time.Millisecond})
	if err == nil {
		t.Fatal("want error when every candidate fails")
	}
	var rl *RateLimitedError
	if errors.As(err, &rl) {
		t.Fatalf("got the default front's rate-limit error %v, want the override's error", err)
	}
	var statusErr *client.BrokerStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want the override's 503, got %v", err)
	}
}

func TestFirstReachableOverrideSingleCandidateFailurePropagatesUnchanged(t *testing.T) {
	// A lone override reduces to exactly one attempt whose error keeps its
	// detail — there is no remainder to race and no stagger wait.
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	_, err := FirstReachable(context.Background(), withOverride(srv.URL), Options{})
	var rl *RateLimitedError
	if !errors.As(err, &rl) {
		t.Fatalf("want *RateLimitedError, got %v", err)
	}
	if rl.BrokerURL != srv.URL || rl.RetryAfter != 7*time.Second {
		t.Fatalf("error lost detail: %+v", rl)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("override received %d requests, want exactly 1", got)
	}
}

func TestFirstReachableOverrideParentCancelMidRaceSurfacesCancellation(t *testing.T) {
	// The override fails fast, the remaining default hangs, then the caller
	// cancels. The surfaced error must be the cancellation — what the caller
	// classifies on — not the override's stale failure.
	const stagger = 100 * time.Millisecond

	override := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer override.Close()
	defaultStarted := make(chan struct{})
	hangingDefault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(defaultStarted)
		<-r.Context().Done()
	}))
	defer hangingDefault.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		fetch Fetch
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		fetch, err := FirstReachable(ctx, withOverride(override.URL, hangingDefault.URL), Options{Stagger: stagger})
		done <- outcome{fetch: fetch, err: err}
	}()

	select {
	case <-defaultStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("default attempt never reached its server")
	}
	cancel()

	select {
	case res := <-done:
		if !errors.Is(res.err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v (fetch %+v)", res.err, res.fetch)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("FirstReachable did not return within 5s of parent cancellation")
	}
}

func TestFirstReachableNoCandidates(t *testing.T) {
	_, err := FirstReachable(context.Background(), config.Candidates{}, Options{})
	if err == nil {
		t.Fatal("want error for empty candidate list")
	}
}

func TestParseRetryAfter(t *testing.T) {
	if got := parseRetryAfter("30"); got != 30*time.Second {
		t.Errorf("delta-seconds: got %v", got)
	}
	if got := parseRetryAfter(""); got != 0 {
		t.Errorf("empty: got %v", got)
	}
	if got := parseRetryAfter("-5"); got != 0 {
		t.Errorf("negative: got %v", got)
	}
	if got := parseRetryAfter("garbage"); got != 0 {
		t.Errorf("garbage: got %v", got)
	}
}
