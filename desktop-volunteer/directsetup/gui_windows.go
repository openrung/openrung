//go:build windows && !bindings

package directsetup

import (
	"errors"

	"golang.org/x/sys/windows"
)

// ValidateGUIStartup rejects manually elevated GUI launches. The direct setup
// operation elevates a fixed system PowerShell command instead.
func ValidateGUIStartup() error {
	return validateWindowsGUIElevation(windows.GetCurrentProcessToken().IsElevated())
}

func validateWindowsGUIElevation(elevated bool) error {
	if elevated {
		return errors.New("refusing to run the OpenRung Volunteer GUI as Administrator; relaunch it normally")
	}
	return nil
}
