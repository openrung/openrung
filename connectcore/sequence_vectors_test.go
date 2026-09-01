package connectcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"
	"github.com/openrung/openrung/punchcore"

	"github.com/openrung/openrung/connectcore/clienttelemetry"
	"github.com/openrung/openrung/connectcore/contract"
)

// This file is the Go runner for the event-sequence contract vectors (ADR-003
// A4): golden scenarios that freeze the engine state machine's observable
// behavior — status transitions, typed notices, and telemetry events with
// their failure tokens — as data every suite that binds the engine must
// reproduce. The scenario format itself is documented inside the vector file
// (contract/vectors/event_sequence.json, the single copy of that spec); this
// runner maps the scripted transport outcomes onto the engine's test seams,
// which is why it lives inside the package rather than beside the other
// contract suites.
//
// The expected sequences are GENERATED from the engine, not hand-written:
//
//	UPDATE_SEQUENCE_VECTORS=1 go test ./connectcore -run TestEventSequenceVectors
//
// rewrites every scenario's "expect" block from an actual run (then re-run
// without the variable to verify the freshly embedded copy). Editing the file
// — regenerated or not — means bumping its version, this suite's pinned
// constant, and the vendored copies, like every other vector file.
const eventSequenceVectorsVersion = 1

// sequenceVectorFile mirrors contract/vectors/event_sequence.json exactly.
// The regeneration path marshals this struct back to the file, so every field
// the file carries — the prose included — must round-trip through it.
type sequenceVectorFile struct {
	Version       int                 `json:"version"`
	Contract      string              `json:"contract"`
	Suites        []string            `json:"suites"`
	PendingSuites []string            `json:"pending_suites"`
	Comment       string              `json:"comment"`
	Format        sequenceFormatNotes `json:"format"`
	Regenerate    string              `json:"regenerate"`
	Scenarios     []sequenceScenario  `json:"scenarios"`
}

type sequenceFormatNotes struct {
	Directory   string `json:"directory"`
	Script      string `json:"script"`
	Causes      string `json:"causes"`
	Steps       string `json:"steps"`
	Expect      string `json:"expect"`
	Projection  string `json:"attribute_projection"`
	Determinism string `json:"determinism"`
}

type sequenceScenario struct {
	ID          string           `json:"id"`
	Description string           `json:"description"`
	Directory   []sequenceRelay  `json:"directory"`
	Script      sequenceScript   `json:"script"`
	Steps       []sequenceStep   `json:"steps"`
	Expect      sequenceExpected `json:"expect"`
}

type sequenceRelay struct {
	ID           string   `json:"id"`
	CountryCode  string   `json:"country_code"`
	City         string   `json:"city,omitempty"`
	Country      string   `json:"country"`
	WSSFrontIDs  []string `json:"wss_fronts,omitempty"`
	PunchCapable bool     `json:"punch_capable,omitempty"`
}

type sequenceScript struct {
	Dials          []sequenceDial `json:"dials,omitempty"`
	Punch          *sequencePunch `json:"punch,omitempty"`
	HoldFirstReady bool           `json:"hold_first_ready,omitempty"`
	TelemetryHeld  bool           `json:"telemetry_held_until_released,omitempty"`
}

type sequenceDial struct {
	Relay string `json:"relay"`
	Cause string `json:"cause"`
}

type sequencePunch struct {
	Reason string `json:"reason"`
}

type sequenceStep struct {
	Do            string `json:"do"`
	Status        string `json:"status,omitempty"`
	Kind          string `json:"kind,omitempty"`
	Count         int    `json:"count,omitempty"`
	Up            *bool  `json:"up,omitempty"`
	Fingerprint   string `json:"fingerprint,omitempty"`
	Cause         string `json:"cause,omitempty"`
	FlushBudgetMS int    `json:"flush_budget_ms,omitempty"`
}

type sequenceExpected struct {
	Statuses []string         `json:"statuses"`
	Notices  []sequenceNotice `json:"notices"`
	Events   []sequenceEvent  `json:"events"`
}

