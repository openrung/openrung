// Package contract serves the golden contract vectors in contract/vectors to
// the Go test suites that check them.
//
// The vectors describe load-bearing behavior that four suites across two repos
// must agree on — failure classification, relay-directory decoding, and the
// broker-front list — and the mobile repo vendors the same files for its
// Kotlin, Swift, and TypeScript suites. Each file carries its own doc header
// stating that rule and the version-bump discipline that goes with it.
//
// The files are embedded rather than read by path so a test can load them from
// any package without a relative path that breaks when the caller moves. Only
// test code imports this package.
package contract

import (
	"embed"
	"encoding/json"
	"fmt"
)

//go:embed vectors/*.json
var vectorFS embed.FS

// ClassificationVectors, RelayDecodeVectors, and BrokerFrontsVectors name the
// vector files. Callers pass one to Load.
const (
	ClassificationVectors = "classification.json"
	RelayDecodeVectors    = "relay_decode.json"
	BrokerFrontsVectors   = "broker_fronts.json"
)

// Raw returns the exact bytes of one vector file.
func Raw(name string) ([]byte, error) {
	data, err := vectorFS.ReadFile("vectors/" + name)
	if err != nil {
		return nil, fmt.Errorf("read contract vectors %s: %w", name, err)
	}
	return data, nil
}

// Load decodes one vector file into target.
func Load(name string, target any) error {
	data, err := Raw(name)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode contract vectors %s: %w", name, err)
	}
	return nil
}

// LoadVersioned decodes one vector file into target after checking the file's
// top-level "version" against wantVersion — the version the calling suite was
// written against, which each suite pins in its own constant. The vectors are
// a cross-repo contract: an edited row has to reach the mobile repo's suites
// too, so the file's version moves with the edit, every Go suite's pin moves
// deliberately with the version, and the vendored copies are refreshed. The
// mismatch error below is the single copy of that guidance; version
// validation lives here and nowhere else.
func LoadVersioned(name string, wantVersion int, target any) error {
	data, err := Raw(name)
	if err != nil {
		return err
	}
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return fmt.Errorf("decode contract vectors %s: %w", name, err)
	}
	if header.Version < 1 {
		return fmt.Errorf("contract vectors %s carry version %d, want a positive version", name, header.Version)
	}
	if header.Version != wantVersion {
		return fmt.Errorf("contract vectors %s are version %d, this suite was written against %d — "+
			"bump the suite's pinned version constant together with the file, and re-vendor it in openrung-mobile-app",
			name, header.Version, wantVersion)
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode contract vectors %s: %w", name, err)
	}
	return nil
}
