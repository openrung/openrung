package discovery

import "runtime"

// desktopPlatform maps GOOS to the short platform token sent in the
// X-OpenRung-Desktop header (mirrors the mobile X-OpenRung-RN value).
func desktopPlatform() string {
	switch runtime.GOOS {
	case "darwin":
		return "macos"
	case "windows":
		return "windows"
	case "linux":
		return "linux"
	default:
		return runtime.GOOS
	}
}