type sequenceNotice struct {
	Kind        string `json:"kind"`
	RelayID     string `json:"relay_id,omitempty"`
	FromRelayID string `json:"from_relay_id,omitempty"`
	FrontID     string `json:"front_id,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Failures    int    `json:"failures,omitempty"`
	Threshold   int    `json:"threshold,omitempty"`
}

type sequenceEvent struct {
	Event      string            `json:"event"`
	RelayID    string            `json:"relay_id,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

// sequenceAttrKeys is the attribute projection: the contract-bearing keys the
// expected events carry, compared exactly. Everything else on a real event —
// failure_detail (host error text), geo, platform, measurements — is
// host-variant metadata and deliberately outside the sequence contract.
var sequenceAttrKeys = []string{
	"transport", "from_transport", "to_transport", "front_id", "trigger",
	"reason", "nat_class", "from_relay_id", "failure_reason", "failure_stage",
}

// scriptedCause maps a scenario's cause token onto the canonical error every
// driver must construct for it. The tokens double as the classification the
// failure will carry, pinned by the classification vectors — except
// "unclassified", the deliberate stand-in for runtime-specific error shapes
// (a tunnel process death) whose classification is the runtime's business,
// not the state machine's.
func scriptedCause(t *testing.T, cause string) error {
	t.Helper()
	switch cause {
	case "connection_refused":
		return fmt.Errorf("scripted dial: %w", syscall.ECONNREFUSED)
	case "unclassified", "":
		return errors.New("scripted failure with no classifiable shape")
	}
	t.Fatalf("scenario scripts unknown cause %q", cause)
	return nil
}

const sequenceStepTimeout = 10 * time.Second

// sequenceTelemetrySink is the loopback broker for sequence scenarios: it
// records every telemetry event in arrival order (deduplicating by event id,
// like the real ingest), and can hold uploads — refusing every batch with 503
// — until a release_telemetry step lets them through, which is how the
// shutdown-with-pending-telemetry scenario builds its backlog.
type sequenceTelemetrySink struct {
	mu     sync.Mutex
	accept bool
	events []clienttelemetry.Event
	seen   map[string]bool
	srv    *httptest.Server
}

func newSequenceTelemetrySink(t *testing.T, accept bool) *sequenceTelemetrySink {
	t.Helper()
	sink := &sequenceTelemetrySink{accept: accept, seen: map[string]bool{}}
	sink.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		if !sink.accept {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var batch struct {
			Events []clienttelemetry.Event `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&batch); err == nil {
			for _, event := range batch.Events {
				if sink.seen[event.EventID] {
					continue
				}
				sink.seen[event.EventID] = true
				sink.events = append(sink.events, event)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(sink.srv.Close)
	return sink
}

func (sink *sequenceTelemetrySink) release() {
	sink.mu.Lock()
	sink.accept = true
	sink.mu.Unlock()
}

func (sink *sequenceTelemetrySink) snapshot() []clienttelemetry.Event {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	out := make([]clienttelemetry.Event, len(sink.events))
	copy(out, sink.events)
	return out
}

func TestEventSequenceVectors(t *testing.T) {
	var file sequenceVectorFile
	if err := contract.LoadVersioned(contract.EventSequenceVectors, eventSequenceVectorsVersion, &file); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, sc := range file.Scenarios {
		if seen[sc.ID] {
			t.Fatalf("scenario id %q appears twice", sc.ID)
		}
		seen[sc.ID] = true
	}

	update := os.Getenv("UPDATE_SEQUENCE_VECTORS") != ""
	for i := range file.Scenarios {
		sc := &file.Scenarios[i]
		t.Run(sc.ID, func(t *testing.T) {
			got := runSequenceScenario(t, sc)
			if update {
				sc.Expect = got
				return
			}
			if !reflect.DeepEqual(got, sc.Expect) {
				t.Fatalf("observed sequence diverges from the vector.\n got: %s\nwant: %s\nIf the engine change is deliberate, regenerate with UPDATE_SEQUENCE_VECTORS=1 and bump the file's version, this suite's pin, and the vendored copies.",
					mustSequenceJSON(got), mustSequenceJSON(sc.Expect))
			}
		})
	}

	if update {
		if t.Failed() {
			t.Fatal("not rewriting the vector file: a scenario failed to run")
		}
		blob, err := json.MarshalIndent(file, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		path := "contract/vectors/event_sequence.json"
		if err := os.WriteFile(path, append(blob, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("rewrote %s from this run; re-run without UPDATE_SEQUENCE_VECTORS to verify the embedded copy", path)
	}
}

// runSequenceScenario builds an engine from the scenario's script, drives the
// step timeline, and returns the projected observation — the three ordered
// output streams the contract freezes.
func runSequenceScenario(t *testing.T, sc *sequenceScenario) sequenceExpected {
	t.Helper()
	sink := newSequenceTelemetrySink(t, !sc.Script.TelemetryHeld)

	// The directory: hosts are the driver's business (the file scripts relay
	// identities, not addresses), assigned deterministically by position. The
	// scripted errors are built here, on the test goroutine, because
	// scriptedCause fails the test on an unknown token and the dial stub runs
	// on engine goroutines, where t.Fatalf must never be called.
	hostCause := map[string]error{}
	frontFor := map[string]brokerapi.RelayWSSFront{}
	relays := make([]brokerapi.RelayDescriptor, 0, len(sc.Directory))
	for i, row := range sc.Directory {
		host := fmt.Sprintf("127.0.0.%d", 10+i)
		var relay brokerapi.RelayDescriptor
		if len(row.WSSFrontIDs) > 0 {
			fronts := make([]brokerapi.RelayWSSFront, 0, len(row.WSSFrontIDs))
			for _, frontID := range row.WSSFrontIDs {
				front := testWSSFront(frontID, testWSSFrontAURL)
				fronts = append(fronts, front)
				frontFor[frontID] = front
			}
			relay = relayWithWSS(row.ID, row.CountryCode, row.City, row.Country, host, fronts...)
		} else {
			relay = relayAt(row.ID, row.CountryCode, row.City, row.Country, host)
		}
		relay.PunchCapable = row.PunchCapable
		relays = append(relays, relay)
		for _, dial := range sc.Script.Dials {
			if dial.Relay == row.ID {
				hostCause[host] = scriptedCause(t, dial.Cause)
			}
		}
	}

	s, events := newLadderService(t, func() []brokerapi.RelayDescriptor { return relays })

	// Periodic activity must not fire on its own inside a scenario: sweeps and
	// heartbeats run only where a step provokes them, so the observed streams
	// are functions of the script alone.
	s.healthTick = time.Hour
	s.heartbeatTick = time.Hour
	s.networkRetryDelay = 2 * time.Millisecond
	s.checkNetworkAlive = func(context.Context, []string) bool { return true }

	// The public-IP geo lookup is best-effort metadata, not sequence behavior;
	// stub it out so no scenario touches the network or races geo attributes
	// onto events (the projection excludes them anyway).
	restoreGeo := lookupGeoAttributes
	lookupGeoAttributes = func(context.Context, *http.Client) map[string]string { return nil }
	t.Cleanup(func() { lookupGeoAttributes = restoreGeo })

	s.dialRelay = func(_ context.Context, host string, _ int) (int64, error) {
		if err, ok := hostCause[host]; ok {
			return 0, err
		}
		return 1, nil
	}

	crash := make(chan error, 1)
	s.TunnelRuntime = runFuncRuntime(func(ctx context.Context, _ []byte) error {
		select {
		case err := <-crash:
			return err
		case <-ctx.Done():
			return nil
		}
	})

	if len(frontFor) > 0 {
		var tickets int
		var ticketMu sync.Mutex
		s.requestWSSTicket = func(_ context.Context, _ string, request brokerapi.WSSTicketRequest, _, _ string) (brokerapi.WSSTicketResponse, error) {
			front, ok := frontFor[request.FrontID]
			if !ok {
				return brokerapi.WSSTicketResponse{}, fmt.Errorf("ticket requested for unscripted front %q", request.FrontID)
			}
			ticketMu.Lock()
			tickets++
			value := fmt.Sprintf("scripted-ticket-%d", tickets)
			ticketMu.Unlock()
			return successfulWSSTicket(front, value), nil
		}
		s.dialWSS = func(context.Context, string, string) (wssBridge, error) {
			return newFakeWSSBridge(), nil
		}
	}

	if sc.Script.Punch != nil {
		reason := sc.Script.Punch.Reason
		s.PunchEnabled = true
		s.PunchEstablisher = func(context.Context, punchcore.HubClient, string) (*PunchPath, punchcore.PunchResult, error) {
			return nil, punchcore.PunchResult{Reason: reason}, errors.New("scripted punch failure")
		}
	}

	readyGate := make(chan struct{}, 1)
	releaseReady := make(chan struct{})
	if sc.Script.HoldFirstReady {
		s.tunnelReady = func(ctx context.Context, _ int) error {
			select {
			case readyGate <- struct{}{}:
			default:
			}
			select {
			case <-releaseReady:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}

	for index, step := range sc.Steps {
		fail := func(format string, args ...any) {
			t.Helper()
			t.Fatalf("step %d (%s): %s", index+1, step.Do, fmt.Sprintf(format, args...))
		}
		switch step.Do {
		case "connect":
			if err := s.Connect(sink.srv.URL, "", ""); err != nil {
				fail("connect: %v", err)
			}
		case "await_status":
			count := max(step.Count, 1)
			deadline := time.Now().Add(sequenceStepTimeout)
			for occurrences(collapseStatuses(events.statesSnapshot()), step.Status) < count {
				if time.Now().After(deadline) {
					fail("timed out waiting for status %q (x%d); statuses so far: %v",
						step.Status, count, collapseStatuses(events.statesSnapshot()))
				}
				time.Sleep(2 * time.Millisecond)
			}
		case "await_notice":
			count := max(step.Count, 1)
			deadline := time.Now().Add(sequenceStepTimeout)
			for len(events.noticesOf(NoticeKind(step.Kind))) < count {
				if time.Now().After(deadline) {
					fail("timed out waiting for notice %q (x%d)", step.Kind, count)
				}
				time.Sleep(2 * time.Millisecond)
			}
		case "network":
			if step.Up == nil {
				fail("network step needs an explicit \"up\"")
			}
			s.UpdateNetworkState(NetworkState{Up: *step.Up, Fingerprint: step.Fingerprint})
		case "crash_tunnel":
			crash <- scriptedCause(t, step.Cause)
		case "await_ready_hold":
			select {
			case <-readyGate:
			case <-time.After(sequenceStepTimeout):
				fail("timed out waiting for the held readiness probe")
			}
		case "release_ready":
			close(releaseReady)
		case "release_telemetry":
			sink.release()
		case "disconnect":
			if err := s.Disconnect(); err != nil {
				fail("disconnect: %v", err)
			}
		case "shutdown":
			if err := s.Shutdown(time.Duration(step.FlushBudgetMS) * time.Millisecond); err != nil {
				fail("shutdown flush: %v", err)
			}
		default:
			fail("unknown step")
		}
	}

	// The connect goroutine finalizes — terminal status, session-end events,
	// terminal flush — before clearing the connection, so idle means every
	// stream is complete.
	waitIdle(t, s)

	got := sequenceExpected{
		Statuses: collapseStatuses(events.statesSnapshot()),
		Notices:  []sequenceNotice{},
		Events:   []sequenceEvent{},
	}
	for _, notice := range events.noticesSnapshot() {
		got.Notices = append(got.Notices, sequenceNotice{
			Kind:        string(notice.Kind),
			RelayID:     notice.RelayID,
			FromRelayID: notice.FromRelayID,
			FrontID:     notice.FrontID,
			Reason:      notice.Reason,
			Failures:    notice.Failures,
			Threshold:   notice.Threshold,
		})
	}
	for _, event := range sink.snapshot() {
		if event.Event == "session_heartbeat" {
			continue // cadence-driven, deliberately outside the sequence contract
		}
		projected := sequenceEvent{Event: event.Event, RelayID: event.RelayID}
		for _, key := range sequenceAttrKeys {
			if value, ok := event.Attributes[key]; ok {
				if projected.Attributes == nil {
					projected.Attributes = map[string]string{}
				}
				projected.Attributes[key] = value
			}
		}
		got.Events = append(got.Events, projected)
	}
	return got
}

// collapseStatuses reduces the raw state stream to its transitions: platforms
// may re-emit a state they are already in, so the contract is over distinct
// consecutive statuses, never emission counts.
func collapseStatuses(states []State) []string {
	out := []string{}
	for _, state := range states {
		status := string(state.Status)
		if len(out) == 0 || out[len(out)-1] != status {
			out = append(out, status)
		}
	}
	return out
}

func occurrences(statuses []string, status string) int {
	count := 0
	for _, s := range statuses {
		if s == status {
			count++
		}
	}
	return count
}

func mustSequenceJSON(v any) string {
	blob, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%+v", v)
	}
	return string(blob)
}
