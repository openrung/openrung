package connectcore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/openrung/openrung/connectcore/contract"
)

// This suite runs the event-sequence contract vectors (ADR-003 A4) against a
// bare engine, through the exported runner in sequencevectors.go — the same
// runner the mobile repo's punchbridge suite drives with a binding-constructed
// engine (ADR-003 B1). The scenario format is documented inside the vector
// file; the runner is its one executable implementation.
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

func TestEventSequenceVectors(t *testing.T) {
	// Strict: the regeneration path marshals SequenceVectorFile back over the
	// file, so a JSON field the struct does not mirror would be silently
	// deleted on the next regen — an unknown field fails the load instead.
	var file SequenceVectorFile
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
			engine := New()
			engine.Sink = &testSink{}
			got := RunSequenceScenario(t, engine, sc)
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

// TestSequenceRunnerRefusesUnsuitableEngines pins the runner's precondition
// checks: the goldens were generated in proxy mode on the in-memory telemetry
// queue, so an engine configured otherwise must be refused loudly rather than
// produce sequences that diverge for an environmental reason.
func TestSequenceRunnerRefusesUnsuitableEngines(t *testing.T) {
	sc := &SequenceScenario{ID: "precondition-probe"}

	tun := New()
	if err := tun.SetMode(ModeTUN); err != nil {
		t.Fatal(err)
	}
	if msg := runSequenceScenarioFailure(t, tun, sc); msg == "" {
		t.Fatal("a TUN-mode engine was accepted")
	}

	outbox := New()
	outbox.TelemetryOutboxDirectory = t.TempDir()
	if msg := runSequenceScenarioFailure(t, outbox, sc); msg == "" {
		t.Fatal("an engine with a persistent outbox was accepted")
	}
}

// runSequenceScenarioFailure runs the runner against a sacrificial TB and
// reports the first fatal message, or "" if the runner accepted the engine.
func runSequenceScenarioFailure(t *testing.T, e *Engine, sc *SequenceScenario) string {
	t.Helper()
	tb := &failingTB{T: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = recover() }() // the fake TB panics instead of Goexit
		_ = RunSequenceScenario(tb, e, sc)
	}()
	<-done
	return tb.fatal
}

// failingTB records Fatalf instead of ending the test, so precondition
// failures can be asserted. It delegates the environment helpers to the real
// *testing.T.
type failingTB struct {
	*testing.T
	fatal string
}

func (tb *failingTB) Fatalf(format string, args ...any) {
	tb.fatal = fmt.Sprintf(format, args...)
	panic("failingTB: fatal")
}

func mustSequenceJSON(v any) string {
	blob, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%+v", v)
	}
	return string(blob)
}
