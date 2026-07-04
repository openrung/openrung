package client

// appVersion is the CLI build version reported in telemetry and the
// X-OpenRung-App-Version header. Override at build time with:
//
//	go build -ldflags "-X openrung/internal/client.appVersion=0.1.2" ./cmd/client
var appVersion = "dev"

// AppVersion returns the CLI build version.
func AppVersion() string { return appVersion }
