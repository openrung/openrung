// SPDX-License-Identifier: GPL-3.0-or-later

package vpnservice

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/openrung/openrung/connectcore"

	"openrung/internal/clientstate"
)

// tunPlatform stands in for the platform files: a system that supports TUN
// mode, with the process elevated or not, and a restart that succeeds or is
// refused. It reports how often a restart and a quit were asked for.
type tunPlatform struct {
	relaunches, quits int
}

func stubTUNPlatform(t *testing.T, supported, elevated bool, relaunchErr error) *tunPlatform {
	t.Helper()
	platform := &tunPlatform{}
	prevSupported, prevElevated, prevRelaunch := tunSupported, processElevated, relaunchElevated
	t.Cleanup(func() {
		tunSupported, processElevated, relaunchElevated = prevSupported, prevElevated, prevRelaunch
	})
	tunSupported = supported
	processElevated = func() bool { return elevated }
	relaunchElevated = func() error {
		platform.relaunches++
		return relaunchErr
	}
	return platform
}

// tunService is a service on the bundled engine with a state directory, as
// the app has after Startup.
func tunService(t *testing.T, platform *tunPlatform) *Service {
	t.Helper()
	s := New()
	s.SingBoxStopsOnStdinClose = true
	s.store = clientstate.NewInDir(t.TempDir())
	s.Quit = func() { platform.quits++ }
	return s
}

// storedMode reads the service's stored capture mode: "tun", "proxy", or ""
// when no choice is stored.
func storedMode(s *Service) string {
	switch tun, stored := s.store.LoadDesktopTUNMode(); {
	case !stored:
		return ""
	case tun:
		return "tun"
	default:
		return "proxy"
	}
}

func logged(s *Service, fragment string) bool {
	for _, line := range s.GetState().LogLines {
		if strings.Contains(line, fragment) {
			return true
		}
	}
	return false
}

func TestGetTunnelModeReportsWhatSettingsCanOffer(t *testing.T) {
	platform := stubTUNPlatform(t, false, false, nil)
	s := tunService(t, platform)
	if got := s.GetTunnelMode(); got != (NativeTunnelMode{Mode: "proxy"}) {
		t.Fatalf("unsupported system: %+v", got)
	}

	stubTUNPlatform(t, true, false, nil)
	if got := s.GetTunnelMode(); got != (NativeTunnelMode{Mode: "proxy", Available: true, RequiresRestart: true}) {
		t.Fatalf("supported, not elevated: %+v", got)
	}
	// An external engine cannot be stopped gracefully, so TUN is not offered.
	s.SingBoxStopsOnStdinClose = false
	if got := s.GetTunnelMode(); got.Available || got.RequiresRestart {
		t.Fatalf("external engine: %+v", got)
	}
	s.SingBoxStopsOnStdinClose = true

	stubTUNPlatform(t, true, true, nil)
	if got := s.GetTunnelMode(); got != (NativeTunnelMode{Mode: "proxy", Available: true}) {
		t.Fatalf("supported, elevated: %+v", got)
	}
}

func TestSetTunnelModeRestartsElevatedWhenItLacksTheRights(t *testing.T) {
	platform := stubTUNPlatform(t, true, false, nil)
	s := tunService(t, platform)

	if err := s.SetTunnelMode("tun"); err != nil {
		t.Fatalf("SetTunnelMode: %v", err)
	}
	if platform.relaunches != 1 || platform.quits != 1 {
		t.Fatalf("relaunches = %d, quits = %d", platform.relaunches, platform.quits)
	}
	// The elevated instance reads the choice; this one never ran in TUN mode.
	if storedMode(s) != "tun" {
		t.Fatal("the choice was not stored for the elevated instance")
	}
	if s.engine.Mode() != connectcore.ModeProxy {
		t.Fatal("an instance without the rights switched its engine to TUN mode")
	}
}

func TestSetTunnelModeKeepsProxyModeWhenTheRestartIsRefused(t *testing.T) {
	refused := errors.New("the prompt was declined")
	platform := stubTUNPlatform(t, true, false, refused)
	s := tunService(t, platform)

	if err := s.SetTunnelMode("tun"); !errors.Is(err, refused) {
		t.Fatalf("err = %v", err)
	}
	if platform.quits != 0 {
		t.Fatal("the app quit although no elevated instance was started")
	}
	// Proxy mode is stored, not merely left unset: unset would start the next
	// launch in TUN mode and ask again.
	if storedMode(s) != "proxy" {
		t.Fatalf("stored mode = %q after a refused restart", storedMode(s))
	}
}

