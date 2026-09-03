package contract

import (
	"encoding/json"
	"slices"
	"testing"
)

// knownSuites is every suite identifier a vector file may name. Suites are
// spelled by language rather than by runner ("ts", not "jest") because the
// runner is an implementation detail of the mobile repo.
var knownSuites = []string{"go", "kotlin", "swift", "ts"}

// vectorFiles is every file in vectors/. A file added there without an entry
// here fails TestVectorFilesAreDeclared, so it cannot ship unchecked.
var vectorFiles = []string{ClassificationVectors, RelayDecodeVectors, BrokerFrontsVectors, EventSequenceVectors}

// vectorHeader is the part of every vector file's shape that is common to all
// of them, whatever their rows look like. Version is deliberately not here:
// version validation lives in LoadVersioned, which every Go suite loads its
// file through.
//
// suites declares intended consumers, not wired runners: a declared suite the
// mobile repo does not run yet is recorded there, in testdata/contract
// pin.json's local_suites/pending_suites accounting, which fails when a
// declared non-go suite is neither wired nor pending with a reason — the
// relay_decode.json kotlin/swift precedent, and how event_sequence.json's
// mobile consumers are declared before Track B wires them.
type vectorHeader struct {
	Suites []string `json:"suites"`
}

// TestVectorFileHeaders checks the declaration every vector file carries.
//
// Per-file suites are the routing table for a change: they say which suites a
// bump has to reach. The per-row claiming check that backs them lives with the
// classification vectors, whose rows are claimed individually; the other two
// files are consumed whole, so what is checkable here is that the declaration
// is well formed and names only suites that exist. Without this the field was
// inert in two of the three files — broker_fronts_test.go did not even
// unmarshal it — which is the same unfalsifiable-claim problem the field was
// added to solve.
func TestVectorFileHeaders(t *testing.T) {
	for _, name := range vectorFiles {
		t.Run(name, func(t *testing.T) {
			var header vectorHeader
			if err := Load(name, &header); err != nil {
				t.Fatalf("load: %v", err)
			}
			if len(header.Suites) == 0 {
				t.Fatal("declares no consuming suites")
			}
			for _, suite := range header.Suites {
				if !slices.Contains(knownSuites, suite) {
					t.Errorf("declares unknown suite %q, want one of %v", suite, knownSuites)
				}
			}
			if slices.Contains(header.Suites, "go") != runsInGo(name) {
				t.Errorf("declares suites %v, which disagrees with whether a Go suite actually runs this file", header.Suites)
			}
			for index, suite := range header.Suites {
				if index > 0 && slices.Contains(header.Suites[:index], suite) {
					t.Errorf("declares suite %q twice", suite)
				}
			}
		})
	}
}

// TestVectorFilesAreDeclared checks that vectorFiles covers vectors/ exactly,
// so a new vector file cannot be added without joining the header checks.
func TestVectorFilesAreDeclared(t *testing.T) {
	entries, err := vectorFS.ReadDir("vectors")
	if err != nil {
		t.Fatalf("read vectors dir: %v", err)
	}
	present := make([]string, 0, len(entries))
	for _, entry := range entries {
		present = append(present, entry.Name())
	}
	declared := slices.Clone(vectorFiles)
	slices.Sort(declared)
	slices.Sort(present)
	if !slices.Equal(declared, present) {
		t.Errorf("vectors/ holds %v but vectorFiles declares %v", present, declared)
	}
}

// TestRelayDecodeInvalidSuitesAreDeclared checks the one narrowing inside the
// relay-decode vectors: the invalid cases are run by a subset of the file's
// suites, and that subset cannot name a suite the file does not have.
func TestRelayDecodeInvalidSuitesAreDeclared(t *testing.T) {
	var file struct {
		vectorHeader
		Invalid struct {
			Suites []string `json:"suites"`
		} `json:"invalid"`
	}
	raw, err := Raw(RelayDecodeVectors)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(file.Invalid.Suites) == 0 {
		t.Fatal("the invalid cases declare no suites")
	}
	for _, suite := range file.Invalid.Suites {
		if !slices.Contains(file.Suites, suite) {
			t.Errorf("the invalid cases are claimed by suite %q, which the file does not declare as a consumer", suite)
		}
	}
}

// runsInGo reports whether a Go suite in this repo runs the file. Kept as an
// explicit list rather than derived from the declaration, so the two are
// independent statements that can disagree and be caught.
func runsInGo(name string) bool {
	switch name {
	case ClassificationVectors, RelayDecodeVectors, BrokerFrontsVectors:
		return true
	case EventSequenceVectors:
		// connectcore/sequence_vectors_test.go — inside the engine package,
		// because the scripted transport outcomes drive its unexported seams.
		return true
	}
	return false
}
