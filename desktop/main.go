package main

import (
	"context"
	"embed"
	"log"
	"os"
	"runtime"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"openrung/desktop/vpnservice"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	// WebKitGTK's DMABUF renderer blanks the whole window on some NVIDIA
	// driver combinations; it must be disabled before the webview process is
	// created, which is why this lives here and not in vpnservice.
	if runtime.GOOS == "linux" && os.Getenv("WEBKIT_DISABLE_DMABUF_RENDERER") == "" {
		os.Setenv("WEBKIT_DISABLE_DMABUF_RENDERER", "1")
	}

	// A Finder-launched .app inherits a minimal PATH without Homebrew, so
	// resolve external tools explicitly (dev worked only because it inherited
	// the shell PATH).
	ensureExternalToolPath()

	svc := vpnservice.New()
	svc.SingBoxPath = resolveSingBoxPath()

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
