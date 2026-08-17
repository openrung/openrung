package contract

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"
	"github.com/openrung/openrung/wsscore"

	"openrung/internal/client"
	"openrung/internal/relay"
)

// relayDecodeVectorsVersion pins the version of
// contract/vectors/relay_decode.json this suite expects; see the note on
// classificationVectorsVersion in internal/clienttelemetry.
const relayDecodeVectorsVersion = 2

// relayInvalidReasonMessages translates each stable rejection reason code the
// invalid vectors carry into the substring of brokerapi's Go error message
// that proves the rejection fired for the pinned cause. The codes are the
// vendored, version-pinned contract; the wording is brokerapi's own, lives
// only in this non-vendored file, and can be reworded without a vector bump
// reaching the mobile repo.
var relayInvalidReasonMessages = map[string]string{
	"relay_list_missing_required_field": "relay list requires count, server_time, and relays",
	"relay_missing_required_field":      "relay 0 is missing a required client field",
	"wss_front_missing_required_field":  "WSS front requires id, url, and protocol_version",
	"unknown_field":                     "unknown field",
}

type relayDecodeVectors struct {
	Cases   []relayDecodeCase  `json:"cases"`
	Invalid relayInvalidVector `json:"invalid"`
}

type relayDecodeCase struct {
	ID        string           `json:"id"`
	Body      json.RawMessage  `json:"body"`
	Expect    relayListExpect  `json:"expect"`
	Usability []usabilityProbe `json:"usability"`
}

type relayListExpect struct {
	Count      int           `json:"count"`
	ServerTime time.Time     `json:"server_time"`
	NotAfter   time.Time     `json:"not_after"`
	KeyID      string        `json:"key_id"`
	Channel    string        `json:"channel"`
	Limit      int           `json:"limit"`
	Relays     []relayExpect `json:"relays"`
	RelayIDs   []string      `json:"relay_ids"`
}

// relayExpect mirrors the wire field names. A pointer field is nullable: null
// means the field is absent from the body and the decoder must produce its own
// no-value representation, which in Go is the zero value.
type relayExpect struct {
	ID                 string           `json:"id"`
	Label              *string          `json:"label"`
	PublicHost         string           `json:"public_host"`
	PublicPort         int              `json:"public_port"`
	City               *string          `json:"city"`
	Country            *string          `json:"country"`
	CountryCode        *string          `json:"country_code"`
	Latitude           *float64         `json:"latitude"`
	Longitude          *float64         `json:"longitude"`
	NodeClass          *string          `json:"node_class"`
	EffectiveNodeClass string           `json:"effective_node_class"`
	Protocol           string           `json:"protocol"`
	ClientID           string           `json:"client_id"`
	RealityPublicKey   string           `json:"reality_public_key"`
	ShortID            string           `json:"short_id"`
	ServerName         string           `json:"server_name"`
	Flow               string           `json:"flow"`
	ExitMode           string           `json:"exit_mode"`
	MaxSessions        int              `json:"max_sessions"`
	MaxMbps            int              `json:"max_mbps"`
	RelayVersion       string           `json:"relay_version"`
	VolunteerVersion   string           `json:"volunteer_version"`
	Transport          *string          `json:"transport"`
	PunchCapable       *bool            `json:"punch_capable"`
	PunchEndpoint      *string          `json:"punch_endpoint"`
	WSSFronts          *[]wsscore.Front `json:"wss_fronts"`
	RegisteredAt       time.Time        `json:"registered_at"`
	LastHeartbeatAt    time.Time        `json:"last_heartbeat_at"`
	ExpiresAt          time.Time        `json:"expires_at"`
}

type usabilityProbe struct {
	Now           time.Time `json:"now"`
	Note          string    `json:"note"`
	UsableIDs     []string  `json:"usable_ids"`
	FirstUsableID *string   `json:"first_usable_id"`
}

type relayInvalidVector struct {
	Suites []string `json:"suites"`
	Cases  []struct {
		ID     string          `json:"id"`
		Reason string          `json:"reason"`
		Body   json.RawMessage `json:"body"`
	} `json:"cases"`
}

func loadRelayDecodeVectors(t *testing.T) relayDecodeVectors {
	t.Helper()
	var vectors relayDecodeVectors
	if err := LoadVersioned(RelayDecodeVectors, relayDecodeVectorsVersion, &vectors); err != nil {
		t.Fatalf("load relay decode vectors: %v", err)
	}
	return vectors
}

