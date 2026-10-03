//go:build hub

// remmote-hub: the connection manager — a window with the connections
// saved on this machine, a form for what to share, and a panel for what
// the host is running.
//
// It is built separately from the rest (make build-hub) because it is
// the one program here that needs a webview (webkit2gtk on Linux); the
// daemon, the CLI and the video viewer stay CGo-free. The viewer is a
// window of its own and stays native — closing it detaches, nothing
// more.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"

	"github.com/lepe/remmote/internal/hub"
	"github.com/lepe/remmote/internal/profile"
	"github.com/lepe/remmote/internal/version"
)

func main() {
	var (
		display     = flag.String("display", os.Getenv("DISPLAY"), "X display for the viewer window")
		verbose     = flag.Bool("v", false, "debug logging")
		logJSON     = flag.Bool("log-json", false, "JSON log output")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVersion {
		fmt.Printf("remmote-hub %s\n", version.Version)
		return
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	out := os.Stderr
	var log *slog.Logger
	if *logJSON {
		log = slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: level}))
	} else {
		log = slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level}))
	}

	store, err := profile.Open("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "remmote-hub:", err)
		os.Exit(1)
	}
	app := hub.NewApp(hub.NewService(store, *display, log))

	if err := wails.Run(&options.App{
		Title:            app.Title(),
		Width:            900,
		Height:           640,
		MinWidth:         720,
		MinHeight:        520,
		BackgroundColour: &options.RGBA{R: 20, G: 24, B: 31, A: 255},
		AssetServer:      &assetserver.Options{Assets: hub.Assets},
		OnStartup:        app.Startup,
		OnShutdown:       app.Shutdown,
		Bind:             []interface{}{app},
		Linux:            &linux.Options{ProgramName: "remmote"},
	}); err != nil {
		log.Error("hub failed", "err", err)
		os.Exit(1)
	}
}
