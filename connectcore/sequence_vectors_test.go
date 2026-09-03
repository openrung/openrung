package connectcore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"
	"github.com/openrung/openrung/punchcore"

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
//	cd connectcore && UPDATE_SEQUENCE_VECTORS=1 go test -run TestEventSequenceVectors .
//
// (from inside the module — connectcore is nested, and a ./connectcore
// pattern from the repo root resolves only through a local gitignored
// go.work). It rewrites every scenario's "expect" block from an actual run
// (then re-run without the variable to verify the freshly embedded copy).
// Editing the file — regenerated or not — means bumping its version, this
// suite's pinned constant, and the vendored copies, like every other vector
// file.
const eventSequenceVectorsVersion = 1

// sequenceVectorFile mirrors contract/vectors/event_sequence.json exactly.
// The regeneration path marshals this struct back to the file, so every field
// the file carries must round-trip through it; the format prose is a
// json.RawMessage so its subtree round-trips verbatim by construction instead
// of through a mirror struct that could drift.
type sequenceVectorFile struct {
	Version    int                `json:"version"`
	Contract   string             `json:"contract"`
	Suites     []string           `json:"suites"`
	Comment    string             `json:"comment"`
	Format     json.RawMessage    `json:"format"`
	Regenerate string             `json:"regenerate"`
	Scenarios  []sequenceScenario `json:"scenarios"`
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
	Dials         []sequenceDial `json:"dials,omitempty"`
	Punch         *sequencePunch `json:"punch,omitempty"`
	HoldReadiness bool           `json:"hold_readiness,omitempty"`
	TelemetryHeld bool           `json:"telemetry_held_until_session_end,omitempty"`
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

// sequenceNotice is the notice projection: the structural fields only.
// Notice.Reason is deliberately absent — it is human-readable presentation
// (engine display prose, or a wrapped failure's error text), excluded for the
// same reason the event projection excludes failure_detail; the trigger
// distinctions the contract needs ride the telemetry attributes instead
// (transport_session_ended's trigger, punch_failed's reason token). Wait
// durations are timing, not sequence, and are likewise excluded.
type sequenceNotice struct {
	Kind        string `json:"kind"`
	RelayID     string `json:"relay_id,omitempty"`
	FromRelayID string `json:"from_relay_id,omitempty"`
	FrontID     string `json:"front_id,omitempty"`
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

// scriptedError renders a fixed message while unwrapping to its typed cause:
// classification sees the canonical errno, while the rendered text — which is
// never part of the contract (the projections exclude error prose) — stays
// deterministic for debugging instead of varying with the platform's errno
// strings (Windows spells them differently).
type scriptedError struct {
	msg   string
	cause error
}

func (e scriptedError) Error() string { return e.msg }
func (e scriptedError) Unwrap() error { return e.cause }

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
		return scriptedError{msg: "scripted dial: connection refused", cause: syscall.ECONNREFUSED}
	case "unclassified", "":
		return errors.New("scripted failure with no classifiable shape")
	}
	t.Fatalf("scenario scripts unknown cause %q", cause)
	return nil
}

const sequenceStepTimeout = 10 * time.Second

