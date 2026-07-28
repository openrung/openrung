//go:build !windows && !linux

package directsetup

func handlePrivilegedCommand([]string) (bool, int) { return false, 0 }
