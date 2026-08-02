package main

import (
	"embed"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	// Sampled before Wails touches anything, so this is the baseline: whatever the Go
	// runtime installed. Enabled by LOCALCHAT_SIGPROBE=1 — see sigprobe_linux.go.
	probeSignals("main() entry")

	// Create an instance of the app structure
	app := NewApp()

	// Allow the database path to be overridden on the command line with --db
	// <path>. Checked before wails.Run so it is available to startup().
	for i, arg := range os.Args[1:] {
		if (arg == "--db" || arg == "-db") && i+1 < len(os.Args[1:]) {
			app.dbPath = os.Args[i+2]
			break
		}
	}

	// Open DuckDB before Wails/WebKit starts. DuckDB's initialization — schema
	// setup plus WAL replay if a prior run crashed — pins cgo threads. Doing it
	// here, before GTK creates its own OS threads, eliminates the thread
	// contention that can hang the UI when both are starting at the same time.
	if err := app.preopenStore(); err != nil {
		println("Error: could not open database:", err.Error())
		os.Exit(1)
	}

	// Create application with options
	err := wails.Run(&options.App{
		Title:  "LocalChat",
		Width:  1024,
		Height: 768,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 27, G: 38, B: 54, A: 1},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		Bind: []interface{}{
			app,
		},
	})

	probeSignals("after wails.Run returned")

	if err != nil {
		println("Error:", err.Error())
	}
}