func TestSetTunnelModeSwitchesInPlaceWhenElevated(t *testing.T) {
	platform := stubTUNPlatform(t, true, true, nil)
	s := tunService(t, platform)

	if err := s.SetTunnelMode("tun"); err != nil {
		t.Fatalf("SetTunnelMode: %v", err)
	}
	if platform.relaunches != 0 || platform.quits != 0 {
		t.Fatal("an elevated instance restarted itself")
	}
	if s.engine.Mode() != connectcore.ModeTUN || storedMode(s) != "tun" {
		t.Fatal("TUN mode was not applied and stored")
	}
	if got := s.GetTunnelMode(); got != (NativeTunnelMode{Mode: "tun", Available: true}) {
		t.Fatalf("mode = %+v", got)
	}

	if err := s.SetTunnelMode("proxy"); err != nil {
		t.Fatalf("SetTunnelMode: %v", err)
	}
	if s.engine.Mode() != connectcore.ModeProxy || storedMode(s) != "proxy" {
		t.Fatal("proxy mode was not applied and stored")
	}
}

func TestSetTunnelModeRefusesWhatItCannotDo(t *testing.T) {
	platform := stubTUNPlatform(t, false, false, nil)
	s := tunService(t, platform)
	if err := s.SetTunnelMode("tun"); err == nil {
		t.Fatal("TUN mode was accepted on a system that does not support it")
	}
	if err := s.SetTunnelMode("vpn"); err == nil {
		t.Fatal("an unknown mode was accepted")
	}
	if platform.relaunches != 0 || storedMode(s) != "" {
		t.Fatal("a refused request had side effects")
	}

	// Without a state directory the elevated instance could not learn the
	// choice, so the app must not restart into proxy mode.
	platform = stubTUNPlatform(t, true, false, nil)
	s.store = nil
	if err := s.SetTunnelMode("tun"); err == nil || platform.relaunches != 0 {
		t.Fatalf("err = %v, relaunches = %d", err, platform.relaunches)
	}
}

func TestRelaunchForTUNFollowsTheStartingMode(t *testing.T) {
	ctx := context.Background()

	t.Run("first launch", func(t *testing.T) {
		// Nothing stored: the app opens in TUN mode where it can offer it.
		platform := stubTUNPlatform(t, true, false, nil)
		s := tunService(t, platform)
		if !s.RelaunchForTUN(ctx, nil) || platform.relaunches != 1 {
			t.Fatal("a first launch did not restart elevated for TUN mode")
		}
	})
	t.Run("first launch where TUN is not offered", func(t *testing.T) {
		platform := stubTUNPlatform(t, false, false, nil)
		s := tunService(t, platform)
		if s.RelaunchForTUN(ctx, nil) || platform.relaunches != 0 {
			t.Fatal("restarted on a system without TUN mode")
		}
		platform = stubTUNPlatform(t, true, false, nil)
		s.SingBoxStopsOnStdinClose = false
		if s.RelaunchForTUN(ctx, nil) || platform.relaunches != 0 {
			t.Fatal("restarted on an external engine")
		}
	})
	t.Run("proxy stored", func(t *testing.T) {
		platform := stubTUNPlatform(t, true, false, nil)
		s := tunService(t, platform)
		_ = s.store.SaveDesktopTUNMode(false)
		if s.RelaunchForTUN(ctx, nil) || platform.relaunches != 0 {
			t.Fatal("restarted although proxy mode is stored")
		}
	})
	t.Run("tun stored", func(t *testing.T) {
		platform := stubTUNPlatform(t, true, false, nil)
		s := tunService(t, platform)
		_ = s.store.SaveDesktopTUNMode(true)
		if !s.RelaunchForTUN(ctx, nil) || platform.relaunches != 1 {
			t.Fatal("did not restart elevated for the stored TUN mode")
		}
	})
	t.Run("already elevated", func(t *testing.T) {
		platform := stubTUNPlatform(t, true, true, nil)
		s := tunService(t, platform)
		if s.RelaunchForTUN(ctx, nil) || platform.relaunches != 0 {
			t.Fatal("an elevated instance restarted itself")
		}
	})
	t.Run("started by a restart", func(t *testing.T) {
		// An instance the app started must never start another, whatever
		// rights it ended up with.
		platform := stubTUNPlatform(t, true, false, nil)
		s := tunService(t, platform)
		if s.RelaunchForTUN(ctx, []string{relaunchFlag + "4242"}) || platform.relaunches != 0 {
			t.Fatal("a restarted instance restarted again")
		}
	})
	t.Run("refused", func(t *testing.T) {
		platform := stubTUNPlatform(t, true, false, errors.New("the prompt was declined"))
		s := tunService(t, platform)
		if s.RelaunchForTUN(ctx, nil) {
			t.Fatal("reported a restart that was refused")
		}
		// Proxy mode from here on, so the next launch does not ask again.
		if storedMode(s) != "proxy" {
			t.Fatalf("stored mode = %q after a refused restart", storedMode(s))
		}
		if !logged(s, "the prompt was declined") {
			t.Fatal("the refusal was not logged")
		}
		if s.RelaunchForTUN(ctx, nil) || platform.relaunches != 1 {
			t.Fatal("the next launch asked again")
		}
	})
}