// TestRelayDecodeVectors runs each directory body through the real fetch path —
// brokerapi validates the wire schema and hands back the exact verified bytes,
// which the application then decodes into its own relay model — and checks the
// decoded fields and the usability filter against the shared expectations.
func TestRelayDecodeVectors(t *testing.T) {
	vectors := loadRelayDecodeVectors(t)
	if len(vectors.Cases) == 0 {
		t.Fatal("relay decode vectors carry no cases")
	}

	for _, testCase := range vectors.Cases {
		t.Run(testCase.ID, func(t *testing.T) {
			list, err := fetchRelayList(t, testCase.Body)
			if err != nil {
				t.Fatalf("fetch relay list: %v", err)
			}

			var response relay.ListResponse
			if err := json.Unmarshal(list.JSON(), &response); err != nil {
				t.Fatalf("decode verified relay list: %v", err)
			}

			expect := testCase.Expect
			if response.Count != expect.Count {
				t.Errorf("count = %d, want %d", response.Count, expect.Count)
			}
			if !response.ServerTime.Equal(expect.ServerTime) {
				t.Errorf("server_time = %s, want %s", response.ServerTime, expect.ServerTime)
			}
			if !response.NotAfter.Equal(expect.NotAfter) {
				t.Errorf("not_after = %s, want %s", response.NotAfter, expect.NotAfter)
			}
			if response.KeyID != expect.KeyID {
				t.Errorf("key_id = %q, want %q", response.KeyID, expect.KeyID)
			}
			if response.Channel != expect.Channel {
				t.Errorf("channel = %q, want %q", response.Channel, expect.Channel)
			}
			if response.Limit != expect.Limit {
				t.Errorf("limit = %d, want %d", response.Limit, expect.Limit)
			}

			checkRelays(t, response.Relays, expect)
			for _, probe := range testCase.Usability {
				checkUsability(t, response, probe)
			}
		})
	}
}

// TestRelayDecodeInvalidVectors pins the wire-schema rejections brokerapi
// applies before any client model sees a body.
func TestRelayDecodeInvalidVectors(t *testing.T) {
	vectors := loadRelayDecodeVectors(t)
	if len(vectors.Invalid.Cases) == 0 {
		t.Fatal("relay decode vectors carry no invalid cases")
	}

	for _, testCase := range vectors.Invalid.Cases {
		t.Run(testCase.ID, func(t *testing.T) {
			message, known := relayInvalidReasonMessages[testCase.Reason]
			if !known {
				t.Fatalf("invalid case carries reason code %q, which relayInvalidReasonMessages does not translate", testCase.Reason)
			}
			_, err := fetchRelayList(t, testCase.Body)
			if err == nil {
				t.Fatalf("body was accepted, want rejection (%s)", testCase.Reason)
			}
			if !strings.Contains(err.Error(), message) {
				t.Fatalf("rejected with %q, want a message containing %q (reason code %s)", err, message, testCase.Reason)
			}
		})
	}
}

func checkRelays(t *testing.T, relays []relay.Descriptor, expect relayListExpect) {
	t.Helper()

	if len(expect.RelayIDs) > 0 {
		if len(relays) != len(expect.RelayIDs) {
			t.Fatalf("decoded %d relays, want %d", len(relays), len(expect.RelayIDs))
		}
		for index, want := range expect.RelayIDs {
			if relays[index].ID != want {
				t.Errorf("relay %d id = %q, want %q (broker order must be preserved)", index, relays[index].ID, want)
			}
		}
		return
	}

	if len(relays) != len(expect.Relays) {
		t.Fatalf("decoded %d relays, want %d", len(relays), len(expect.Relays))
	}
	for index, want := range expect.Relays {
		got := relays[index]
		field := func(name string, got, want any) {
			t.Helper()
			if got != want {
				t.Errorf("relay %d %s = %v, want %v", index, name, got, want)
			}
		}
		field("id", got.ID, want.ID)
		field("label", got.Label, orZero(want.Label))
		field("public_host", got.PublicHost, want.PublicHost)
		field("public_port", got.PublicPort, want.PublicPort)
		field("city", got.City, orZero(want.City))
		field("country", got.Country, orZero(want.Country))
		field("country_code", got.CountryCode, orZero(want.CountryCode))
		field("latitude", got.Latitude, orZero(want.Latitude))
		field("longitude", got.Longitude, orZero(want.Longitude))
		field("node_class", got.NodeClass, orZero(want.NodeClass))
		field("protocol", got.Protocol, want.Protocol)
		field("client_id", got.ClientID, want.ClientID)
		field("reality_public_key", got.RealityPublicKey, want.RealityPublicKey)
		field("short_id", got.ShortID, want.ShortID)
		field("server_name", got.ServerName, want.ServerName)
		field("flow", got.Flow, want.Flow)
		field("exit_mode", got.ExitMode, want.ExitMode)
		field("max_sessions", got.MaxSessions, want.MaxSessions)
		field("max_mbps", got.MaxMbps, want.MaxMbps)
		// The model carries one version field, fed by relay_version or the
		// legacy volunteer_version alias; both expectations name the same value.
		field("relay_version", got.RelayVersion, want.RelayVersion)
		field("volunteer_version", got.RelayVersion, want.VolunteerVersion)
		field("transport", got.Transport, orZero(want.Transport))
		field("punch_capable", got.PunchCapable, orZero(want.PunchCapable))
		field("punch_endpoint", got.PunchEndpoint, orZero(want.PunchEndpoint))
		if !got.RegisteredAt.Equal(want.RegisteredAt) {
			t.Errorf("relay %d registered_at = %s, want %s", index, got.RegisteredAt, want.RegisteredAt)
		}
		if !got.LastHeartbeatAt.Equal(want.LastHeartbeatAt) {
			t.Errorf("relay %d last_heartbeat_at = %s, want %s", index, got.LastHeartbeatAt, want.LastHeartbeatAt)
		}
		if !got.ExpiresAt.Equal(want.ExpiresAt) {
			t.Errorf("relay %d expires_at = %s, want %s", index, got.ExpiresAt, want.ExpiresAt)
		}

		checkFronts(t, index, got.WSSFronts, want.WSSFronts)

		// An absent or unrecognized class is the volunteer class; the decoded
		// field stays exactly what the wire carried. This is the read-side rule
		// (relay.EffectiveNodeClass), not the ingest-side validator
		// relay.NormalizeNodeClass, which rejects an unrecognized class instead
		// of degrading it — using that one here would make the harness fail the
		// very forward-compatibility rows the contract requires.
		if effective := relay.EffectiveNodeClass(got.NodeClass); effective != want.EffectiveNodeClass {
			t.Errorf("relay %d effective node_class = %q, want %q", index, effective, want.EffectiveNodeClass)
		}
	}
}

