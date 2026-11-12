// SPDX-License-Identifier: GPL-3.0-or-later

package vpnservice

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/openrung/openrung/connectcore"

	"openrung/internal/clientstate"
)

// TUN mode in the desktop app.
//
// The engine already captures the whole device through a TUN interface (see
// connectcore.ModeTUN); what a GUI has to add is the privilege to create that
// interface. On Windows the app obtains it by restarting itself elevated:
// when a launch is to run in TUN mode, the app starts a new instance of
// itself through the "runas" verb (one UAC prompt) and quits, and the elevated
// instance switches the engine to TUN mode. TUN is the mode a first launch
// starts in there; after that the app starts in the mode Settings last
// stored, so every launch repeats the restart while that is TUN mode.
//
// The whole app is elevated rather than only the tunnel process because the
// tunnel process is stopped by closing its standard input, and the shell verb
// that elevates a process cannot hand it a pipe.
//
// The platform files (elevation_windows.go, elevation_other.go) supply
// tunSupported, processElevated and relaunchElevated, as variables so the
// logic here is testable on every platform.

const (
	tunnelModeProxy = "proxy"
	tunnelModeTUN   = "tun"

	// relaunchFlag marks an instance the app started elevated; its value is
	// the starting instance's process id.
	relaunchFlag = "-openrung-relaunched-from="
)

// NativeTunnelMode is what Settings needs to offer the capture mode.
type NativeTunnelMode struct {
	// Mode is the mode connects use: "proxy" or "tun".
	Mode string `json:"mode"`
	// Available says TUN mode can be selected in this build on this system.
	Available bool `json:"available"`
	// RequiresRestart says that selecting TUN mode restarts the app with
	// administrator rights.
	RequiresRestart bool `json:"requiresRestart"`
}

// GetTunnelMode is Wails-bound.
func (s *Service) GetTunnelMode() NativeTunnelMode {
	mode := tunnelModeProxy
	if s.engine.Mode() == connectcore.ModeTUN {
		mode = tunnelModeTUN
	}
	available := s.tunUnavailable() == nil
	return NativeTunnelMode{
		Mode:            mode,
		Available:       available,
		RequiresRestart: available && !processElevated(),
	}
}

// SetTunnelMode is Wails-bound. It selects the capture mode for later
// connects and stores the choice. Selecting TUN mode in an instance without
// administrator rights restarts the app elevated instead; a refused restart
// (a declined UAC prompt) is an error and leaves proxy mode in place. The
// mode cannot change while a tunnel is up.
func (s *Service) SetTunnelMode(mode string) error {
	switch mode {
	case tunnelModeProxy:
		if err := s.engine.SetMode(connectcore.ModeProxy); err != nil {
			return err
		}
		s.saveTUNMode(false)
		return nil
	case tunnelModeTUN:
	default:
		return fmt.Errorf("unknown tunnel mode %q", mode)
	}

	if err := s.tunUnavailable(); err != nil {
		return err
	}
	if processElevated() {
		if err := s.engine.SetMode(connectcore.ModeTUN); err != nil {
			return err
		}
		s.saveTUNMode(true)
		return nil
	}
	// The restart ends this instance, so it must not take a tunnel with it.
	switch s.engine.State().Status {
	case connectcore.StatusDisconnected, connectcore.StatusFailed:
	default:
		return errors.New("disconnect before changing the tunnel mode")
	}
	if s.store == nil {
		// The elevated instance learns the choice from the state directory.
		return errors.New("TUN mode needs the client state directory, which is unavailable")
	}
	if err := s.store.SaveDesktopTUNMode(true); err != nil {
		return fmt.Errorf("store the tunnel mode: %w", err)
	}
	if err := relaunchElevated(); err != nil {
		s.saveTUNMode(false)
		return err
	}
	s.appendLog("restarting with administrator rights for TUN mode")
	if s.Quit != nil {
		s.Quit()
	}
	return nil
}

