package connectcore

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"openrung/internal/relay"
)

func usableRelay(id, countryCode, city, country string) relay.Descriptor {
	return relay.Descriptor{
		ID:               id,
		PublicHost:       "203.0.113.5",
		PublicPort:       443,
		Protocol:         relay.ProtocolVLESSRealityVision,
		ClientID:         "uuid",
		RealityPublicKey: "pk",
		ShortID:          "sid",
		ServerName:       "sni",
		Flow:             relay.FlowVision,
		ExitMode:         relay.ExitModeDirect,
		ExpiresAt:        time.Now().Add(time.Hour),
		GeoLocation:      relay.GeoLocation{City: city, Country: country, CountryCode: countryCode, Latitude: 1, Longitude: 2},
	}
}

func listOf(relays ...relay.Descriptor) relay.ListResponse {
	return relay.ListResponse{Count: len(relays), ServerTime: time.Now(), Relays: relays}
}

func candidateIDs(cands []relay.Descriptor) []string {
	ids := make([]string, 0, len(cands))
	for _, cand := range cands {
		ids = append(ids, cand.ID)
	}
	return ids
}

func TestFilterCandidatesPinnedID(t *testing.T) {
	usable := []relay.Descriptor{usableRelay("a", "JP", "Tokyo", "Japan"), usableRelay("b", "SG", "", "Singapore")}
	got, stage, err := filterCandidates(usable, "JP", "b") // id wins over country
	if err != nil || stage != "" {
		t.Fatalf("pinned id: stage %q err %v", stage, err)
	}
	// Pinned: exactly the target, never a fallback relay.
	if len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("pinned id candidates = %v", candidateIDs(got))
	}
}

func TestFilterCandidatesPinnedIDAbsent(t *testing.T) {
	usable := []relay.Descriptor{usableRelay("a", "JP", "Tokyo", "Japan")}
	_, stage, err := filterCandidates(usable, "", "zz")
	if err == nil || stage != "relay_id_filter" {
		t.Fatalf("absent pinned id: stage %q err %v", stage, err)
	}
}

func TestFilterCandidatesCountryKeepsBrokerOrder(t *testing.T) {
	usable := []relay.Descriptor{
		usableRelay("a", "SG", "", "Singapore"),
		usableRelay("b", "JP", "Tokyo", "Japan"),
		usableRelay("c", "sg", "", "Singapore"), // case-insensitive match
		usableRelay("d", "", "", ""),            // geo-less: excluded from a targeted connect
	}
	got, stage, err := filterCandidates(usable, "sg", "")
	if err != nil || stage != "" {
		t.Fatalf("country filter: stage %q err %v", stage, err)
	}
	if ids := candidateIDs(got); len(ids) != 2 || ids[0] != "a" || ids[1] != "c" {
		t.Fatalf("country candidates = %v", ids)
	}
}

func TestFilterCandidatesCountryAbsent(t *testing.T) {
	usable := []relay.Descriptor{usableRelay("a", "JP", "Tokyo", "Japan")}
	_, stage, err := filterCandidates(usable, "US", "")
	if err == nil || stage != "relay_geo_filter" {
		t.Fatalf("absent country: stage %q err %v", stage, err)
	}
}

func TestFilterCandidatesAutoKeepsWholeList(t *testing.T) {
	usable := []relay.Descriptor{usableRelay("a", "JP", "Tokyo", "Japan"), usableRelay("b", "SG", "", "Singapore")}
	got, stage, err := filterCandidates(usable, "", "")
	if err != nil || stage != "" {
		t.Fatalf("auto: stage %q err %v", stage, err)
	}
	if ids := candidateIDs(got); len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("auto candidates = %v", ids)
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
	cands := []relay.Descriptor{
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

// testSink captures every engine event for assertions.
type testSink struct {
	mu     sync.Mutex
	states []State
	logs   []string
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
	fail := true
	next := 46685
	s.ResolveProxyPort = func() (ProxyPortResolution, error) {
		if fail {
			return ProxyPortResolution{}, errors.New("resolution unavailable")
		}
		port := next
		next++ // a later resolution would hand out a different endpoint
		return ProxyPortResolution{Port: port}, nil
	}
	if _, err := s.LocalProxyPort(); err == nil {
		t.Fatal("first failed resolution unexpectedly succeeded")
	}

	fail = false
	port, err := s.LocalProxyPort()
	if err != nil || port != 46685 {
		t.Fatalf("retry = %d, %v; want 46685, nil", port, err)
	}

	// Once resolution succeeds, later calls keep that endpoint even though the
	// resolver would now hand out a different one.
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
	restores   []OSProxySnapshot
}

func (f *fakeProxyController) Supported() bool { return f.supported }

func (f *fakeProxyController) Snapshot() (OSProxySnapshot, error) {
	return f.snap, nil
}

func (f *fakeProxyController) Set(host string, port int) error {
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
