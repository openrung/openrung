package persist

import (
	"testing"

	"openrung/desktop/proxymode"
)

func TestRecentsRoundTrip(t *testing.T) {
	store := NewInDir(t.TempDir())
	if got := store.LoadRecents(); len(got) != 0 {
		t.Fatalf("fresh store should have no recents, got %v", got)
	}
	recents := []RecentNode{{CountryCode: "JP", Label: "Tokyo, Japan", Latitude: 35.6, Longitude: 139.7}}
	if err := store.SaveRecents(recents); err != nil {
		t.Fatalf("SaveRecents: %v", err)
	}
	got := store.LoadRecents()
	if len(got) != 1 || got[0].CountryCode != "JP" || got[0].Label != "Tokyo, Japan" {
		t.Fatalf("recents round-trip mismatch: %+v", got)
	}
}

func TestPrependRecentDedupesAndCaps(t *testing.T) {
	var recents []RecentNode
	add := func(cc string) {
		recents = PrependRecent(recents, RecentNode{CountryCode: cc, Label: cc}, 3)
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
		if seen[r.CountryCode] > 1 {
			t.Fatalf("duplicate country code %q", r.CountryCode)
		}
	}
}

func TestProxySnapshotLifecycle(t *testing.T) {
	store := NewInDir(t.TempDir())
	if _, ok := store.LoadProxySnapshot(); ok {
		t.Fatal("fresh store should have no proxy snapshot")
	}
	snap := proxymode.Snapshot{
		Platform: "darwin",
		Services: []proxymode.ServiceProxyState{{Name: "Wi-Fi", WebEnabled: true, WebHost: "10.0.0.1", WebPort: 3128}},
	}
	if err := store.SaveProxySnapshot(snap); err != nil {
		t.Fatalf("SaveProxySnapshot: %v", err)
	}
	loaded, ok := store.LoadProxySnapshot()
	if !ok || loaded.Platform != "darwin" || len(loaded.Services) != 1 {
		t.Fatalf("snapshot round-trip mismatch: ok=%v %+v", ok, loaded)
	}
	if err := store.ClearProxySnapshot(); err != nil {
		t.Fatalf("ClearProxySnapshot: %v", err)
	}
	if _, ok := store.LoadProxySnapshot(); ok {
		t.Fatal("snapshot should be gone after clear")
	}
	// Clearing a missing snapshot is not an error.
	if err := store.ClearProxySnapshot(); err != nil {
		t.Fatalf("ClearProxySnapshot on missing: %v", err)
	}
}
