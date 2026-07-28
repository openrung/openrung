//go:build !windows

package relayruntime

func platformProbePermissionDenied(error) bool { return false }
func platformProbePortInUse(error) bool        { return false }
