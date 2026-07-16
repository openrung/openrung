package main

import (
	_ "embed"
	"fmt"
	"regexp"
	"strings"
)

// sourceVersion is the desktop volunteer's component-owned version. Embedding
// it keeps ordinary `go build`/`wails dev` builds on the same version as
// packaged builds without requiring generated source or linker flags.
//
//go:embed VERSION
var sourceVersion string

var stableVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func componentVersion() (string, error) {
	version := strings.TrimSpace(sourceVersion)
	if !stableVersionPattern.MatchString(version) {
		return "", fmt.Errorf("invalid desktop-volunteer/VERSION %q (want X.Y.Z)", version)
	}
	return version, nil
}
