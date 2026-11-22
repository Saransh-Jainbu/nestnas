// NestNAS Desktop: the NestNAS dashboard in its own native window.
//
// It attaches to a NestNAS server that is already running on this machine (for example the
// background service), or starts one itself. Phones and other computers keep using the web
// address; this window is just the friendly front door for the laptop that hosts the NAS.
package main

import (
	"context"
	_ "embed"
	"fmt"
	"net/http"
	"os"
	"strconv"

	"github.com/Saransh-Jainbu/nestnas/pkg/app"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/menu"
	"github.com/wailsapp/wails/v2/pkg/menu/keys"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed icon.png
var icon []byte

var version = "0.1.0-dev"

func main() {
	cleanSnapEnv()
	port := 8080
	if p, err := strconv.Atoi(os.Getenv("NESTNAS_PORT")); err == nil && p > 0 {
		port = p
	}
	tlsPort := 8443
	if p, err := strconv.Atoi(os.Getenv("NESTNAS_TLS_PORT")); err == nil && p > 0 {
		tlsPort = p
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)

	// Only one NestNAS window: if one is already open, raise it and exit.
	raise, ok := singleInstance()
	if !ok {
		return
	}

	// Attach to a running server (e.g. the background service) or start our own.
	var started *app.Instance
	if !app.Probe(port) {
		in, err := app.Start(app.Options{Port: port, TLSPort: tlsPort, Version: version})
		if err != nil {
			fmt.Fprintln(os.Stderr, "NestNAS could not start:", err)
			os.Exit(1)
		}
		started = in
		defer in.Close()
	}

	var ctx context.Context
	appMenu := menu.NewMenu()
	m := appMenu.AddSubmenu("NestNAS")
	m.AddText("Open in browser", keys.CmdOrCtrl("b"), func(*menu.CallbackData) { runtime.BrowserOpenURL(ctx, url) })
	m.AddText("Show phone / network addresses", nil, func(*menu.CallbackData) {
		msg := "Open one of these on a phone or another computer on the same Wi-Fi (encrypted with HTTPS):\n\n"
		for _, u := range app.DeviceURLs(tlsPort) {
			msg += u + "\n"
		}
		msg += "\nOn a new device, first install the NestNAS certificate (Security tab in the dashboard)."
		_, _ = runtime.MessageDialog(ctx, runtime.MessageDialogOptions{Type: runtime.InfoDialog, Title: "NestNAS addresses", Message: msg})
	})
	m.AddSeparator()
	m.AddText("Quit", keys.CmdOrCtrl("q"), func(*menu.CallbackData) { runtime.Quit(ctx) })

	// The window loads a tiny page that forwards to the local NestNAS dashboard.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta http-equiv="refresh" content="0;url=%s"><body style="font:16px system-ui;background:#12151b;color:#e6e9ef;display:grid;place-items:center;height:100vh;margin:0">Starting NestNAS…</body>`, url)
	})

	err := wails.Run(&options.App{
		Title:            "NestNAS",
		Width:            1100,
		Height:           760,
		MinWidth:         420,
		MinHeight:        500,
		BackgroundColour: &options.RGBA{R: 18, G: 21, B: 27, A: 1},
		Menu:             appMenu,
		AssetServer:      &assetserver.Options{Handler: handler},
		Linux:            &linux.Options{Icon: icon, ProgramName: "NestNAS"},
		OnStartup: func(c context.Context) {
			ctx = c
			go func() { // a second launch pings this socket; raise our window
				for range raise {
					runtime.WindowUnminimise(ctx)
					runtime.Show(ctx)
				}
			}()
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "NestNAS window error:", err)
		os.Exit(1)
	}
	_ = started
}
