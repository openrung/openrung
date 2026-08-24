package main

import (
	"errors"

	"golang.org/x/sys/windows"
)

const (
	// tunModeSummary is the parenthetical the Settings Mode row and the --tun
	// flag help carry; tunModeAdvice is the sentence shown after switching to
	// TUN mode.
	tunModeSummary = "needs Administrator"
	tunModeAdvice  = "the client must run from an elevated (Run as administrator) terminal"
)

// tunModeAvailable reports whether this process may create a TUN device and
// install routes: on Windows that is an elevated token — creating the wintun
// adapter needs Administrator. Like the Unix check, refusing here costs a
// rerun from an elevated terminal, while wrongly admitting the process would
// surface as an opaque sing-box failure after the ladder had already dialed
// relays.
//
// Elevation used to be beside the point on Windows: with no way to stop
// sing-box gracefully (no os.Interrupt, no console event into a
// CREATE_NO_WINDOW child), every stop was a hard kill that could leave the
// routing table and DNS pointing at a tunnel that was gone, so TUN mode was
// refused outright. The bundled runtime's stdin-close stop protocol
// (connectcore's client.StopOnStdinCloseFlag) is that graceful stop, which is
// also why TUN mode here requires the bundled runtime — an external -sing-box
// binary does not speak it and stays refused (tunRequiresBundledRuntime).
//
// The wintun driver needs no separate install or DLL: sing-tun embeds
// wintun.dll in the binary and loads it from memory (THIRD_PARTY_NOTICES.md
// section 8).
func tunModeAvailable() error {
	if windows.GetCurrentProcessToken().IsElevated() {
		return nil
	}
	return errors.New(
		"TUN mode needs Administrator privileges to create the tunnel device: rerun the client from an elevated terminal (Run as administrator), or drop --tun to use proxy mode (no privileges needed)",
	)
}
