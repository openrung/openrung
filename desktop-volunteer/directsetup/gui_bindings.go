//go:build bindings

package directsetup

// ValidateGUIStartup is a no-op only in Wails' short-lived bindings generator.
// The generator executes the application package to inspect bound methods, but
// it never initializes the desktop GUI. Production builds do not carry the
// bindings tag and retain the platform-specific root/Administrator guard.
func ValidateGUIStartup() error { return nil }
