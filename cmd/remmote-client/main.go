// remmote-client: the viewer side of remmote screen sharing. Opens an X11
// window showing the host's screen and forwards your keyboard and mouse.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/lepe/remmote/internal/client"
)

func main() {
	var (
		display     = flag.String("display", os.Getenv("DISPLAY"), "X display for the viewer window")
		server      = flag.String("server", "", "remmote server address host:port (required)")
		quality     = flag.Int("quality", 0, "encoder quality 1-100 to request (0 = keep server default)")
		once        = flag.Bool("once", false, "exit after the first keyframe (no window; CI mode)")
		snapshot    = flag.String("snapshot", "", "with -once: write the first full frame as a PNG")
		noClipboard = flag.Bool("no-clipboard", false, "disable clipboard synchronization")
		verbose     = flag.Bool("v", false, "debug logging")
		logJSON     = flag.Bool("log-json", false, "JSON log output")
	)
	flag.Parse()

	if *server == "" {
		fmt.Fprintln(os.Stderr, "remmote-client: -server is required")
		flag.Usage()
		os.Exit(2)
	}
	if *quality != 0 && (*quality < 1 || *quality > 100) {
		fmt.Fprintln(os.Stderr, "remmote-client: -quality must be 1..100 or 0")
		os.Exit(2)
	}

	log := newLogger(*verbose, *logJSON)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := client.Run(ctx, client.Options{
		Display:     *display,
		ServerAddr:  *server,
		Quality:     *quality,
		Once:        *once,
		Snapshot:    *snapshot,
		NoClipboard: *noClipboard,
	}, log); err != nil {
		log.Error("client failed", "err", err)
		os.Exit(1)
	}
}

func newLogger(verbose, json bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	if json {
		return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}
