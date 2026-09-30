// remmote-client: the viewer side of remmote screen sharing. Opens an X11
// window showing the host's screen and forwards your keyboard and mouse.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/lepe/remmote/internal/auth"
	"github.com/lepe/remmote/internal/client"
	"github.com/lepe/remmote/internal/hub"
	"github.com/lepe/remmote/internal/profile"
	"github.com/lepe/remmote/internal/tlsutil"
)

func main() {
	var (
		display     = flag.String("display", os.Getenv("DISPLAY"), "X display for the viewer window")
		server      = flag.String("server", "", "remmote server address host:port (required)")
		fastScale   = flag.Bool("fast-scale", false, "use faster nearest-neighbor scaling (less smooth when resizing)")
		upscale     = flag.Int("upscale", 1, "magnify the stream by 1, 2 or 4 back to host resolution (match the server's -downscale)")
		useTLS      = flag.String("tls", "off", "encrypt the stream: 'auto' (or no value) to accept any certificate, SHA256:… to pin the server's, or the shared secret the server was started with")
		identity    = flag.String("identity", "", "authenticate as this paired device (credential in ~/.config/remmote/credentials/<name>)")
		quality     = flag.Int("quality", 0, "JPEG/WebP quality 1-100 to request (0 = server default; ZRAW stays lossless)")
		once        = flag.Bool("once", false, "exit after the first keyframe (no window; CI mode)")
		snapshot    = flag.String("snapshot", "", "with -once: write the first full frame as a PNG")
		snapAfter   = flag.Duration("snapshot-after", 0, "with -snapshot: run the live session for D, write the composited canvas as PNG (keyframe + deltas), exit")
		noClipboard = flag.Bool("no-clipboard", false, "disable clipboard synchronization")
		verbose     = flag.Bool("v", false, "debug logging")
		logJSON     = flag.Bool("log-json", false, "JSON log output")
	)
	// -tls takes an optional value, which the flag package cannot express on
	// its own: reshape the arguments first (see tlsutil.NormalizeArgs).
	flag.CommandLine.Parse(tlsutil.NormalizeArgs(os.Args[1:]))

	if *server == "" {
		// Nothing named: open the connection manager — the speed dial,
		// the editor, and the panel for whatever is running.
		log := newLogger(*verbose, *logJSON)
		store, err := profile.Open("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "remmote-client:", err)
			os.Exit(1)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := hub.Run(ctx, hub.Options{Display: *display, Store: store, Log: log}); err != nil {
			if ctx.Err() == nil {
				log.Error("hub failed", "err", err)
				os.Exit(1)
			}
		}
		return
	}
	if *upscale != 1 && *upscale != 2 && *upscale != 4 {
		fmt.Fprintln(os.Stderr, "remmote-client: -upscale must be 1, 2 or 4")
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

	// A paired device carries its own credential and the CA that says
	// which daemon it is talking to; -tls then has nothing to add.
	var tlsCfg *tls.Config
	if *identity != "" {
		id, err := auth.LoadIdentity(auth.DefaultIdentityDir(*identity))
		if err != nil {
			fmt.Fprintln(os.Stderr, "remmote-client:", err)
			os.Exit(2)
		}
		if tlsCfg, err = id.TLSConfig(*server); err != nil {
			fmt.Fprintln(os.Stderr, "remmote-client:", err)
			os.Exit(2)
		}
	}

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
		TLS:           tlsutil.On(*useTLS),
		TLSValue:      *useTLS,
		TLSConfig:     tlsCfg,
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
