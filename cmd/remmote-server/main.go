// remmote-server: the host side of remmote screen sharing. Captures the
// X11 root window, streams damage-driven JPEG/WebP rect updates over TCP,
// and injects client keyboard/mouse input via XTEST.
//
// WARNING: no authentication, no encryption. Trusted LAN use only.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/lepe/remmote/internal/encode"
	"github.com/lepe/remmote/internal/proto"
	"github.com/lepe/remmote/internal/server"
	"github.com/lepe/remmote/internal/tlsutil"
	"github.com/lepe/remmote/internal/xconn"
)

func main() {
	var (
		display     = flag.String("display", os.Getenv("DISPLAY"), "X display to capture and control")
		listen      = flag.String("listen", ":7677", "TCP listen address")
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
		useTLS      = flag.String("tls", "off", "encrypt the stream: 'auto' (or no value) to generate and print a certificate fingerprint, a shared secret both sides pass, or SHA256:… to assert the -tls-cert certificate")
		tlsCert     = flag.String("tls-cert", "", "with -tls: PEM certificate to use (default: generate and cache one)")
		tlsKey      = flag.String("tls-key", "", "with -tls: PEM private key to use (default: generate and cache one)")
		verbose     = flag.Bool("v", false, "debug logging")
		logJSON     = flag.Bool("log-json", false, "JSON log output")
	)
	// -tls takes an optional value, which the flag package cannot express on
	// its own: reshape the arguments first (see tlsutil.NormalizeArgs).
	flag.CommandLine.Parse(tlsutil.NormalizeArgs(os.Args[1:]))

	log := newLogger(*verbose, *logJSON)

	codecID, err := parseCodec(*codec)
	if err != nil {
		log.Error("bad -codec", "err", err)
		os.Exit(2)
	}
	if *quality < 1 || *quality > 100 {
		log.Error("-quality must be 1..100")
		os.Exit(2)
	}
	if *fps < 1 || *fps > 240 {
		log.Error("-fps must be 1..240")
		os.Exit(2)
	}
	var winID uint64
	if *windowID != "" {
		winID, err = strconv.ParseUint(strings.TrimPrefix(*windowID, "0x"), 16, 32)
		if err != nil {
			log.Error("bad -window", "value", *windowID, "err", err)
			os.Exit(2)
		}
	}
	if (*tlsCert == "") != (*tlsKey == "") {
		log.Error("-tls-cert and -tls-key must be given together")
		os.Exit(2)
	}
	if !tlsutil.On(*useTLS) && (*tlsCert != "" || *tlsKey != "") {
		log.Error("-tls-cert/-tls-key require -tls")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); stop() }()
	srv, err := server.NewContext(ctx, server.Options{
		Display:       *display,
		ListenAddr:    *listen,
		FPS:           *fps,
		Downscale:     *downscale,
		Quality:       *quality,
		Codec:         codecID,
		FullRefresh:   *refresh,
		NoClipboard:   *noClipboard,
		Exec:          *execCmd,
		Window:        uint32(winID),
		Maximize:      *maximize,
		ResizeDesktop: *resizeDesk,
		TLS:           tlsutil.On(*useTLS),
		TLSValue:      *useTLS,
		TLSCertFile:   *tlsCert,
		TLSKeyFile:    *tlsKey,
	}, log)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}

	switch {
	case *dumpFrame != "":
		if err := srv.DumpFrame(*dumpFrame); err != nil {
			log.Error("dump-frame failed", "err", err)
			os.Exit(1)
		}
		w, h := mustDims(*display, log)
		log.Info("frame dumped", "path", *dumpFrame, "width", w, "height", h)
		return
	case *testInj:
		srv.TestInject()
		log.Info("test injection sent: pointer to (150,80), 'a' keypress")
		return
	}

	if err := srv.Run(ctx); err != nil {
		log.Error("server failed", "err", err)
		os.Exit(1)
	}
}

func parseCodec(s string) (uint8, error) {
	switch s {
	case "hybrid":
		return encode.CodecHybrid, nil
	case "zraw":
		return proto.CodecZRAW, nil
	case "jpeg":
		return proto.CodecJPEG, nil
	case "webp":
		return proto.CodecWebP, nil
	default:
		return 0, fmt.Errorf("unknown codec %q (want hybrid, zraw, jpeg or webp)", s)
	}
}

// mustDims reports the screen size for the dump-frame log line.
func mustDims(display string, log *slog.Logger) (uint16, uint16) {
	xc, err := xconn.Dial(display)
	if err != nil {
		return 0, 0
	}
	defer xc.Close()
	w, h := xc.ScreenSize()
	return w, h
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
