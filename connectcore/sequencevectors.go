package connectcore

// This file is the shared runner for the event-sequence contract vectors
// (contract/vectors/event_sequence.json, ADR-003 A4): the one executable
// implementation of the scenario format documented inside that file. Two Go
// suites drive it — this repo's sequence_vectors_test.go against a bare
// engine, and the mobile repo's punchbridge suite against the engine as its
// binding constructs it (ADR-003 B1) — so the runner is exported from the
// module rather than duplicated across repos.
//
// It is exported TEST SUPPORT, not engine API: nothing here is part of the
// engine's behavioral contract, no production host may call it, and gomobile
// never binds it (dependencies of the bound package generate no surface). It
// lives inside package connectcore because scripting the transport outcomes
// requires the engine's unexported seams — the same seams the golden expects
// were generated through — and the alternatives are worse: a mobile-side
// driver at real network boundaries cannot script WSS at all
// (wsscore.NormalizeFrontURL deliberately refuses IP-literal and
// non-default-port front URLs, and that hardening must not be loosened for
// tests), and exporting raw seam setters would put far more dangerous knobs
// on the engine than one self-contained runner.
//
// The runner owns EVERY transport seam while a scenario runs — dials, tunnel
// runs, WSS tickets and bridges, the punch establisher, probes, the network
// liveness gate, periodic cadences, and the geo lookup — which is exactly the
// determinism contract the vector file's format notes state. What the
// caller's engine construction contributes is everything else: the host's
// sinks (the runner tees them, so a binding's event delivery still fires),
// platform hooks, and telemetry identity.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"syscall"
	"time"

	"github.com/openrung/openrung/brokerapi"
	"github.com/openrung/openrung/punchcore"
	"github.com/openrung/openrung/wsscore"

	"github.com/openrung/openrung/connectcore/clienttelemetry"
	"github.com/openrung/openrung/connectcore/discovery"
)

// SequenceTB is the slice of *testing.T the sequence runner needs. It is a
// local interface so this production-built file never imports package testing
// into every consumer of the module.
type SequenceTB interface {
	Helper()
	Fatalf(format string, args ...any)
	Cleanup(func())
	Setenv(key, value string)
	TempDir() string
}

// SequenceVectorFile mirrors contract/vectors/event_sequence.json exactly.
// The regeneration path (sequence_vectors_test.go) marshals this struct back
// to the file, so every field the file carries must round-trip through it;
// the format prose is a json.RawMessage so its subtree round-trips verbatim
// by construction instead of through a mirror struct that could drift.
type SequenceVectorFile struct {
	Version    int                `json:"version"`
	Contract   string             `json:"contract"`
	Suites     []string           `json:"suites"`
	Comment    string             `json:"comment"`
	Format     json.RawMessage    `json:"format"`
	Regenerate string             `json:"regenerate"`
	Scenarios  []SequenceScenario `json:"scenarios"`
}

// SequenceScenario is one scripted connect scenario; the authoritative
// field-by-field spec is the vector file's own "format" block.
type SequenceScenario struct {
	ID          string           `json:"id"`
	Description string           `json:"description"`
	Directory   []SequenceRelay  `json:"directory"`
	Script      SequenceScript   `json:"script"`
	Steps       []SequenceStep   `json:"steps"`
	Expect      SequenceExpected `json:"expect"`
}

type SequenceRelay struct {
	ID           string   `json:"id"`
	CountryCode  string   `json:"country_code"`
	City         string   `json:"city,omitempty"`
	Country      string   `json:"country"`
	WSSFrontIDs  []string `json:"wss_fronts,omitempty"`
	PunchCapable bool     `json:"punch_capable,omitempty"`
}

type SequenceScript struct {
	Dials         []SequenceDial `json:"dials,omitempty"`
	Punch         *SequencePunch `json:"punch,omitempty"`
	HoldReadiness bool           `json:"hold_readiness,omitempty"`
	TelemetryHeld bool           `json:"telemetry_held_until_session_end,omitempty"`
}

type SequenceDial struct {
	Relay string `json:"relay"`
	Cause string `json:"cause"`
}