func checkFronts(t *testing.T, index int, got []wsscore.Front, want *[]wsscore.Front) {
	t.Helper()
	if want == nil {
		if len(got) != 0 {
			t.Errorf("relay %d wss_fronts = %v, want none", index, got)
		}
		return
	}
	if len(got) != len(*want) {
		t.Fatalf("relay %d has %d wss_fronts, want %d", index, len(got), len(*want))
	}
	for frontIndex, wantFront := range *want {
		if got[frontIndex] != wantFront {
			t.Errorf("relay %d front %d = %+v, want %+v", index, frontIndex, got[frontIndex], wantFront)
		}
	}
}

func checkUsability(t *testing.T, response relay.ListResponse, probe usabilityProbe) {
	t.Helper()

	usable := make([]string, 0, len(response.Relays))
	for _, candidate := range response.Relays {
		if client.IsUsableRelay(candidate, probe.Now) {
			usable = append(usable, candidate.ID)
		}
	}
	if strings.Join(usable, ",") != strings.Join(probe.UsableIDs, ",") {
		t.Errorf("at %s usable relays = %v, want %v (%s)", probe.Now.Format(time.RFC3339), usable, probe.UsableIDs, probe.Note)
	}

	// Freshness is judged against broker time, so the probe instant is supplied
	// the same way the broker would: as the response's own server_time.
	atProbe := response
	atProbe.ServerTime = probe.Now
	selected, err := client.SelectRelay(atProbe)
	switch {
	case probe.FirstUsableID == nil:
		if err == nil {
			t.Errorf("at %s selected %q, want no usable relay", probe.Now.Format(time.RFC3339), selected.ID)
		}
	case err != nil:
		t.Errorf("at %s selection failed (%v), want %q", probe.Now.Format(time.RFC3339), err, *probe.FirstUsableID)
	case selected.ID != *probe.FirstUsableID:
		t.Errorf("at %s selected %q, want %q", probe.Now.Format(time.RFC3339), selected.ID, *probe.FirstUsableID)
	}
}

// fetchRelayList runs a vector body through brokerapi's real fetch path. The
// server is loopback, which is the development exception that accepts an
// unsigned body: the vectors carry no signature (the production signing key is
// offline), and it is the wire-schema validation and the decode that they pin.
func fetchRelayList(t *testing.T, body json.RawMessage) (brokerapi.RelayList, error) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)

	return brokerapi.NewClient(server.Client(), brokerapi.Options{Platform: brokerapi.PlatformDesktop}).
		ListRelays(t.Context(), server.URL, brokerapi.ListOptions{Limit: brokerapi.DefaultRelayLimit})
}

func orZero[T any](value *T) T {
	if value == nil {
		var zero T
		return zero
	}
	return *value
}
