package client

// appVersion is the host application version reported in telemetry and the
// X-OpenRung-App-Version header. In-repo builds inject it at link time:
//
//	go build -ldflags "-X github.com/openrung/openrung/connectcore/client.appVersion=0.1.2" ./cmd/client
//
// The Go linker silently ignores -X for a symbol it cannot resolve, and a
// consumer of the fetched module cannot practically thread -X through its own
// build (gomobile bindings foremost), so external hosts set the version in
// code with SetAppVersion instead of shipping the "dev" default.
var appVersion = "dev"

// AppVersion returns the host application version.
func AppVersion() string { return appVersion }

// SetAppVersion overrides the reported version for hosts that cannot inject
// the link-time symbol (mobile bindings foremost). Call it once during host
// initialization, before any engine or broker client runs; it is not
// synchronized for concurrent use. An empty version is ignored so a host
// passing an unset build variable keeps the current value.
func SetAppVersion(version string) {
	if version != "" {
		appVersion = version
	}
}
