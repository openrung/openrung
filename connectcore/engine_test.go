package connectcore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"
	"github.com/openrung/openrung/connectcore/client"
	"github.com/openrung/openrung/connectcore/clienttelemetry"
	"github.com/openrung/openrung/connectcore/proxyconfig"
)

func usableRelay(id, countryCode, city, country string) brokerapi.RelayDescriptor {
	return brokerapi.RelayDescriptor{
		ID:               id,
		PublicHost:       "203.0.113.5",
		PublicPort:       443,
		Protocol:         brokerapi.ProtocolVLESSRealityVision,
		ClientID:         "uuid",
		RealityPublicKey: "pk",
		ShortID:          "sid",
		ServerName:       "sni",
		Flow:             brokerapi.FlowVision,
		ExitMode:         brokerapi.ExitModeDirect,
		ExpiresAt:        time.Now().Add(time.Hour),
		RelayGeoLocation: brokerapi.RelayGeoLocation{City: city, Country: country, CountryCode: countryCode, Latitude: 1, Longitude: 2},
	}
}

func listOf(relays ...brokerapi.RelayDescriptor) brokerapi.RelayListResponse {
	return brokerapi.RelayListResponse{Count: len(relays), ServerTime: time.Now(), Relays: relays}
}

func candidateIDs(cands []brokerapi.RelayDescriptor) []string {
	ids := make([]string, 0, len(cands))
	for _, cand := range cands {
		ids = append(ids, cand.ID)
	}
	return ids
}

func TestFilterCandidatesPinnedID(t *testing.T) {
	usable := []brokerapi.RelayDescriptor{usableRelay("a", "JP", "Tokyo", "Japan"), usableRelay("b", "SG", "", "Singapore")}
	got, stage, err := FilterCandidates(usable, RelayTarget{Country: "JP", RelayID: "b"}) // id wins over country
	if err != nil || stage != "" {
		t.Fatalf("pinned id: stage %q err %v", stage, err)
	}
	// Pinned: exactly the target, never a fallback relay.
	if len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("pinned id candidates = %v", candidateIDs(got))
	}
}

func TestFilterCandidatesPinnedIDAbsent(t *testing.T) {
	usable := []brokerapi.RelayDescriptor{usableRelay("a", "JP", "Tokyo", "Japan")}
	_, stage, err := FilterCandidates(usable, RelayTarget{RelayID: "zz"})
	if err == nil || stage != "relay_id_filter" {
		t.Fatalf("absent pinned id: stage %q err %v", stage, err)
	}
	if !errors.Is(err, client.ErrRelayNotInList) || !strings.Contains(err.Error(), `relay "zz"`) {
		t.Fatalf("absent pinned id error = %v", err)
	}
}

// A label may name several relays (the CLI's -relay-label), and it is an
// identity target like an id: no silent fallback to an unlabelled relay.
func TestFilterCandidatesLabel(t *testing.T) {
	labelled := func(id, label string) brokerapi.RelayDescriptor {
		r := usableRelay(id, "JP", "Tokyo", "Japan")
		r.Label = label
		return r
	}
	usable := []brokerapi.RelayDescriptor{labelled("a", "home"), labelled("b", "lab"), labelled("c", "home")}

	got, stage, err := FilterCandidates(usable, RelayTarget{Label: "home"})
	if err != nil || stage != "" {
		t.Fatalf("label filter: stage %q err %v", stage, err)
	}
	if ids := candidateIDs(got); len(ids) != 2 || ids[0] != "a" || ids[1] != "c" {
		t.Fatalf("label candidates = %v", ids)
	}

	// id and label together keep every relay matching either, as the CLI's
	// two flags always have.
	got, _, err = FilterCandidates(usable, RelayTarget{RelayID: "b", Label: "home"})
	if err != nil {
		t.Fatalf("id+label filter: %v", err)
	}
	if ids := candidateIDs(got); len(ids) != 3 {
		t.Fatalf("id+label candidates = %v", ids)
	}

	_, stage, err = FilterCandidates(usable, RelayTarget{Label: "absent"})
	if err == nil || stage != "relay_id_filter" {
		t.Fatalf("absent label: stage %q err %v", stage, err)
	}
	if !errors.Is(err, client.ErrRelayNotInList) || !strings.Contains(err.Error(), `label "absent"`) {
		t.Fatalf("absent label error = %v", err)
	}
}