func TestStartingModeIsAppliedOnlyWithTheRights(t *testing.T) {
	// A first launch that holds the rights opens in TUN mode.
	platform := stubTUNPlatform(t, true, true, nil)
	s := tunService(t, platform)
	s.restoreTunnelMode()
	if s.engine.Mode() != connectcore.ModeTUN {
		t.Fatal("an elevated first launch did not start in TUN mode")
	}

	// A stored proxy choice wins over that default.
	s = tunService(t, platform)
	_ = s.store.SaveDesktopTUNMode(false)
	s.restoreTunnelMode()
	if s.engine.Mode() != connectcore.ModeProxy {
		t.Fatal("a stored proxy choice was overridden")
	}

	stubTUNPlatform(t, true, false, nil)
	s = tunService(t, platform)
	_ = s.store.SaveDesktopTUNMode(true)
	s.restoreTunnelMode()
	if s.engine.Mode() != connectcore.ModeProxy || !logged(s, "TUN mode is off") {
		t.Fatal("an instance without the rights must stay in proxy mode and say so")
	}

	// Where TUN mode is not offered, a first launch is plain proxy mode and
	// says nothing about TUN.
	stubTUNPlatform(t, false, false, nil)
	s = tunService(t, platform)
	s.restoreTunnelMode()
	if s.engine.Mode() != connectcore.ModeProxy || logged(s, "TUN mode") {
		t.Fatal("a system without TUN mode must start silently in proxy mode")
	}
}

// The engine asks this hook before a TUN connect dials anything.
func TestElevationHookGuardsTUNConnects(t *testing.T) {
	platform := stubTUNPlatform(t, true, false, nil)
	s := tunService(t, platform)
	if err := s.engine.Elevation.Elevate(context.Background()); err == nil {
		t.Fatal("a TUN connect was admitted without administrator rights")
	}
	stubTUNPlatform(t, true, true, nil)
	if err := s.engine.Elevation.Elevate(context.Background()); err != nil {
		t.Fatalf("an elevated TUN connect was refused: %v", err)
	}
	s.SingBoxStopsOnStdinClose = false
	if err := s.engine.Elevation.Elevate(context.Background()); err == nil {
		t.Fatal("a TUN connect was admitted on an external engine")
	}
}

func TestRelaunchedFromReadsTheStartingProcess(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want uint32
	}{
		{nil, 0},
		{[]string{"--some-launcher-flag"}, 0},
		{[]string{relaunchFlag}, 0},
		{[]string{relaunchFlag + "abc"}, 0},
		{[]string{"x", relaunchFlag + "4242"}, 4242},
	} {
		if got := relaunchedFrom(tc.args); got != tc.want {
			t.Errorf("relaunchedFrom(%v) = %d, want %d", tc.args, got, tc.want)
		}
	}
}
