package server

import (
	"bufio"
	"bytes"
	"context"
	"image"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/lepe/remmote/internal/proto"
)

// fakeResizer records what the capture loop would apply.
type fakeResizer struct {
	got chan [2]int
	err error
}

func (f *fakeResizer) ResizeTo(w, h int) error {
	select {
	case f.got <- [2]int{w, h}:
	default:
	}
	return f.err
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil))
}

// A viewer's window size arrives in stream coordinates, is multiplied
// back by -downscale like MouseMove, and reaches the surface resizer
// only through the capture loop's channel.
func TestResizeRequestReachesResizerOnCaptureLoop(t *testing.T) {
	srv := &Server{
		log:       discardLogger(),
		opts:      Options{Downscale: 2},
		resizer:   &fakeResizer{got: make(chan [2]int, 1)},
		resizeReq: make(chan image.Point, 1),
	}
	srvPeer, cliPeer := net.Pipe()
	defer cliPeer.Close()
	sess := &session{id: 1, srv: srv, conn: srvPeer, br: bufio.NewReader(srvPeer)}
	done := make(chan struct{})
	go func() { defer close(done); sess.reader(context.Background()) }()

	if err := proto.WriteMsg(cliPeer, proto.MsgResize, 0,
		(&proto.Resize{Width: 800, Height: 600}).Encode()); err != nil {
		t.Fatal(err)
	}
	var p image.Point
	select {
	case p = <-srv.resizeReq:
		if p.X != 1600 || p.Y != 1200 {
			t.Fatalf("capture loop got %v, want 1600x1200 (800x600 × downscale 2)", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no resize request reached the capture loop")
	}
	srv.applyResize(p)
	if got := <-srv.resizer.(*fakeResizer).got; got != [2]int{1600, 1200} {
		t.Fatalf("resizer got %v, want 1600x1200", got)
	}

	cliPeer.Close()
	<-done
}

// Nonsense sizes from the wire are clamped, never passed on: a 1 px or
// 65535 px request must not reach X.
func TestResizeRequestIsClamped(t *testing.T) {
	srv := &Server{log: discardLogger(), resizer: &fakeResizer{got: make(chan [2]int, 1)},
		resizeReq: make(chan image.Point, 1)}
	srv.requestResize(1, 1)
	if p := <-srv.resizeReq; p.X != minResizePx || p.Y != minResizePx {
		t.Fatalf("min clamp = %v, want %dpx", p, minResizePx)
	}
	srv.requestResize(60000, 60000)
	if p := <-srv.resizeReq; p.X != maxResizePx || p.Y != maxResizePx {
		t.Fatalf("max clamp = %v, want %dpx", p, maxResizePx)
	}
}

// Whole-desktop mode without -resize-desktop: the request is dropped
// with exactly one warning, however many viewers keep asking.
func TestDesktopResizeNeedsTheFlagAndWarnsOnce(t *testing.T) {
	var logs bytes.Buffer
	srv := &Server{
		log:       slog.New(slog.NewTextHandler(&logs, nil)),
		opts:      Options{},
		desktop:   true,
		resizer:   &fakeResizer{got: make(chan [2]int, 1)},
		resizeReq: make(chan image.Point, 1),
	}
	for i := 0; i < 3; i++ {
		srv.requestResize(1024, 768)
	}
	select {
	case p := <-srv.resizeReq:
		t.Fatalf("resize reached the capture loop without -resize-desktop: %v", p)
	default:
	}
	if n := strings.Count(logs.String(), "-resize-desktop"); n != 1 {
		t.Fatalf("warning logged %d times, want exactly 1:\n%s", n, logs.String())
	}
}

// With the flag on, the same request is handed to the capture loop.
func TestDesktopResizeWithFlagIsApplied(t *testing.T) {
	srv := &Server{log: discardLogger(), opts: Options{ResizeDesktop: true}, desktop: true,
		resizer:   &fakeResizer{got: make(chan [2]int, 1)},
		resizeReq: make(chan image.Point, 1)}
	srv.requestResize(1024, 768)
	select {
	case p := <-srv.resizeReq:
		if p.X != 1024 || p.Y != 768 {
			t.Fatalf("capture loop got %v, want 1024x768", p)
		}
	default:
		t.Fatal("no resize request queued")
	}
}

// The latest request replaces an older one still waiting: a drag
// produces many sizes and only the last matters.
func TestResizeRequestKeepsOnlyTheLatest(t *testing.T) {
	srv := &Server{log: discardLogger(), resizer: &fakeResizer{got: make(chan [2]int, 1)},
		resizeReq: make(chan image.Point, 1)}
	srv.requestResize(800, 600)
	srv.requestResize(900, 700)
	srv.requestResize(1024, 768)
	if p := <-srv.resizeReq; p.X != 1024 || p.Y != 768 {
		t.Fatalf("kept %v, want only the newest 1024x768", p)
	}
	select {
	case p := <-srv.resizeReq:
		t.Fatalf("stale request survived: %v", p)
	default:
	}
}

// An empty size never gets past decode, so nothing reaches the queue.
func TestResizeMessageRejectsEmptySize(t *testing.T) {
	srv := &Server{log: discardLogger(), resizer: &fakeResizer{got: make(chan [2]int, 1)},
		resizeReq: make(chan image.Point, 1)}
	srvPeer, cliPeer := net.Pipe()
	defer cliPeer.Close()
	sess := &session{id: 1, srv: srv, conn: srvPeer, br: bufio.NewReader(srvPeer)}
	done := make(chan struct{})
	go func() { defer close(done); sess.reader(context.Background()) }()

	if err := proto.WriteMsg(cliPeer, proto.MsgResize, 0,
		(&proto.Resize{Width: 0, Height: 100}).Encode()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	select {
	case p := <-srv.resizeReq:
		t.Fatalf("empty resize reached the capture loop: %v", p)
	default:
	}
	cliPeer.Close()
	<-done
}
