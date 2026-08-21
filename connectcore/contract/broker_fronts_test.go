package contract

import (
	"slices"
	"testing"

	"github.com/openrung/openrung/brokerapi"
)

// brokerFrontsVectorsVersion pins the version of
// contract/vectors/broker_fronts.json this suite expects; see the note on
// classificationVectorsVersion in the clienttelemetry package.
const brokerFrontsVectorsVersion = 2

type brokerFrontVectors struct {
	DefaultOrder []string `json:"default_order"`
	Phases       []struct {
		Phase int      `json:"phase"`
		Name  string   `json:"name"`
		URLs  []string `json:"urls"`
	} `json:"phases"`
	Classification []struct {
		URL             string `json:"url"`
		EndpointUnbound bool   `json:"endpoint_unbound"`
		Note            string `json:"note"`
	} `json:"classification"`
	Candidates []struct {
		ID                  string   `json:"id"`
		Primary             string   `json:"primary"`
		Note                string   `json:"note"`
		ExpectURLs          []string `json:"expect_urls"`
		ExpectOverrideFirst bool     `json:"expect_override_first"`
	} `json:"candidates"`
}

func loadBrokerFrontVectors(t *testing.T) brokerFrontVectors {
	t.Helper()
	var vectors brokerFrontVectors
	if err := LoadVersioned(BrokerFrontsVectors, brokerFrontsVectorsVersion, &vectors); err != nil {
		t.Fatalf("load broker front vectors: %v", err)
	}
	return vectors
}

// TestBrokerFrontVectors checks the advertised front list and the two-phase
// race order against the snapshot every client is expected to agree with.
func TestBrokerFrontVectors(t *testing.T) {
	vectors := loadBrokerFrontVectors(t)

	if got := brokerapi.DefaultBrokerURLs(); !slices.Equal(got, vectors.DefaultOrder) {
		t.Errorf("DefaultBrokerURLs() = %v, want %v", got, vectors.DefaultOrder)
	}

	// The phases must partition the default order, in the order they are raced:
	// every phase-1 front is endpoint-bound, every phase-2 front is not.
	partition := make([]string, 0, len(vectors.DefaultOrder))
	for _, phase := range vectors.Phases {
		for _, brokerURL := range phase.URLs {
			partition = append(partition, brokerURL)
			unbound := brokerapi.EndpointUnboundBrokerFront(brokerURL)
			if want := phase.Name == "endpoint_unbound"; unbound != want {
				t.Errorf("%s is in phase %q (%d) but EndpointUnboundBrokerFront reports %v",
					brokerURL, phase.Name, phase.Phase, unbound)
			}
		}
	}
	slices.Sort(partition)
	defaults := slices.Clone(vectors.DefaultOrder)
	slices.Sort(defaults)
	if !slices.Equal(partition, defaults) {
		t.Errorf("the phases cover %v, want exactly the default order %v", partition, defaults)
	}
}

// TestBrokerFrontClassificationVectors pins how a URL is assigned to a race
// phase. The rule matches the shape of a hostname rather than this deployment's
// own endpoints, so the rows include names this deployment does not use.
func TestBrokerFrontClassificationVectors(t *testing.T) {
	vectors := loadBrokerFrontVectors(t)
	if len(vectors.Classification) == 0 {
		t.Fatal("broker front vectors carry no classification rows")
	}

	for _, row := range vectors.Classification {
		if got := brokerapi.EndpointUnboundBrokerFront(row.URL); got != row.EndpointUnbound {
			t.Errorf("EndpointUnboundBrokerFront(%q) = %v, want %v (%s)", row.URL, got, row.EndpointUnbound, row.Note)
		}
	}
}

// TestBrokerCandidateVectors pins the discovery order a client builds from its
// configured primary, including whether that primary is attempted alone first.
func TestBrokerCandidateVectors(t *testing.T) {
	vectors := loadBrokerFrontVectors(t)
	if len(vectors.Candidates) == 0 {
		t.Fatal("broker front vectors carry no candidate rows")
	}

	for _, row := range vectors.Candidates {
		t.Run(row.ID, func(t *testing.T) {
			candidates := brokerapi.BrokerCandidates(row.Primary)
			if !slices.Equal(candidates.URLs, row.ExpectURLs) {
				t.Errorf("BrokerCandidates(%q).URLs = %v, want %v (%s)", row.Primary, candidates.URLs, row.ExpectURLs, row.Note)
			}
			if candidates.OverrideFirst != row.ExpectOverrideFirst {
				t.Errorf("BrokerCandidates(%q).OverrideFirst = %v, want %v (%s)",
					row.Primary, candidates.OverrideFirst, row.ExpectOverrideFirst, row.Note)
			}
		})
	}
}