func TestEventSequenceVectors(t *testing.T) {
	// Strict: the regeneration path marshals sequenceVectorFile back over the
	// file, so a JSON field the struct does not mirror would be silently
	// deleted on the next regen — an unknown field fails the load instead.
	var file sequenceVectorFile
	if err := contract.LoadVersionedStrict(contract.EventSequenceVectors, eventSequenceVectorsVersion, &file); err != nil {
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
	ran := 0
	for i := range file.Scenarios {
		sc := &file.Scenarios[i]
		t.Run(sc.ID, func(t *testing.T) {
			ran++
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
		// The file is rewritten as a whole, so a partial run must not write:
		// a -run filter that skips scenarios would freeze the skipped ones at
		// whatever the loaded copy held without ever having run them.
		if ran != len(file.Scenarios) {
			t.Fatalf("not rewriting the vector file: only %d of %d scenarios ran — drop the subtest filter for UPDATE_SEQUENCE_VECTORS", ran, len(file.Scenarios))
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false) // prose stays readable: no <-style escapes
		enc.SetIndent("", "  ")
		if err := enc.Encode(file); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join("contract", "vectors", contract.EventSequenceVectors)
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
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
	sink := newTelemetrySink(t)
	if sc.Script.TelemetryHeld {
		// Held until the batch carrying the session's connection_ended — the
		// engine-ordered release, so on EVERY interleaving only the terminal
		// flush can deliver, and it delivers the whole session as one backlog.
		sink.holdUntilEvent("connection_ended")
	}

	// The directory: hosts are the driver's business (the file scripts relay
	// identities, not addresses), assigned deterministically by position. The
	// scripted errors are built here, on the test goroutine, because
	// scriptedCause fails the test on an unknown token and the dial stub runs
	// on engine goroutines, where t.Fatalf must never be called.
	hostCause := map[string]error{}
	frontFor := map[string]brokerapi.RelayWSSFront{}
	relayIDs := map[string]bool{}
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
		relayIDs[row.ID] = true
		for _, dial := range sc.Script.Dials {
			if dial.Relay == row.ID {
				hostCause[host] = scriptedCause(t, dial.Cause)
			}
		}
	}
	for _, dial := range sc.Script.Dials {
		if !relayIDs[dial.Relay] {
			t.Fatalf("script dials relay %q, which the directory does not serve", dial.Relay)
		}
	}

	s, events := newLadderService(t, func() []brokerapi.RelayDescriptor { return relays })

	// Periodic activity must not fire on its own inside a scenario: sweeps and
	// heartbeats run only where a step provokes them, so the observed streams
	// are functions of the script alone. The network-liveness probe is scripted
	// always-alive — recovery gates never hold a scenario on real dials — and
	// the geo lookup is already stubbed per engine by newLadderService.
	s.healthTick = time.Hour
	s.heartbeatTick = time.Hour
	s.checkNetworkAlive = func(context.Context, []string) bool { return true }

	s.dialRelay = func(_ context.Context, host string, _ int) (int64, error) {
		if err, ok := hostCause[host]; ok {
			return 0, err
		}
		return 1, nil
	}

	// Unbuffered: a crash_tunnel step hands its error directly to a live
	// tunnel run, so a crash nothing is running to receive fails the step
	// rather than silently poisoning a later run.
	crash := make(chan error)
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

	// With hold_readiness, every readiness probe blocks until release_ready;
	// probes after the release pass immediately — the shared holdReadiness
	// gate (also used by the network epoch tests), whose held channel only
	// ever signals for a probe that actually held.
	var readyGate <-chan struct{}
	var releaseReady func()
	readyReleased := false
	if sc.Script.HoldReadiness {
		readyGate, releaseReady = holdReadiness(s)
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
			awaitSequenceCondition(t, fail,
				func() bool {
					return occurrences(collapseStatuses(events.statesSnapshot()), step.Status) >= max(step.Count, 1)
				},
				func() string {
					return fmt.Sprintf("status %q (x%d); statuses so far: %v",
						step.Status, max(step.Count, 1), collapseStatuses(events.statesSnapshot()))
				})
		case "await_notice":
			awaitSequenceCondition(t, fail,
				func() bool {
					return len(events.noticesOf(NoticeKind(step.Kind))) >= max(step.Count, 1)
				},
				func() string {
					kinds := []string{}
					for _, notice := range events.noticesSnapshot() {
						kinds = append(kinds, string(notice.Kind))
					}
					return fmt.Sprintf("notice %q (x%d); notices so far: %v", step.Kind, max(step.Count, 1), kinds)
				})
		case "network":
			if step.Up == nil {
				fail("network step needs an explicit \"up\"")
			}
			s.UpdateNetworkState(NetworkState{Up: *step.Up, Fingerprint: step.Fingerprint})
		case "crash_tunnel":
			// The send must be consumed by a live tunnel run; a bounded wait
			// turns a mis-scripted crash (no session, or two crashes for one
			// run) into a step failure instead of a wedged test goroutine.
			select {
			case crash <- scriptedCause(t, step.Cause):
			case <-time.After(sequenceStepTimeout):
				fail("no tunnel run consumed the previous crash; is a session live?")
			}
		case "await_ready_hold":
			if !sc.Script.HoldReadiness {
				fail("await_ready_hold without script.hold_readiness")
			}
			if readyReleased {
				fail("await_ready_hold after release_ready: nothing will hold again")
			}
			select {
			case <-readyGate:
			case <-time.After(sequenceStepTimeout):
				fail("timed out waiting for the held readiness probe")
			}
		case "release_ready":
			if !sc.Script.HoldReadiness {
				fail("release_ready without script.hold_readiness")
			}
			if readyReleased {
				fail("release_ready scripted twice")
			}
			readyReleased = true
			releaseReady()
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

// awaitSequenceCondition is the one polling loop behind the await_* steps:
// poll cond until it holds or the step deadline passes, failing with what was
// actually observed so a timeout names the divergence, not just the wait.
func awaitSequenceCondition(t *testing.T, fail func(string, ...any), cond func() bool, describe func() string) {
	t.Helper()
	deadline := time.Now().Add(sequenceStepTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			fail("timed out waiting for %s", describe())
		}
		time.Sleep(2 * time.Millisecond)
	}
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