func TestFilterCandidatesCountryKeepsBrokerOrder(t *testing.T) {
	usable := []brokerapi.RelayDescriptor{
		usableRelay("a", "SG", "", "Singapore"),
		usableRelay("b", "JP", "Tokyo", "Japan"),
		usableRelay("c", "sg", "", "Singapore"), // case-insensitive match
		usableRelay("d", "", "", ""),            // geo-less: excluded from a targeted connect
	}
	got, stage, err := FilterCandidates(usable, RelayTarget{Country: "sg"})
	if err != nil || stage != "" {
		t.Fatalf("country filter: stage %q err %v", stage, err)
	}
	if ids := candidateIDs(got); len(ids) != 2 || ids[0] != "a" || ids[1] != "c" {
		t.Fatalf("country candidates = %v", ids)
	}
}

func TestFilterCandidatesCountryAbsent(t *testing.T) {
	usable := []brokerapi.RelayDescriptor{usableRelay("a", "JP", "Tokyo", "Japan")}
	_, stage, err := FilterCandidates(usable, RelayTarget{Country: "US"})
	if err == nil || stage != "relay_geo_filter" {
		t.Fatalf("absent country: stage %q err %v", stage, err)
	}
}

func TestFilterCandidatesAutoKeepsWholeList(t *testing.T) {
	usable := []brokerapi.RelayDescriptor{usableRelay("a", "JP", "Tokyo", "Japan"), usableRelay("b", "SG", "", "Singapore")}
	got, stage, err := FilterCandidates(usable, RelayTarget{})
	if err != nil || stage != "" {
		t.Fatalf("auto: stage %q err %v", stage, err)
	}
	if ids := candidateIDs(got); len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("auto candidates = %v", ids)
	}
}

func TestRelayTargetTargeted(t *testing.T) {
	cases := []struct {
		target RelayTarget
		want   bool
	}{
		{RelayTarget{}, false},
		{RelayTarget{RelayID: " "}, false},
		{RelayTarget{RelayID: "relay_abc"}, true},
		{RelayTarget{Label: "home"}, true},
		{RelayTarget{Country: "jp"}, true},
	}
	for _, c := range cases {
		if got := c.target.Targeted(); got != c.want {
			t.Fatalf("Targeted(%+v) = %t, want %t", c.target, got, c.want)
		}
	}
}

func TestUsableRelaysFiltersWithoutReordering(t *testing.T) {
	expired := usableRelay("x", "JP", "", "Japan")
	expired.ExpiresAt = time.Now().Add(-time.Minute)
	resp := listOf(usableRelay("a", "JP", "Tokyo", "Japan"), expired, usableRelay("b", "SG", "", "Singapore"))
	got := usableRelays(resp)
	if ids := candidateIDs(got); len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("usable = %v", ids)
	}
}

func TestDemoteRelayMovesFailedToEnd(t *testing.T) {
	cands := []brokerapi.RelayDescriptor{
		usableRelay("a", "JP", "", "Japan"),
		usableRelay("b", "SG", "", "Singapore"),
		usableRelay("c", "DE", "", "Germany"),
	}
	got := demoteRelay(cands, "a")
	if ids := candidateIDs(got); ids[0] != "b" || ids[1] != "c" || ids[2] != "a" {
		t.Fatalf("demoted order = %v", ids)
	}
	// Demoting an id that is not present is a no-op.
	same := demoteRelay(cands, "zz")
	if ids := candidateIDs(same); ids[0] != "a" || ids[1] != "b" || ids[2] != "c" {
		t.Fatalf("no-op demote order = %v", ids)
	}
}

