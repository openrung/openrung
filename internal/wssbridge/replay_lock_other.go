//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package wssbridge

import "os"

// The relay sidecar is deployed on Linux. Other build targets retain
// in-process atomicity but do not provide an advisory cross-process lock.
func lockReplayFile(*os.File) error   { return nil }
func unlockReplayFile(*os.File) error { return nil }
