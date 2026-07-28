//go:build (linux || darwin) && !bindings

package directsetup

import (
	"errors"
	"os"
)

// ValidateGUIStartup rejects sudo/root launches. Privileged fixed helper modes
// are consumed before this check and exit without initializing Wails.
func ValidateGUIStartup() error {
	return validateGUIEUID(os.Geteuid())
}

func validateGUIEUID(euid int) error {
	if euid == 0 {
		return errors.New("refusing to run the OpenRung Volunteer GUI as root; launch it as your normal user")
	}
	return nil
}