func TestGeoLabelPrefersCityCountry(t *testing.T) {
	if got := geoLabel(usableRelay("a", "JP", "Tokyo", "Japan")); got != "Tokyo, Japan" {
		t.Fatalf("geoLabel = %q", got)
	}
	countryOnly := usableRelay("a", "JP", "", "Japan")
	if got := geoLabel(countryOnly); got != "Japan" {
		t.Fatalf("country-only geoLabel = %q", got)
	}
}

func TestRecentFromRequiresCountryCode(t *testing.T) {
	if recentFrom(usableRelay("a", "", "", "")) != nil {
		t.Fatal("relay without country code should yield no recent")
	}
	r := recentFrom(usableRelay("a", "jp", "Tokyo", "Japan"))
	if r == nil || r.CountryCode != "JP" || r.Label != "Tokyo, Japan" {
		t.Fatalf("unexpected recent: %+v", r)
	}
}

func TestPrependRecentDedupesAndCaps(t *testing.T) {
	var recents []RecentNode
	add := func(cc string) {
		recents = prependRecent(recents, RecentNode{CountryCode: cc, Label: cc}, 3)
	}
	add("JP")
	add("SG")
	add("US")
	add("DE") // exceeds cap 3 → oldest (JP) drops
	if len(recents) != 3 {
		t.Fatalf("expected cap 3, got %d: %+v", len(recents), recents)
	}
	if recents[0].CountryCode != "DE" {
		t.Fatalf("newest should be first, got %q", recents[0].CountryCode)
	}
	// Re-adding an existing code moves it to front without duplicating.
	add("US")
	if len(recents) != 3 {
		t.Fatalf("dedupe failed, len=%d: %+v", len(recents), recents)
	}
	if recents[0].CountryCode != "US" {
		t.Fatalf("re-added code should move to front, got %q", recents[0].CountryCode)
	}
	seen := map[string]int{}
	for _, r := range recents {
		seen[r.CountryCode]++
	}
	for cc, count := range seen {
		if count != 1 {
			t.Fatalf("country %s appears %d times: %+v", cc, count, recents)
		}
	}
}

// testSink captures every engine event for assertions. It implements
// NoticeSink so tests can assert the typed notices alongside states and logs.
type testSink struct {
	mu      sync.Mutex
	states  []State
	logs    []string
	notices []Notice
}

func (c *testSink) StateChanged(state State) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.states = append(c.states, state)
}

func (c *testSink) Log(entry LogEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logs = append(c.logs, entry.Line)
}

func (c *testSink) Notice(notice Notice) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.notices = append(c.notices, notice)
}

func (c *testSink) noticesOf(kind NoticeKind) []Notice {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Notice
	for _, notice := range c.notices {
		if notice.Kind == kind {
			out = append(out, notice)
		}
	}
	return out
}

func (c *testSink) last() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.states[len(c.states)-1]
}

func (c *testSink) logLines() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.logs, "\n")
}

func TestSetStatusEmitsStateAndLogsReachSink(t *testing.T) {
	sink := &testSink{}
	s := New()
	s.Sink = sink

	s.appendLog("hello")
	s.setStatus(StatusConnecting, keepLabel, clearError)

	last := sink.last()
	if last.Status != StatusConnecting {
		t.Fatalf("status = %q", last.Status)
	}
	if last.LastError != nil {
		t.Fatalf("lastError should be cleared, got %v", *last.LastError)
	}
	// The log line was delivered before the state change that followed it.
	if lines := sink.logLines(); lines != "hello" {
		t.Fatalf("expected log line at sink, got %q", lines)
	}
	// Contract: slices are never nil.
	if last.Recents == nil {
		t.Fatal("recents must be a non-nil array")
	}
}

