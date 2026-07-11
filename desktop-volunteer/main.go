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

	"openrung/desktop-volunteer/volunteerservice"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	// WebKitGTK's DMABUF renderer blanks the whole window on some NVIDIA
	// driver combinations; it must be disabled before the webview process is
	// created (same workaround as the desktop client).
	if runtime.GOOS == "linux" && os.Getenv("WEBKIT_DISABLE_DMABUF_RENDERER") == "" {
		os.Setenv("WEBKIT_DISABLE_DMABUF_RENDERER", "1")
	}

	// A Finder-launched .app inherits a minimal PATH without Homebrew, so
	// resolve external tools explicitly.
	ensureExternalToolPath()

	svc := volunteerservice.New()
	svc.XrayPath, svc.XrayFound = resolveXrayPath()

	err := wails.Run(&options.App{
		Title:            "OpenRung Volunteer",
		Width:            980,
		Height:           700,
		MinWidth:         820,
		MinHeight:        600,
		BackgroundColour: &options.RGBA{R: 4, G: 12, B: 9, A: 1},
		AssetServer:      &assetserver.Options{Assets: assets},
		OnStartup: func(ctx context.Context) {
			// volunteerservice stays free of wails imports (same Emitter
			// isolation rule as the desktop client's vpnservice).
			svc.Emitter = func(state volunteerservice.State) {
				wailsruntime.EventsEmit(ctx, "volunteerStateChanged", state)
			}
			svc.Startup(ctx)
		},
		OnBeforeClose: func(ctx context.Context) (prevent bool) {
			if !svc.Running() {
				return false
			}
			choice, err := wailsruntime.MessageDialog(ctx, wailsruntime.MessageDialogOptions{
				Type:          wailsruntime.QuestionDialog,
				Title:         "Stop volunteering?",
				Message:       "Quitting stops your relay, and people connected through it will be moved to other volunteers. Quit anyway?",
				Buttons:       []string{"Quit", "Keep running"},
				DefaultButton: "Keep running",
				CancelButton:  "Keep running",
			})
			// Fail safe: prevent the quit unless the user affirmatively chose to
			// quit. Wails maps custom labels only on macOS; GTK/Windows return
			// "Yes"/"No" (or "" for Escape / window-close) and a dialog error
			// returns "" too — every one of those must keep the relay running,
			// never silently drop the people using it.
			if err != nil {
				return true
			}
			return choice != "Quit" && choice != "Yes"
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
