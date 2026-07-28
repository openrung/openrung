//go:build !linux && !darwin && !windows && !bindings

package directsetup

func ValidateGUIStartup() error { return nil }
