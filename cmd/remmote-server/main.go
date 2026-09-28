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

	"github.com/lepe/remmote/internal/proto"
	"github.com/lepe/remmote/internal/server"
	"github.com/lepe/remmote/internal/xconn"
)

func main() {
	var (
		display     = flag.String("display", os.Getenv("DISPLAY"), "X display to capture and control")
		listen      = flag.String("listen", ":7677", "TCP listen address")
		fps         = flag.Int("fps", 30, "max frames per second (damage merge window)")
		codec       = flag.String("codec", "jpeg", "image codec: jpeg | webp (webp requires a -tags webp build)")
		quality     = flag.Int("quality", 75, "encoder quality 1-100 (client may override)")
		refresh     = flag.Duration("refresh", 2*time.Second, "periodic full-frame keyframe interval")
		dumpFrame   = flag.String("dump-frame", "", "capture one frame to this PNG path and exit")
		testInj     = flag.Bool("test-inject", false, "inject a pointer move and a keystroke, then exit")
		noClipboard = flag.Bool("no-clipboard", false, "disable clipboard synchronization")
		execCmd     = flag.String("exec", "", "run this command and share only its windows (e.g. -exec xcalc)")
		windowID    = flag.String("window", "", "share this existing window id (hex) and windows it spawns")
		verbose     = flag.Bool("v", false, "debug logging")
		logJSON     = flag.Bool("log-json", false, "JSON log output")
	)
	flag.Parse()

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
	var winID uint64
	if *windowID != "" {
		winID, err = strconv.ParseUint(strings.TrimPrefix(*windowID, "0x"), 16, 32)
		if err != nil {
			log.Error("bad -window", "value", *windowID, "err", err)
			os.Exit(2)
		}
	}

	srv, err := server.New(server.Options{
		Display:     *display,
		ListenAddr:  *listen,
		FPS:         *fps,
		Quality:     *quality,
		Codec:       codecID,
		FullRefresh: *refresh,
		NoClipboard: *noClipboard,
		Exec:        *execCmd,
		Window:      uint32(winID),
	}, log)
	if err != nil {
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx); err != nil {
		log.Error("server failed", "err", err)
		os.Exit(1)
	}
}

func parseCodec(s string) (uint8, error) {
	switch s {
	case "jpeg":
		return proto.CodecJPEG, nil
	case "webp":
		return proto.CodecWebP, nil
	default:
		return 0, fmt.Errorf("unknown codec %q (want jpeg or webp)", s)
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
