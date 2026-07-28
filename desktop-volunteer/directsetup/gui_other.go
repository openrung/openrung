//go:build !linux && !darwin && !windows

package directsetup

func ValidateGUIStartup() error { return nil }
