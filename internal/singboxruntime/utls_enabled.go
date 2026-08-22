//go:build with_utls

package singboxruntime

// UTLSEnabled reports whether this build carries uTLS/Reality client support.
// Release and Makefile builds pass -tags with_utls; a plain `go build` does
// not, and such a binary cannot reach any OpenRung relay's Reality endpoint.
const UTLSEnabled = true
