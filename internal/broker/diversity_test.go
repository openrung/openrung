package broker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"openrung/internal/relay"
)

func geoRelay(id, countryCode string) relay.Descriptor {
	return relay.Descriptor{ID: id, GeoLocation: relay.GeoLocation{CountryCode: countryCode}}
}

func relayIDs(relays []relay.Descriptor) []string {
	ids := make([]string, len(relays))
	for i, desc := range relays {
		ids[i] = desc.ID
	}
	return ids
}

func TestDiversifyRelayPage(t *testing.T) {
	tests := []struct {
		name   string
		relays []relay.Descriptor
		limit  int
		want   []string
	}{
		{
			name: "page already diverse stays byte-identical",
			relays: []relay.Descriptor{
				geoRelay("kr1", "KR"), geoRelay("de1", "DE"), geoRelay("kr2", "KR"),
				geoRelay("kr3", "KR"), geoRelay("jp1", "JP"),
				geoRelay("de2", "DE"), geoRelay("fi1", "FI"),
			},
			limit: 5,
			want:  []string{"kr1", "de1", "kr2", "kr3", "jp1"},
		},
		{
			name: "below-fold europe fills the tail without touching the top",
			relays: []relay.Descriptor{
				geoRelay("kr1", "KR"), geoRelay("kr2", "KR"), geoRelay("kr3", "KR"),
				geoRelay("kr4", "KR"), geoRelay("kr5", "KR"),
				geoRelay("kr6", "KR"), geoRelay("de1", "DE"), geoRelay("fi1", "FI"),
			},
			limit: 5,
			want:  []string{"kr1", "kr2", "kr3", "kr4", "de1"},
		},
		{
			name: "two missing regions fill at most two tail slots",
			relays: []relay.Descriptor{
				geoRelay("us1", "US"), geoRelay("us2", "US"), geoRelay("us3", "US"),
				geoRelay("us4", "US"), geoRelay("us5", "US"),
				geoRelay("kr1", "KR"), geoRelay("kr2", "KR"), geoRelay("de1", "DE"),
				geoRelay("fi1", "FI"),
			},
			limit: 5,
			// Best-ranked relay per missing region, best region first at the
			// deepest repurposed slot; kr2 and fi1 never enter (one fill per
			// region), us4/us5 are displaced.
			want: []string{"us1", "us2", "us3", "de1", "kr1"},
		},
		{
			name: "three missing regions fill only two slots",
			relays: []relay.Descriptor{
				geoRelay("kr1", "KR"), geoRelay("kr2", "KR"), geoRelay("kr3", "KR"),
				geoRelay("kr4", "KR"), geoRelay("kr5", "KR"),
				geoRelay("au1", "AU"), geoRelay("us1", "US"), geoRelay("de1", "DE"),
			},
			limit: 5,
			// Global order picks which missing regions win the capped slots:
			// oceania and americas rank above europe, so de1 stays below.
			want: []string{"kr1", "kr2", "kr3", "us1", "au1"},
		},
		{
			name: "a fill never evicts a region's only representative",
			relays: []relay.Descriptor{
				geoRelay("us1", "US"), geoRelay("us2", "US"), geoRelay("us3", "US"),
				geoRelay("us4", "US"), geoRelay("kr1", "KR"),
				geoRelay("de1", "DE"),
			},
			limit: 5,
			// kr1 holds the tail slot but is asia's only page relay, so the
			// europe fill displaces us4 instead.
			want: []string{"us1", "us2", "us3", "de1", "kr1"},
		},
		{
			name: "no displaceable slot leaves the page unchanged",
			relays: []relay.Descriptor{
				geoRelay("us1", "US"), geoRelay("kr1", "KR"), geoRelay("de1", "DE"),
				geoRelay("au1", "AU"),
			},
			limit: 3,
			// Every tail relay is its region's only representative, so the
			// oceania fill has no victim and the page stays the global head.
			want: []string{"us1", "kr1", "de1"},
		},
		{
			name: "fleet smaller than the page is untouched",
			relays: []relay.Descriptor{
				geoRelay("kr1", "KR"), geoRelay("kr2", "KR"), geoRelay("de1", "DE"),
			},
			limit: 5,
			want:  []string{"kr1", "kr2", "de1"},
		},
		{
			name: "unknown country codes never win a diversity slot",
			relays: []relay.Descriptor{
				geoRelay("kr1", "KR"), geoRelay("kr2", "KR"), geoRelay("kr3", "KR"),
				geoRelay("kr4", "KR"), geoRelay("kr5", "KR"),
				geoRelay("mystery1", ""), geoRelay("mystery2", "XX"), geoRelay("de1", "DE"),
			},
			limit: 5,
			want:  []string{"kr1", "kr2", "kr3", "kr4", "de1"},
		},
		{
			name: "single-slot page never displaces the global best",
			relays: []relay.Descriptor{
				geoRelay("kr1", "KR"), geoRelay("de1", "DE"),
			},
			limit: 1,
			want:  []string{"kr1"},
		},
		{
			name: "two-slot page fills only the second slot",
			relays: []relay.Descriptor{
				geoRelay("us1", "US"), geoRelay("us2", "US"),
				geoRelay("kr1", "KR"), geoRelay("de1", "DE"),
			},
			limit: 2,
			want:  []string{"us1", "kr1"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := diversifyRelayPage(tc.relays, tc.limit)
			page := got
			if len(page) > tc.limit {
				page = page[:tc.limit]
			}
			gotIDs := relayIDs(page)
			if len(gotIDs) != len(tc.want) {
				t.Fatalf("page = %v, want %v", gotIDs, tc.want)
			}
			for i, id := range tc.want {
				if gotIDs[i] != id {
					t.Fatalf("page = %v, want %v", gotIDs, tc.want)
				}
			}
			if len(got) != len(tc.relays) {
				t.Fatalf("full list length changed: %d, want %d", len(got), len(tc.relays))
			}
		})
	}
}