func TestMarkConnectedSetsLabelAndRecent(t *testing.T) {
	sink := &testSink{}
	s := New()
	s.Sink = sink

	recent := recentFrom(usableRelay("a", "JP", "Tokyo", "Japan"))
	s.markConnected("Tokyo, Japan", recent)

	last := sink.last()
	if last.Status != StatusConnected {
		t.Fatalf("status = %q", last.Status)
	}
	if last.RelayLabel == nil || *last.RelayLabel != "Tokyo, Japan" {
		t.Fatalf("relayLabel = %v", last.RelayLabel)
	}
	if len(last.Recents) != 1 || last.Recents[0].CountryCode != "JP" {
		t.Fatalf("recents = %+v", last.Recents)
	}
}

func TestFailedStatusCarriesError(t *testing.T) {
	sink := &testSink{}
	s := New()
	s.Sink = sink
	s.setStatus(StatusFailed, keepLabel, setError("boom"))
	last := sink.last()
	if last.Status != StatusFailed || last.LastError == nil || *last.LastError != "boom" {
		t.Fatalf("failed state not carried: %+v", last)
	}
}

func TestLocalProxyPortRetriesAfterResolutionFailure(t *testing.T) {
	s := New()
	s.Persistence = &fakePersistence{}

	// An unusable override reports an error and pins nothing.
	t.Setenv(proxyconfig.PortEnv, "not-a-port")
	if _, err := s.LocalProxyPort(); err == nil {
		t.Fatal("first failed resolution unexpectedly succeeded")
	}

	t.Setenv(proxyconfig.PortEnv, "46685")
	port, err := s.LocalProxyPort()
	if err != nil || port != 46685 {
		t.Fatalf("retry = %d, %v; want 46685, nil", port, err)
	}

	// Later calls keep that endpoint even though resolving again would not.
	t.Setenv(proxyconfig.PortEnv, "46686")
	pinned, err := s.LocalProxyPort()
	if err != nil || pinned != port {
		t.Fatalf("successful endpoint was not pinned: %d, %v", pinned, err)
	}
}

// testProxySnapshot is the opaque platform snapshot as engine tests see it:
// the engine must round-trip it between OSProxy and Persistence unmodified.
type testProxySnapshot struct {
	Server string
}

type fakeProxyController struct {
	supported  bool
	snap       OSProxySnapshot
	setErr     error
	restoreErr error
	sets       int
	restores   []OSProxySnapshot
}

func (f *fakeProxyController) Supported() bool { return f.supported }

func (f *fakeProxyController) Snapshot() (OSProxySnapshot, error) {
	return f.snap, nil
}

func (f *fakeProxyController) Set(host string, port int) error {
	f.sets++
	return f.setErr
}

func (f *fakeProxyController) Restore(snap OSProxySnapshot) error {
	f.restores = append(f.restores, snap)
	return f.restoreErr
}

// fakePersistence is an in-memory Persistence for engine tests.
type fakePersistence struct {
	mu      sync.Mutex
	recents []RecentNode
	snap    OSProxySnapshot
	hasSnap bool

	port int // the persisted port; 0 means none stored yet
	// winner stands in for another process that persisted first, which the
	// locked store reports instead of the candidate offered.
	winner    int
	saveErr   error
	loadCalls int
	saveCalls int
}

func (f *fakePersistence) LoadRecents() []RecentNode {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]RecentNode(nil), f.recents...)
}

func (f *fakePersistence) SaveRecents(recents []RecentNode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recents = append([]RecentNode(nil), recents...)
	return nil
}

func (f *fakePersistence) LoadProxyPort() (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadCalls++
	return f.port, f.port != 0
}

func (f *fakePersistence) LoadOrSaveProxyPort(candidate int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saveCalls++
	if f.saveErr != nil {
		return 0, f.saveErr
	}
	if f.winner != 0 {
		f.port = f.winner
	} else if f.port == 0 {
		f.port = candidate
	}
	return f.port, nil
}

