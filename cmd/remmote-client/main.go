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
		fastScale   = flag.Bool("fast-scale", false, "use faster nearest-neighbor scaling (less smooth when resizing)")
		upscale     = flag.Int("upscale", 1, "magnify the stream by 1, 2 or 4 back to host resolution (match the server's -downscale)")
		useTLS      = flag.Bool("tls", false, "encrypt the stream (the server must be started with -tls)")
		tlsPin      = flag.String("tls-fingerprint", "", "also verify the server certificate against this SHA-256 fingerprint (printed by the server); without it the stream is encrypted but not verified")
		quality     = flag.Int("quality", 0, "JPEG/WebP quality 1-100 to request (0 = server default; ZRAW stays lossless)")
		once        = flag.Bool("once", false, "exit after the first keyframe (no window; CI mode)")
		snapshot    = flag.String("snapshot", "", "with -once: write the first full frame as a PNG")
		snapAfter   = flag.Duration("snapshot-after", 0, "with -snapshot: run the live session for D, write the composited canvas as PNG (keyframe + deltas), exit")
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
	if *upscale != 1 && *upscale != 2 && *upscale != 4 {
		fmt.Fprintln(os.Stderr, "remmote-client: -upscale must be 1, 2 or 4")
		os.Exit(2)
	}
	if !*useTLS && *tlsPin != "" {
		fmt.Fprintln(os.Stderr, "remmote-client: -tls-fingerprint requires -tls")
		os.Exit(2)
	}
	if *quality != 0 && (*quality < 1 || *quality > 100) {
		fmt.Fprintln(os.Stderr, "remmote-client: -quality must be 1..100 or 0")
		os.Exit(2)
	}
	if *snapAfter > 0 && *snapshot == "" {
		fmt.Fprintln(os.Stderr, "remmote-client: -snapshot-after requires -snapshot")
		os.Exit(2)
	}
	if *snapAfter > 0 && *once {
		fmt.Fprintln(os.Stderr, "remmote-client: -snapshot-after runs the live session, so it cannot be combined with -once")
		os.Exit(2)
	}

	log := newLogger(*verbose, *logJSON)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); stop() }()

	if err := client.Run(ctx, client.Options{
		Display:       *display,
		ServerAddr:    *server,
		Quality:       *quality,
		FastScale:     *fastScale,
		Upscale:       *upscale,
		Once:          *once,
		Snapshot:      *snapshot,
		SnapshotAfter: *snapAfter,
		NoClipboard:   *noClipboard,
		TLS:           *useTLS,
		TLSPin:        *tlsPin,
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
