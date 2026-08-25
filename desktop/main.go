package main

import (
	"context"
	"embed"
	"fmt"
	"log"
	"os"
	"runtime"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/openrung/openrung/connectcore/proxyconfig"
	"openrung/desktop/vpnservice"
	"openrung/internal/singboxruntime"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	// If this process was launched from a shell using OpenRung's helper, remove
	// only our own inherited loopback proxy values before any HTTP transport can
	// cache them. The parent shell remains activated and unrelated upstream
	// proxy settings remain intact.
	proxyconfig.SanitizeInheritedProxyEnvironment()

	// Before any GUI startup work: the connection engine runs the bundled
	// sing-box engine by re-invoking this binary (singbox.go), and that child
	// must stay headless.
	if handled, err := dispatchSubcommand(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	// WebKitGTK's DMABUF renderer blanks the whole window on some NVIDIA
	// driver combinations; it must be disabled before the webview process is
	// created, which is why this lives here and not in vpnservice.
	if runtime.GOOS == "linux" && os.Getenv("WEBKIT_DISABLE_DMABUF_RENDERER") == "" {
		os.Setenv("WEBKIT_DISABLE_DMABUF_RENDERER", "1")
	}

	svc := vpnservice.New()
	// The engine's sing-box child process is this executable (singbox.go), so
	// there is no external binary to hunt for on a PATH a Finder-launched .app
	// never had. OPENRUNG_SING_BOX overrides that with an external binary —
	// the desktop counterpart of the client's -sing-box flag, and the escape
	// hatch for builds whose linked engine is unusable (see below).
	if external := os.Getenv("OPENRUNG_SING_BOX"); external != "" {
		svc.SingBoxPath = external
	} else {
		exe, err := singboxruntime.SelfPath()
		if err != nil {
			log.Fatal(err)
		}
		svc.SingBoxPath = exe
		// The bundled run shim speaks the stdin-close stop protocol, giving
		// the engine a graceful stop that works on Windows and a tunnel child
		// that unwinds itself if this process dies without running teardown.
		svc.SingBoxStopsOnStdinClose = true
		if !singboxruntime.UTLSEnabled {
			// Only packaged builds (scripts/versioned-wails-build.mjs) carry
			// the engine's build tags; a plain `wails dev` or `go build`
			// binary links an engine that cannot dial any relay, so every
			// connect would fail with upstream's rebuild hint buried in the
			// engine state. Those builds start from a terminal — say so there,
			// with the recovery knobs.
			log.Printf("warning: %s — every connect will fail; rebuild with `wails dev -tags with_utls` or point OPENRUNG_SING_BOX at an external sing-box binary", singboxruntime.VersionLine())
		}
	}

	err := wails.Run(&options.App{
		Title:            "OpenRung",
		Width:            1120,
		Height:           720,
		MinWidth:         900,
		MinHeight:        600,
		BackgroundColour: &options.RGBA{R: 4, G: 12, B: 9, A: 1},
		AssetServer:      &assetserver.Options{Assets: assets},
		OnStartup: func(ctx context.Context) {
			// vpnservice stays free of wails imports (Emitter isolation, see
			// plan §Wails v2→v3); the runtime coupling is confined to here.
			svc.Emitter = func(state vpnservice.NativeVpnState) {
				wailsruntime.EventsEmit(ctx, "openrungStateChanged", state)
			}
			svc.Startup(ctx)
		},
		OnShutdown: func(ctx context.Context) {
			svc.Shutdown(ctx)
		},
		Bind: []interface{}{svc},
		Mac: &mac.Options{
			Appearance: mac.NSAppearanceNameDarkAqua,
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}
