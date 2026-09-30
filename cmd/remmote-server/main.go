// remmote-server: the daemon of remmote screen sharing. It serves the
// control API on one port — a client says what to share, or the flags
// below decide it at startup — and viewers attach over the same port.
// The session lives here: closing a viewer window detaches, terminating
// a session stops this service.
//
// WARNING: without -tls there is no authentication and no encryption.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/lepe/remmote/internal/api"
	"github.com/lepe/remmote/internal/auth"
	"github.com/lepe/remmote/internal/daemon"
	"github.com/lepe/remmote/internal/stream"
	"github.com/lepe/remmote/internal/tlsutil"
	"github.com/lepe/remmote/internal/xconn"
)

func main() {
	var (
		display     = flag.String("display", os.Getenv("DISPLAY"), "X display to capture and control")
		listen      = flag.String("listen", ":7677", "TCP listen address (control API and streams)")
		fps         = flag.Int("fps", 60, "max frames per second (damage merge window)")
		downscale   = flag.Int("downscale", 1, "divide stream width/height by 1, 2 or 4 (less detail, lower CPU/bandwidth)")
		codec       = flag.String("codec", "hybrid", "image codec: hybrid (zstd raw + JPEG fallback, default) | zraw | jpeg | webp (webp requires a -tags webp build)")
		quality     = flag.Int("quality", 75, "JPEG/WebP quality 1-100; ZRAW is lossless (client may override)")
		refresh     = flag.Duration("refresh", 2*time.Second, "periodic full-frame keyframe interval")
		dumpFrame   = flag.String("dump-frame", "", "capture one frame to this PNG path and exit")
		testInj     = flag.Bool("test-inject", false, "inject a pointer move and a keystroke, then exit")
		noClipboard = flag.Bool("no-clipboard", false, "disable clipboard synchronization")
		execCmd     = flag.String("exec", "", "run this command and share only its windows (e.g. -exec xcalc)")
		windowID    = flag.String("window", "", "share this existing window id (hex) and windows it spawns")
		maximize    = flag.Bool("maximize", false, "with -exec/-window: maximize the shared window on the host screen")
		resizeDesk  = flag.Bool("resize-desktop", false, "whole-desktop mode: let a viewer resizing its window resize the host screen too (best effort via RANDR)")
		idle        = flag.Bool("idle", false, "share nothing at startup; wait for a client to say what to share")
		allowExec   = flag.String("allow-exec", "", "comma-separated commands clients may launch (source app); empty refuses them all")
		useAuth     = flag.Bool("auth", false, "admit only paired devices — each named, roled and revocable; brings its own TLS")
		authDir     = flag.String("auth-dir", "", "with -auth: where the authority and its roster live (default ~/.config/remmote/daemon)")
		insecure    = flag.Bool("insecure", false, "allow an unencrypted listener that is not loopback-only (never on a shared network)")
		useTLS      = flag.String("tls", "off", "encrypt the listener: 'auto' (or no value) to generate and print a certificate fingerprint, a shared secret both sides pass, or SHA256:… to assert the -tls-cert certificate")
		tlsCert     = flag.String("tls-cert", "", "with -tls: PEM certificate to use (default: generate and cache one)")
		tlsKey      = flag.String("tls-key", "", "with -tls: PEM private key to use (default: generate and cache one)")
		verbose     = flag.Bool("v", false, "debug logging")
		logJSON     = flag.Bool("log-json", false, "JSON log output")
	)
	// -tls takes an optional value, which the flag package cannot express on
	// its own: reshape the arguments first (see tlsutil.NormalizeArgs).
	flag.CommandLine.Parse(tlsutil.NormalizeArgs(os.Args[1:]))

	log := newLogger(*verbose, *logJSON)

	if (*tlsCert == "") != (*tlsKey == "") {
		log.Error("-tls-cert and -tls-key must be given together")
		os.Exit(2)
	}
	if !tlsutil.On(*useTLS) && (*tlsCert != "" || *tlsKey != "") {
		log.Error("-tls-cert/-tls-key require -tls")
		os.Exit(2)
	}
	if *useAuth && tlsutil.On(*useTLS) {
		log.Error("-auth brings its own TLS (the authority signs the daemon's certificate); drop -tls")
		os.Exit(2)
	}
	authPath := *authDir
	if authPath == "" {
		authPath = filepath.Join(auth.ConfigHome(), "daemon")
	}
	if *useAuth && authPath == "" {
		log.Error("-auth needs a directory for the authority (-auth-dir)")
		os.Exit(2)
	}
	if *idle && (*dumpFrame == "" && !*testInj) {
		// -idle shares nothing at startup; the sharing flags would be
		// silently ignored, so say no instead.
		set := map[string]bool{}
		flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
		for _, name := range []string{"display", "exec", "window", "maximize",
			"resize-desktop", "codec", "fps", "quality", "downscale", "refresh", "no-clipboard"} {
			if set[name] {
				log.Error("-idle shares nothing at startup and cannot be combined with -" + name)
				os.Exit(2)
			}
		}
	}

	// The flags describe one session — what today's script calls "the
	// configuration". They are the startup session unless -idle.
	spec := api.SessionSpec{
		Source:   api.SourceDesktop,
		Display:  api.DisplaySpec{Kind: api.KindExisting, Name: *display},
		Maximize: *maximize,
		Stream: api.StreamSpec{
			Codec:         *codec,
			FPS:           *fps,
			Quality:       *quality,
			Downscale:     *downscale,
			Refresh:       refresh.String(),
			Clipboard:     boolPtr(!*noClipboard),
			ResizeDesktop: *resizeDesk,
		},
	}
	switch {
	case *execCmd != "":
		spec.Source = api.SourceApp
		spec.App = &api.AppSpec{Command: *execCmd}
	case *windowID != "":
		spec.Source = api.SourceWindow
		spec.Window = &api.WindowSpec{ID: *windowID}
	}
	if err := spec.Validate(); err != nil {
		log.Error("bad options", "err", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); stop() }()

	// Diagnostics want the stream and nothing else.
	if *dumpFrame != "" || *testInj {
		opts, err := spec.StreamOptions()
		if err != nil {
			log.Error("bad options", "err", err)
			os.Exit(2)
		}
		st, err := stream.NewContext(ctx, opts, log)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error("startup failed", "err", err)
			os.Exit(1)
		}
		defer st.Close()
		if *dumpFrame != "" {
			if err := st.DumpFrame(*dumpFrame); err != nil {
				log.Error("dump-frame failed", "err", err)
				os.Exit(1)
			}
			w, h := mustDims(*display, log)
			log.Info("frame dumped", "path", *dumpFrame, "width", w, "height", h)
			return
		}
		st.TestInject()
		log.Info("test injection sent: pointer to (150,80), 'a' keypress")
		return
	}

	dopts := daemon.Options{
		ListenAddr:  *listen,
		TLS:         tlsutil.On(*useTLS),
		TLSValue:    *useTLS,
		TLSCertFile: *tlsCert,
		TLSKeyFile:  *tlsKey,
		AllowExec:   splitList(*allowExec),
		Insecure:    *insecure,
		Log:         log,
	}
	if *useAuth {
		dopts.AuthDir = authPath
	}
	if !*idle {
		dopts.Initial = &spec
	}
	d, err := daemon.New(dopts)
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
	if err := d.Run(ctx); err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Error("server failed", "err", err)
		os.Exit(1)
	}
}

func boolPtr(b bool) *bool { return &b }

// splitList reads a comma-separated flag value into trimmed names.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// mustDims reports the screen size for the dump-frame log line.
func mustDims(display string, log *slog.Logger) (int, int) {
	xc, err := xconn.Dial(display)
	if err != nil {
		return 0, 0
	}
	defer xc.Close()
	w, h := xc.ScreenSize()
	return int(w), int(h)
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