type SequencePunch struct {
	Reason string `json:"reason"`
}

type SequenceStep struct {
	Do            string `json:"do"`
	Status        string `json:"status,omitempty"`
	Kind          string `json:"kind,omitempty"`
	Count         int    `json:"count,omitempty"`
	Up            *bool  `json:"up,omitempty"`
	Fingerprint   string `json:"fingerprint,omitempty"`
	Cause         string `json:"cause,omitempty"`
	FlushBudgetMS int    `json:"flush_budget_ms,omitempty"`
}

// SequenceExpected is the projected observation the contract freezes: three
// ordered streams, compared exactly.
type SequenceExpected struct {
	Statuses []string         `json:"statuses"`
	Notices  []SequenceNotice `json:"notices"`
	Events   []SequenceEvent  `json:"events"`
}

// SequenceNotice is the notice projection: the structural fields only.
// Notice.Reason is deliberately absent — it is human-readable presentation
// (engine display prose, or a wrapped failure's error text), excluded for the
// same reason the event projection excludes failure_detail; the trigger
// distinctions the contract needs ride the telemetry attributes instead
// (transport_session_ended's trigger, punch_failed's reason token). Wait
// durations are timing, not sequence, and are likewise excluded.
type SequenceNotice struct {
	Kind        string `json:"kind"`
	RelayID     string `json:"relay_id,omitempty"`
	FromRelayID string `json:"from_relay_id,omitempty"`
	FrontID     string `json:"front_id,omitempty"`
	Failures    int    `json:"failures,omitempty"`
	Threshold   int    `json:"threshold,omitempty"`
}

type SequenceEvent struct {
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

// sequenceScriptedError renders a fixed message while unwrapping to its typed
// cause: classification sees the canonical errno, while the rendered text —
// which is never part of the contract (the projections exclude error prose) —
// stays deterministic for debugging instead of varying with the platform's
// errno strings (Windows spells them differently).
type sequenceScriptedError struct {
	msg   string
	cause error
}

func (e sequenceScriptedError) Error() string { return e.msg }
func (e sequenceScriptedError) Unwrap() error { return e.cause }

// sequenceCause maps a scenario's cause token onto the canonical error every
// driver must construct for it. The tokens double as the classification the
// failure will carry, pinned by the classification vectors — except
// "unclassified", the deliberate stand-in for runtime-specific error shapes
// (a tunnel process death) whose classification is the runtime's business,
// not the state machine's.
func sequenceCause(tb SequenceTB, cause string) error {
	tb.Helper()
	switch cause {
	case "connection_refused":
		return sequenceScriptedError{msg: "scripted dial: connection refused", cause: syscall.ECONNREFUSED}
	case "unclassified", "":
		return errors.New("scripted failure with no classifiable shape")
	}
	tb.Fatalf("scenario scripts unknown cause %q", cause)
	return nil
}

const sequenceStepTimeout = 10 * time.Second

// sequenceRecorder tees the engine's sink streams into ordered recordings
// while still delivering every event to the host's own sink, so a binding's
// event path keeps firing while a scenario runs.
type sequenceRecorder struct {
	mu      sync.Mutex
	states  []State
	notices []Notice
	inner   EventSink
}

func (r *sequenceRecorder) StateChanged(state State) {
	r.mu.Lock()
	r.states = append(r.states, state)
	r.mu.Unlock()
	if r.inner != nil {
		r.inner.StateChanged(state)
	}
}

func (r *sequenceRecorder) Log(entry LogEntry) {
	if r.inner != nil {
		r.inner.Log(entry)
	}
}

func (r *sequenceRecorder) Notice(notice Notice) {
	r.mu.Lock()
	r.notices = append(r.notices, notice)
	r.mu.Unlock()
	if sink, ok := r.inner.(NoticeSink); ok {
		sink.Notice(notice)
	}
}

func (r *sequenceRecorder) statesSnapshot() []State {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]State, len(r.states))
	copy(out, r.states)
	return out
}

func (r *sequenceRecorder) noticesOf(kind NoticeKind) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, notice := range r.notices {
		if notice.Kind == kind {
			count++
		}
	}
	return count
}