func (f *fakePersistence) portCalls() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loadCalls, f.saveCalls
}

func (f *fakePersistence) SaveProxySnapshot(snap OSProxySnapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap = snap
	f.hasSnap = true
	return nil
}

func (f *fakePersistence) LoadProxySnapshot() (OSProxySnapshot, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap, f.hasSnap
}

func (f *fakePersistence) ClearProxySnapshot() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap = nil
	f.hasSnap = false
	return nil
}

func TestCleanupKeepsRecoverySnapshotUntilRestoreSucceeds(t *testing.T) {
	store := &fakePersistence{}
	snap := testProxySnapshot{Server: "10.0.0.1:3128"}
	if err := store.SaveProxySnapshot(snap); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	proxy := &fakeProxyController{supported: true, restoreErr: errors.New("notify failed")}
	s := New()
	s.OSProxy = proxy
	s.Persistence = store
	conn := &connection{proxySet: true, snapshotTaken: true, snapshot: snap}

	s.cleanupConn(conn)
	if !conn.proxySet {
		t.Fatal("failed restore must remain pending")
	}
	if _, ok := store.LoadProxySnapshot(); !ok {
		t.Fatal("failed restore must keep the crash-recovery snapshot")
	}

	proxy.restoreErr = nil
	s.cleanupConn(conn)
	if conn.proxySet {
		t.Fatal("successful retry must clear the pending proxy state")
	}
	if _, ok := store.LoadProxySnapshot(); ok {
		t.Fatal("successful retry must clear the crash-recovery snapshot")
	}
}

func TestApplySystemProxyRestoresSnapshotWhenSetFails(t *testing.T) {
	snap := testProxySnapshot{Server: "10.0.0.1:3128"}
	proxy := &fakeProxyController{
		supported: true,
		snap:      snap,
		setErr:    errors.New("notify failed after write"),
	}
	s := New()
	s.OSProxy = proxy
	conn := &connection{}

	s.applyProxy(conn, 7890)

	if conn.proxySet {
		t.Fatal("connection should not be marked proxySet when Set fails")
	}
	if len(proxy.restores) != 1 {
		t.Fatalf("expected failed Set to restore snapshot once, got %d restores", len(proxy.restores))
	}
	if got, ok := proxy.restores[0].(testProxySnapshot); !ok || got != snap {
		t.Fatalf("restored snapshot = %+v, want %+v", proxy.restores[0], snap)
	}
}

// captureTransport records the last request body it saw and answers 200, so a
// telemetry flush can be inspected without a real broker.
type captureTransport struct {
	mu   sync.Mutex
	body []byte
}

func (c *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.body = append([]byte(nil), body...)
	c.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("{}")),
		Header:     http.Header{},
	}, nil
}

