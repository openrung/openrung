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