func (r *sequenceRecorder) noticeKinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	kinds := make([]string, 0, len(r.notices))
	for _, notice := range r.notices {
		kinds = append(kinds, string(notice.Kind))
	}
	return kinds
}

func (r *sequenceRecorder) noticesSnapshot() []Notice {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Notice, len(r.notices))
	copy(out, r.notices)
	return out
}

// sequenceBrokerSink is the runner's loopback telemetry broker: it records
// every event in arrival order (deduplicating by event id, like the real
// ingest), and while held it refuses every upload with 503 until a batch
// carries the named event — the engine-ordered release the shutdown scenario
// scripts, so only the terminal flush can deliver on any interleaving.
type sequenceBrokerSink struct {
	mu           sync.Mutex
	held         bool
	releaseEvent string
	events       []clienttelemetry.Event
	seen         map[string]bool
	srv          *httptest.Server
}

func newSequenceBrokerSink(tb SequenceTB) *sequenceBrokerSink {
	tb.Helper()
	sink := &sequenceBrokerSink{seen: map[string]bool{}}
	sink.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Decode before taking the lock: a body read is a network operation,
		// and holding the mutex across it would serialize every upload.
		var batch struct {
			Events []clienttelemetry.Event `json:"events"`
		}
		decodeErr := json.NewDecoder(r.Body).Decode(&batch)

		sink.mu.Lock()
		defer sink.mu.Unlock()
		if sink.held {
			releases := false
			for _, event := range batch.Events {
				if decodeErr == nil && event.Event == sink.releaseEvent {
					releases = true
					break
				}
			}
			if !releases {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			sink.held = false
		}
		if decodeErr != nil {
			// Never acknowledge a batch that was not recorded — and refuse it
			// as transient (503), never a 4xx the persistent outbox's poison
			// classifier would read as a permanent rejection and discard on.
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		for _, event := range batch.Events {
			if sink.seen[event.EventID] {
				continue
			}
			sink.seen[event.EventID] = true
			sink.events = append(sink.events, event)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	tb.Cleanup(sink.srv.Close)
	return sink
}

func (sink *sequenceBrokerSink) holdUntilEvent(event string) {
	sink.mu.Lock()
	sink.held = true
	sink.releaseEvent = event
	sink.mu.Unlock()
}

func (sink *sequenceBrokerSink) snapshot() []clienttelemetry.Event {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	out := make([]clienttelemetry.Event, len(sink.events))
	copy(out, sink.events)
	return out
}

// sequenceRunFunc adapts a blocking run function to the TunnelRuntime seam,
// the runner-owned analog of the test files' runFuncRuntime.
type sequenceRunFunc func(ctx context.Context, configJSON []byte) error

func (f sequenceRunFunc) Run(ctx context.Context, configJSON []byte) (TunnelRun, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	run := &sequenceFuncRun{cancel: cancel, done: make(chan error, 1)}
	go func() {
		err := f(runCtx, configJSON)
		run.mu.Lock()
		if run.stopped {
			err = nil
		}
		run.mu.Unlock()
		run.done <- err
		close(run.done)
	}()
	return run, nil
}

type sequenceFuncRun struct {
	cancel  context.CancelFunc
	done    chan error
	mu      sync.Mutex
	stopped bool
}

func (r *sequenceFuncRun) Done() <-chan error { return r.done }

func (r *sequenceFuncRun) Stop(grace time.Duration) error {
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	r.cancel()
	<-r.done
	return nil
}

// sequenceWSSBridge is the runner's inert WSS access bridge: it holds a fixed
// loopback endpoint and serves until stopped. Nothing dials it — the tunnel
// run is scripted too — so it carries no traffic, only lifecycle.
type sequenceWSSBridge struct {
	fatal chan error
	end   wsscore.SessionEnd
}

func newSequenceWSSBridge() *sequenceWSSBridge {
	return &sequenceWSSBridge{fatal: make(chan error, 1)}
}

func (b *sequenceWSSBridge) Endpoint() (string, int) { return "127.0.0.1", 43123 }

func (b *sequenceWSSBridge) Serve(ctx context.Context) error {
	select {
	case err := <-b.fatal:
		return err
	case <-ctx.Done():
		return nil
	}
}

func (b *sequenceWSSBridge) Close() error { return nil }

func (b *sequenceWSSBridge) SessionEnd() wsscore.SessionEnd { return b.end }

// RunSequenceScenario drives e through one scenario's timeline and returns
// the projected observation — the three ordered output streams the contract
// freezes. The caller owns the engine's construction (sinks, hooks, mode,
// telemetry identity); the runner owns every transport seam, per the vector
// file's determinism note. The engine must be freshly constructed, in proxy
// mode, and without a TelemetryOutboxDirectory — the conditions the golden
// expects were generated under.
func RunSequenceScenario(tb SequenceTB, e *Engine, sc *SequenceScenario) SequenceExpected {
	tb.Helper()
	if e.Mode() != ModeProxy {
		tb.Fatalf("sequence scenarios run in proxy mode (the goldens' generation mode); got %s", e.Mode())
	}
	if e.TelemetryOutboxDirectory != "" {
		tb.Fatalf("sequence scenarios run on the in-memory telemetry queue; unset TelemetryOutboxDirectory")
	}
	if state := e.State(); state.Status != StatusDisconnected {
		tb.Fatalf("sequence scenarios need a fresh engine; status is already %q", state.Status)
	}

	// The persistent client identity must not leak between scenarios or out
	// of the suite.
	tmp := tb.TempDir()
	tb.Setenv("HOME", tmp)
	tb.Setenv("XDG_CONFIG_HOME", tmp)
	tb.Setenv("AppData", tmp)

	broker := newSequenceBrokerSink(tb)
	if sc.Script.TelemetryHeld {
		// Held until the batch carrying the session's connection_ended — the
		// engine-ordered release, so on EVERY interleaving only the terminal
		// flush can deliver, and it delivers the whole session as one backlog.
		broker.holdUntilEvent("connection_ended")
	}

	recorder := &sequenceRecorder{inner: e.Sink}
	e.Sink = recorder

	// The directory: hosts are the driver's business (the file scripts relay
	// identities, not addresses), assigned deterministically by position. The
	// scripted errors are built here, on the caller's goroutine, because
	// sequenceCause fails the run on an unknown token and the dial stub runs
	// on engine goroutines, where a test must never fail.
	hostCause := map[string]error{}
	frontFor := map[string]brokerapi.RelayWSSFront{}
	relayIDs := map[string]bool{}
	relays := make([]brokerapi.RelayDescriptor, 0, len(sc.Directory))
	for i, row := range sc.Directory {
		if relayIDs[row.ID] {
			tb.Fatalf("directory serves relay %q twice", row.ID)
		}
		host := fmt.Sprintf("127.0.0.%d", 10+i)
		relay := sequenceRelayDescriptor(row, host)
		for _, front := range relay.WSSFronts {
			frontFor[front.ID] = front
		}
		relays = append(relays, relay)
		relayIDs[row.ID] = true
		for _, dial := range sc.Script.Dials {
			if dial.Relay == row.ID {
				hostCause[host] = sequenceCause(tb, dial.Cause)
			}
		}
	}
	for _, dial := range sc.Script.Dials {
		if !relayIDs[dial.Relay] {
			tb.Fatalf("script dials relay %q, which the directory does not serve", dial.Relay)
		}
	}

	// Every transport seam is scripted; periodic activity must not fire on
	// its own inside a scenario (sweeps and heartbeats run only where a step
	// provokes them); the network-liveness probe is scripted always-alive so
	// recovery gates never hold a scenario on real dials; and the geo lookup
	// is best-effort metadata stubbed to nothing.
	e.fetchRelays = func(ctx context.Context, brokerURL string, limit int, clientID, sessionID string) (discovery.Fetch, error) {
		return discovery.Fetch{BrokerURL: brokerURL, Response: brokerapi.RelayListResponse{
			Count: len(relays), ServerTime: time.Now(), Relays: relays,
		}}, nil
	}
	e.dialRelay = func(_ context.Context, host string, _ int) (int64, error) {
		if err, ok := hostCause[host]; ok {
			return 0, err
		}
		return 1, nil
	}
	e.probeTunnel = func(context.Context, int) (int64, error) { return 2, nil }
	e.healthProbe = func(context.Context, int) error { return nil }
	e.tunnelReady = func(context.Context, int) error { return nil }
	e.healthTick = time.Hour
	e.heartbeatTick = time.Hour
	e.checkNetworkAlive = func(context.Context, []string) bool { return true }
	e.lookupGeo = func(context.Context, *http.Client) map[string]string { return nil }
	e.OSProxy = nil // mobile hosts have no OS proxy; nil behaves as unsupported

	// Unbuffered: a crash_tunnel step hands its error directly to a live
	// tunnel run, so a crash nothing is running to receive fails the step
	// rather than silently poisoning a later run.
	crash := make(chan error)
	e.TunnelRuntime = sequenceRunFunc(func(ctx context.Context, _ []byte) error {
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
		e.requestWSSTicket = func(_ context.Context, _ string, request brokerapi.WSSTicketRequest, _, _ string) (brokerapi.WSSTicketResponse, error) {
			front, ok := frontFor[request.FrontID]
			if !ok {
				return brokerapi.WSSTicketResponse{}, fmt.Errorf("ticket requested for unscripted front %q", request.FrontID)
			}
			ticketMu.Lock()
			tickets++
			value := fmt.Sprintf("scripted-ticket-%d", tickets)
			ticketMu.Unlock()
			return brokerapi.WSSTicketResponse{
				Ticket: value, ExpiresAt: time.Now().Add(time.Minute), URL: front.URL,
			}, nil
		}
		e.dialWSS = func(context.Context, string, string) (wssBridge, error) {
			return newSequenceWSSBridge(), nil
		}
	}

	e.PunchEnabled = sc.Script.Punch != nil
	if sc.Script.Punch != nil {
		reason := sc.Script.Punch.Reason
		e.PunchEstablisher = func(context.Context, punchcore.HubClient, string) (*PunchPath, punchcore.PunchResult, error) {
			return nil, punchcore.PunchResult{Reason: reason}, errors.New("scripted punch failure")
		}
	}

	// With hold_readiness, every readiness probe blocks until release_ready;
	// probes after the release pass immediately. The gate only ever signals
	// for a probe that actually held, so an await_ready_hold scheduled after
	// the release fails its deadline instead of passing vacuously.
	var readyGate <-chan struct{}
	var releaseReady func()
	readyReleased := false
	if sc.Script.HoldReadiness {
		gate := make(chan struct{}, 1)
		releaseCh := make(chan struct{})
		e.tunnelReady = func(ctx context.Context, _ int) error {
			select {
			case <-releaseCh:
				return nil
			default:
			}
			select {
			case gate <- struct{}{}:
			default:
			}
			select {
			case <-releaseCh:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		var once sync.Once
		readyGate = gate
		releaseReady = func() { once.Do(func() { close(releaseCh) }) }
	}

	for index, step := range sc.Steps {
		fail := func(format string, args ...any) {
			tb.Helper()
			tb.Fatalf("step %d (%s): %s", index+1, step.Do, fmt.Sprintf(format, args...))
		}
		switch step.Do {
		case "connect":
			if err := e.Connect(broker.srv.URL, "", ""); err != nil {
				fail("connect: %v", err)
			}
		case "await_status":
			awaitSequenceCondition(tb, fail,
				func() bool {
					return occurrences(collapseStatuses(recorder.statesSnapshot()), step.Status) >= max(step.Count, 1)
				},
				func() string {
					return fmt.Sprintf("status %q (x%d); statuses so far: %v",
						step.Status, max(step.Count, 1), collapseStatuses(recorder.statesSnapshot()))
				})
		case "await_notice":
			awaitSequenceCondition(tb, fail,
				func() bool {
					return recorder.noticesOf(NoticeKind(step.Kind)) >= max(step.Count, 1)
				},
				func() string {
					return fmt.Sprintf("notice %q (x%d); notices so far: %v", step.Kind, max(step.Count, 1), recorder.noticeKinds())
				})
		case "network":
			if step.Up == nil {
				fail("network step needs an explicit \"up\"")
			}
			e.UpdateNetworkState(NetworkState{Up: *step.Up, Fingerprint: step.Fingerprint})
		case "crash_tunnel":
			// The send must be consumed by a live tunnel run; a bounded wait
			// turns a mis-scripted crash (no session, or two crashes for one
			// run) into a step failure instead of a wedged test.
			select {
			case crash <- sequenceCause(tb, step.Cause):
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
			if err := e.Disconnect(); err != nil {
				fail("disconnect: %v", err)
			}
		case "shutdown":
			if err := e.Shutdown(time.Duration(step.FlushBudgetMS) * time.Millisecond); err != nil {
				fail("shutdown flush: %v", err)
			}
		default:
			fail("unknown step")
		}
	}

	// The connect goroutine finalizes — terminal status, session-end events,
	// terminal flush — before clearing the connection, so idle means every
	// stream is complete.
	awaitSequenceCondition(tb,
		func(format string, args ...any) { tb.Helper(); tb.Fatalf(format, args...) },
		func() bool {
			e.mu.Lock()
			idle := e.conn == nil
			e.mu.Unlock()
			return idle
		},
		func() string { return "the connection to finish" })

	got := SequenceExpected{
		Statuses: collapseStatuses(recorder.statesSnapshot()),
		Notices:  []SequenceNotice{},
		Events:   []SequenceEvent{},
	}
	for _, notice := range recorder.noticesSnapshot() {
		got.Notices = append(got.Notices, SequenceNotice{
			Kind:        string(notice.Kind),
			RelayID:     notice.RelayID,
			FromRelayID: notice.FromRelayID,
			FrontID:     notice.FrontID,
			Failures:    notice.Failures,
			Threshold:   notice.Threshold,
		})
	}
	for _, event := range broker.snapshot() {
		if event.Event == "session_heartbeat" {
			continue // cadence-driven, deliberately outside the sequence contract
		}
		projected := SequenceEvent{Event: event.Event, RelayID: event.RelayID}
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

// sequenceRelayDescriptor builds the broker row for one scripted relay: a
// usable VLESS/Reality descriptor at the assigned loopback host, with signed
// WSS fronts when the scenario advertises them.
func sequenceRelayDescriptor(row SequenceRelay, host string) brokerapi.RelayDescriptor {
	relay := brokerapi.RelayDescriptor{
		ID:               row.ID,
		PublicHost:       host,
		PublicPort:       443,
		Protocol:         brokerapi.ProtocolVLESSRealityVision,
		ClientID:         "uuid",
		RealityPublicKey: "pk",
		ShortID:          "sid",
		ServerName:       "sni",
		Flow:             brokerapi.FlowVision,
		ExitMode:         brokerapi.ExitModeDirect,
		ExpiresAt:        time.Now().Add(time.Hour),
		RelayGeoLocation: brokerapi.RelayGeoLocation{
			City: row.City, Country: row.Country, CountryCode: row.CountryCode,
			Latitude: 1, Longitude: 2,
		},
		PunchCapable: row.PunchCapable,
	}
	if len(row.WSSFrontIDs) > 0 {
		relay.NodeClass = brokerapi.NodeClassFoundation
		relay.Transport = brokerapi.TransportDirect
		fronts := make([]brokerapi.RelayWSSFront, 0, len(row.WSSFrontIDs))
		for _, frontID := range row.WSSFrontIDs {
			fronts = append(fronts, brokerapi.RelayWSSFront{
				ID:              frontID,
				URL:             "wss://a.cdn.example/api/v1/wss-bridge",
				ProtocolVersion: wsscore.ProtocolVersion,
			})
		}
		relay.WSSFronts = fronts
	}
	return relay
}

// awaitSequenceCondition is the one polling loop behind the await_* steps:
// poll cond until it holds or the step deadline passes, failing with what was
// actually observed so a timeout names the divergence, not just the wait.
func awaitSequenceCondition(tb SequenceTB, fail func(string, ...any), cond func() bool, describe func() string) {
	tb.Helper()
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