// TestDiversityFillSurvivesWSSReservation pins the interplay between the two
// page-composition passes on the live fleet's shape: a head of WSS-less
// volunteers, the diversity fill also WSS-less, and the only WSS-capable
// foundation relay below the fold. The fill must move to the second-to-last
// slot so the WSS reservation's overwrite of the last slot cannot cancel it.
func TestDiversityFillSurvivesWSSReservation(t *testing.T) {
	wssJP := relay.Descriptor{
		ID: "jp-wss", NodeClass: relay.NodeClassFoundation, Transport: relay.TransportDirect,
		ExitMode: relay.ExitModeDirect, PublicPort: 443, IdentityPublicKey: "identity",
		WSSFronts:   []relay.WSSFrontDescriptor{{ID: "front-a", ProtocolVersion: relay.WSSProtocolVersion}},
		GeoLocation: relay.GeoLocation{CountryCode: "JP"},
	}
	relays := []relay.Descriptor{
		geoRelay("kr1", "KR"), geoRelay("kr2", "KR"), geoRelay("kr3", "KR"),
		geoRelay("kr4", "KR"), geoRelay("kr5", "KR"),
		geoRelay("kr6", "KR"), geoRelay("de1", "DE"), wssJP,
	}
	page := reserveWSSCandidate(diversifyRelayPage(relays, 5), 5)
	want := []string{"kr1", "kr2", "kr3", "de1", "jp-wss"}
	got := relayIDs(page)
	if len(got) != len(want) {
		t.Fatalf("page = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("page = %v, want %v", got, want)
		}
	}
}

func TestDiversifyRelayPageIsDeterministic(t *testing.T) {
	relays := []relay.Descriptor{
		geoRelay("kr1", "KR"), geoRelay("kr2", "KR"), geoRelay("kr3", "KR"),
		geoRelay("kr4", "KR"), geoRelay("kr5", "KR"),
		geoRelay("de1", "DE"), geoRelay("de2", "DE"),
	}
	first := relayIDs(diversifyRelayPage(relays, 5))
	for i := 0; i < 10; i++ {
		again := relayIDs(diversifyRelayPage(relays, 5))
		for j := range first {
			if again[j] != first[j] {
				t.Fatalf("run %d produced %v, first run produced %v", i, again, first)
			}
		}
	}
}

// registerGeoFleet fills a store with count KR relays plus one DE relay
// carrying the oldest heartbeat. With no telemetry every composite score ties,
// heartbeat recency breaks the tie in both ranking modes, and the DE relay
// ranks dead last: the default page head is Korea-only with the EU relay
// below the fold.
func registerGeoFleet(t *testing.T, store *Store, count int) {
	t.Helper()
	now := time.Now().UTC()
	for i := 0; i <= count; i++ {
		req := validRegisterRequest()
		req.PublicHost = fmt.Sprintf("203.0.113.%d", i+1)
		desc, err := store.Register(req, now.Add(-time.Duration(i)*time.Minute), time.Hour)
		if err != nil {
			t.Fatalf("register relay %d: %v", i, err)
		}
		countryCode := "KR"
		if i == count {
			countryCode = "DE"
		}
		if err := store.UpdateGeo(desc.ID, desc.LeaseToken, relay.GeoLocation{CountryCode: countryCode}); err != nil {
			t.Fatalf("set relay %d geo: %v", i, err)
		}
	}
}

func listedCountryCodes(t *testing.T, server http.Handler) []string {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/relays", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("list relays: expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var listed struct {
		Relays []relay.Descriptor `json:"relays"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode relay list: %v", err)
	}
	codes := make([]string, len(listed.Relays))
	for i, desc := range listed.Relays {
		codes[i] = desc.CountryCode
	}
	return codes
}

func TestListRelaysPageDiversitySlots(t *testing.T) {
	store := NewStore()
	registerGeoFleet(t, store, 6)
	server := NewServer(store, Config{SigningSeed: testSigningSeed()})

	codes := listedCountryCodes(t, server)
	want := []string{"KR", "KR", "KR", "KR", "DE"}
	if len(codes) != len(want) {
		t.Fatalf("page countries = %v, want %v", codes, want)
	}
	for i := range want {
		if codes[i] != want[i] {
			t.Fatalf("page countries = %v, want %v", codes, want)
		}
	}
}

func TestListRelaysPageDiversityDisabledAndLegacyServeGlobalHead(t *testing.T) {
	tests := []struct {
		name string
		mode RankingMode
		cfg  Config
	}{
		{
			name: "flag disabled",
			mode: RankingModeGlobal,
			cfg:  Config{SigningSeed: testSigningSeed(), RelayPageDiversityDisabled: true},
		},
		{
			name: "legacy ranking mode",
			mode: RankingModeLegacy,
			cfg:  Config{SigningSeed: testSigningSeed(), RelayRanking: RankingModeLegacy},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := NewStoreWithRanking(tc.mode)
			registerGeoFleet(t, store, 6)
			server := NewServer(store, tc.cfg)
			for _, code := range listedCountryCodes(t, server) {
				if code != "KR" {
					t.Fatalf("page must be the untouched ranking head, found country %q", code)
				}
			}
		})
	}
}