func TestAttachGeoAttributesStampsSessionTelemetry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// The lookup blocks on gate, so the test can prove done tracks the
	// lookup's actual lifetime.
	gate := make(chan struct{})
	orig := lookupGeoAttributes
	lookupGeoAttributes = func(context.Context, *http.Client) map[string]string {
		<-gate
		return map[string]string{"country": "Testland", "country_code": "TL", "isp": "Test ISP"}
	}
	t.Cleanup(func() { lookupGeoAttributes = orig })

	transport := &captureTransport{}
	mgr, err := clienttelemetry.New("https://broker.test", "test", &http.Client{Transport: transport})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if _, err := mgr.BeginSession(); err != nil {
		t.Fatalf("begin session: %v", err)
	}

	g := attachGeoAttributes(mgr, nil)
	select {
	case <-g.done:
		t.Fatal("done closed while the lookup was still in flight")
	default:
	}

	close(gate)
	select {
	case <-g.done:
	case <-time.After(5 * time.Second):
		t.Fatal("done never closed after the lookup returned")
	}

	// Once the lookup has finished, the attributes are attached: an event
	// recorded afterwards carries them.
	mgr.Record("connection_succeeded", "relay_1", nil, nil)
	if err := mgr.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	body := string(transport.body)
	for _, want := range []string{`"country":"Testland"`, `"country_code":"TL"`, `"isp":"Test ISP"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("flushed telemetry missing %s:\n%s", want, body)
		}
	}
}

func TestAttachGeoAttributesNilManagerSkipsLookup(t *testing.T) {
	orig := lookupGeoAttributes
	called := false
	lookupGeoAttributes = func(context.Context, *http.Client) map[string]string {
		called = true
		return nil
	}
	t.Cleanup(func() { lookupGeoAttributes = orig })

	if g := attachGeoAttributes(nil, nil); g != nil {
		t.Fatal("nil manager must return a nil lookup")
	}
	(*geoLookup)(nil).abandon() // must be a safe no-op
	if called {
		t.Fatal("nil manager must not trigger a public-IP lookup")
	}
}

// abandon must cancel an in-flight lookup without waiting for it, and the
// abandoned lookup must not stamp geo on later events.
func TestGeoLookupAbandonCancelsWithoutWaiting(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	orig := lookupGeoAttributes
	lookupGeoAttributes = func(ctx context.Context, _ *http.Client) map[string]string {
		<-ctx.Done() // a blocked/censored ipwho.is: only the cancel releases it
		return nil
	}
	t.Cleanup(func() { lookupGeoAttributes = orig })

	transport := &captureTransport{}
	mgr, err := clienttelemetry.New("https://broker.test", "test", &http.Client{Transport: transport})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if _, err := mgr.BeginSession(); err != nil {
		t.Fatalf("begin session: %v", err)
	}

	g := attachGeoAttributes(mgr, nil)
	g.abandon()
	g.abandon() // idempotent

	// Well under geoLookupTimeout: only the abandon can have released it.
	select {
	case <-g.done:
	case <-time.After(2 * time.Second):
		t.Fatal("abandon did not cancel the in-flight lookup")
	}

	mgr.Record("connection_failed", "", nil, nil)
	if err := mgr.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	body := string(transport.body)
	if !strings.Contains(body, `"connection_failed"`) {
		t.Fatalf("flush missing connection_failed:\n%s", body)
	}
	if strings.Contains(body, `"country"`) {
		t.Fatalf("abandoned lookup must not stamp geo:\n%s", body)
	}
}

// finalizeConn must not block on a stuck geo lookup: teardown proceeds
// immediately, the terminal events flush without geo, and the lookup is
// cancelled rather than left running past its session.
func TestFinalizeConnAbandonsStuckGeoLookup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	orig := lookupGeoAttributes
	lookupGeoAttributes = func(ctx context.Context, _ *http.Client) map[string]string {
		<-ctx.Done()
		return nil
	}
	t.Cleanup(func() { lookupGeoAttributes = orig })

	transport := &captureTransport{}
	mgr, err := clienttelemetry.New("https://broker.test", "test", &http.Client{Transport: transport})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if _, err := mgr.BeginSession(); err != nil {
		t.Fatalf("begin session: %v", err)
	}

	s := New()
	conn := &connection{
		cancel: func() {},
		done:   make(chan struct{}),
		mgr:    mgr,
		geo:    attachGeoAttributes(mgr, nil),
	}
	s.finalizeConn(conn, "broker_fetch", errors.New("boom"))

	// finalizeConn returned while the lookup was stuck; it must also have
	// cancelled it (done closes well under geoLookupTimeout).
	select {
	case <-conn.geo.done:
	case <-time.After(2 * time.Second):
		t.Fatal("finalizeConn did not cancel the in-flight lookup")
	}
	body := string(transport.body)
	if !strings.Contains(body, `"connection_failed"`) {
		t.Fatalf("terminal flush missing connection_failed:\n%s", body)
	}
}