// RelaunchForTUN is for package main, before it opens a window (the context
// parameter keeps Wails from binding it). When the launch is to start in TUN
// mode (see startsInTUN) and this instance lacks the rights for it, it starts
// an elevated instance and reports true: the caller exits. A refused restart
// stores proxy mode and the app carries on in it, so a declined prompt is not
// asked again at every launch.
func (s *Service) RelaunchForTUN(_ context.Context, args []string) bool {
	if s.tunUnavailable() != nil || processElevated() || relaunchedFrom(args) != 0 {
		return false
	}
	// Startup has not run yet, so the service has no store of its own.
	store := s.store
	if store == nil {
		opened, err := clientstate.New()
		if err != nil {
			return false
		}
		store = opened
	}
	if !s.startsInTUN(store) {
		return false
	}
	if err := relaunchElevated(); err != nil {
		s.appendLog("TUN mode is off: " + err.Error())
		if err := store.SaveDesktopTUNMode(false); err != nil {
			s.appendLog("could not store the tunnel mode: " + err.Error())
		}
		return false
	}
	return true
}

// startsInTUN reports the mode a launch starts in: the stored choice, or,
// before any choice is stored, TUN mode wherever the app can offer it. The
// app therefore opens in TUN mode on a first launch on Windows, and a
// declined prompt for the rights stores proxy mode (see RelaunchForTUN).
func (s *Service) startsInTUN(store *clientstate.Store) bool {
	if tun, stored := store.LoadDesktopTUNMode(); stored {
		return tun
	}
	return s.tunUnavailable() == nil
}

// restoreTunnelMode applies the starting mode at startup.
func (s *Service) restoreTunnelMode() {
	if s.store == nil || !s.startsInTUN(s.store) {
		return
	}
	if err := s.tunUnavailable(); err != nil {
		s.appendLog("TUN mode is off: " + err.Error())
		return
	}
	if !processElevated() {
		s.appendLog("TUN mode is off: OpenRung is running without administrator rights")
		return
	}
	if err := s.engine.SetMode(connectcore.ModeTUN); err != nil {
		s.appendLog("TUN mode is off: " + err.Error())
	}
}

func (s *Service) saveTUNMode(tun bool) {
	if s.store == nil {
		return
	}
	if err := s.store.SaveDesktopTUNMode(tun); err != nil {
		s.appendLog("could not store the tunnel mode: " + err.Error())
	}
}

// tunUnavailable explains why TUN mode cannot be offered, or returns nil.
func (s *Service) tunUnavailable() error {
	if !tunSupported {
		return errors.New("TUN mode is not available in the desktop app on this system")
	}
	if !s.SingBoxStopsOnStdinClose {
		// Same refusal as the terminal client's: the only graceful stop on
		// Windows is the bundled engine's stdin-close protocol, and a forced
		// stop leaves routes and DNS pointing at a tunnel that is gone.
		return errors.New("TUN mode needs the bundled tunnel engine; unset OPENRUNG_SING_BOX")
	}
	return nil
}

// elevation implements connectcore.Elevation. SetTunnelMode only selects TUN
// mode in an instance that holds the rights, so this is the engine's backstop
// rather than the place the user is asked.
type elevation struct{ s *Service }

func (e elevation) Elevate(context.Context) error {
	if err := e.s.tunUnavailable(); err != nil {
		return err
	}
	if !processElevated() {
		return errors.New("TUN mode needs administrator rights: turn TUN mode on in Settings to restart OpenRung with them")
	}
	return nil
}

// relaunchedFrom returns the process id of the instance that started this one
// elevated, or 0 for an ordinary launch.
func relaunchedFrom(args []string) uint32 {
	index := slices.IndexFunc(args, func(arg string) bool {
		return len(arg) > len(relaunchFlag) && arg[:len(relaunchFlag)] == relaunchFlag
	})
	if index < 0 {
		return 0
	}
	var pid uint32
	if _, err := fmt.Sscanf(args[index][len(relaunchFlag):], "%d", &pid); err != nil {
		return 0
	}
	return pid
}

// WaitForRelaunchParent is for package main, first thing in an instance the
// app started elevated: it waits for the instance that started it to exit, so
// the two never hold the state directory or the webview's data at once. It
// returns at once for an ordinary launch, and gives up after a few seconds.
func WaitForRelaunchParent(args []string) {
	if pid := relaunchedFrom(args); pid != 0 {
		waitForProcessExit(pid)
	}
}
