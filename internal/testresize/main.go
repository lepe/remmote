// testresize is the integration-test driver for viewer-driven resizing.
// It speaks the remmote wire protocol directly, so a test can ask a
// server for a new surface size and watch what comes back, and it can
// resize the viewer's own window (by matching its WM_NAME) so the real
// client path — ConfigureNotify → debounced Resize message — is exercised
// too.
//
// Exit status: 0 = expectations met, 1 = connection/usage error,
// 2 = expectation not met.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"

	"github.com/lepe/remmote/internal/proto"
)

func main() {
	var (
		server   = flag.String("server", "", "speak the protocol to this remmote server (host:port)")
		size     = flag.String("size", "", "surface size to request, WxH (stream coordinates)")
		expect   = flag.String("expect", "resize", "what to expect: resize | none")
		keyframe = flag.Bool("keyframe", false, "require a keyframe too (proves the server still streams)")
		wait     = flag.Duration("wait", 6*time.Second, "how long to wait for it")
		upscale  = flag.Int("upscale", 1, "client -upscale: divide the requested size by this")
		winPat   = flag.String("resize-window", "", "resize the local window whose WM_NAME starts with this, to -size")
		display  = flag.String("display", "", "X display for -resize-window (default: $DISPLAY)")
	)
	flag.Parse()

	if *winPat != "" {
		w, h, err := parseSize(*size)
		if err != nil {
			fatal(err)
		}
		if err := resizeWindow(*display, *winPat, w, h, *wait); err != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
			os.Exit(1)
		}
		return
	}
	if *server == "" {
		fmt.Fprintln(os.Stderr, "usage: testresize -server host:port [-size WxH] [-expect resize|none] [-keyframe] | -resize-window PREFIX WxH")
		os.Exit(1)
	}

	w, h := 0, 0
	var err error
	if *size != "" {
		if w, h, err = parseSize(*size); err != nil {
			fatal(err)
		}
		w, h = max(1, w/max(*upscale, 1)), max(1, h/max(*upscale, 1))
	}
	wantResize := *expect == "resize"
	if err := session(*server, w, h, wantResize, *keyframe, *wait); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(2)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "ERROR:", err)
	os.Exit(1)
}

func parseSize(s string) (int, int, error) {
	var w, h int
	if _, err := fmt.Sscanf(s, "%dx%d", &w, &h); err != nil || w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("bad -size %q, want WxH", s)
	}
	return w, h, nil
}

// session connects, optionally asks for a new size, and reports what the
// server announces. A matching ScreenResize is required when one is
// wanted, and forbidden when it is not.
func session(addr string, w, h int, wantResize, wantKeyframe bool, wait time.Duration) error {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(wait + 3*time.Second))
	br := bufio.NewReader(conn)

	if err := proto.WriteMsg(conn, proto.MsgClientHello, 0,
		(&proto.ClientHello{Version: proto.ProtoVersion}).Encode()); err != nil {
		return err
	}
	t, _, payload, err := proto.ReadMsg(br)
	if err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	if t != proto.MsgServerHello {
		return fmt.Errorf("expected ServerHello, got %v", t)
	}
	hello, err := proto.DecodeServerHello(payload)
	if err != nil {
		return err
	}
	fmt.Printf("HELLO %dx%d name=%s\n", hello.Width, hello.Height, hello.Name)

	if w > 0 {
		if err := proto.WriteMsg(conn, proto.MsgResize, 0,
			(&proto.Resize{Width: uint16(w), Height: uint16(h)}).Encode()); err != nil {
			return err
		}
		fmt.Printf("SENT %dx%d\n", w, h)
	}

	deadline := time.Now().Add(wait)
	gotResize, gotKeyframe := false, false
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		t, flags, payload, err := proto.ReadMsg(br)
		if err != nil {
			break // deadline: fall through to the verdict
		}
		switch t {
		case proto.MsgScreenResize:
			m, err := proto.DecodeScreenResize(payload)
			if err != nil {
				return err
			}
			fmt.Printf("RESIZE %d %d\n", m.Width, m.Height)
			if wantResize && w > 0 && near(int(m.Width), int(m.Height), w, h) {
				gotResize = true
			}
		case proto.MsgRectUpdate:
			if flags&proto.FlagKeyframe != 0 && !gotKeyframe {
				m, err := proto.DecodeRectUpdate(payload)
				if err != nil {
					return err
				}
				fmt.Printf("KEYFRAME %d %d\n", m.W, m.H)
				gotKeyframe = true
			}
		case proto.MsgPing:
			_ = proto.WriteMsg(conn, proto.MsgPong, 0, (&proto.PingPong{}).Encode())
		}
		if (!wantResize || gotResize) && (!wantKeyframe || gotKeyframe) {
			break
		}
	}

	switch {
	case wantResize && !gotResize:
		return fmt.Errorf("no ScreenResize near %dx%d within %s", w, h, wait)
	case !wantResize && gotResize:
		return fmt.Errorf("server resized the surface although it should not have")
	case wantKeyframe && !gotKeyframe:
		return fmt.Errorf("no keyframe within %s: the server stopped streaming", wait)
	}
	fmt.Println("OK")
	return nil
}

// close allows the canvas grid (32 px) its slack: a window-mode canvas
// lands within 16 px of the request, a desktop resize exactly on it.
func near(gotW, gotH, wantW, wantH int) bool {
	return abs(gotW-wantW) <= 20 && abs(gotH-wantH) <= 20
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// resizeWindow finds a local window whose WM_NAME starts with pattern
// and resizes it — the user's side of a viewer resize.
func resizeWindow(display, pattern string, w, h int, wait time.Duration) error {
	if display == "" {
		display = os.Getenv("DISPLAY")
	}
	x, err := xgb.NewConnDisplay(display)
	if err != nil {
		return fmt.Errorf("open display %q: %w", display, err)
	}
	defer x.Close()
	root := xproto.Setup(x).DefaultScreen(x).Root

	deadline := time.Now().Add(wait)
	for {
		win, err := findWindow(x, root, pattern)
		if err == nil {
			if err := xproto.ConfigureWindowChecked(x, win,
				xproto.ConfigWindowWidth|xproto.ConfigWindowHeight,
				[]uint32{uint32(w), uint32(h)}).Check(); err != nil {
				return fmt.Errorf("resize window %#x: %w", uint32(win), err)
			}
			fmt.Printf("WINDOW %#x resized to %dx%d\n", uint32(win), w, h)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no window titled %q* on %s: %w", pattern, display, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// findWindow walks the tree for a window whose WM_NAME starts with
// pattern, descending through any frame a window manager put in between.
func findWindow(x *xgb.Conn, root xproto.Window, pattern string) (xproto.Window, error) {
	var found xproto.Window
	seen := map[xproto.Window]bool{}
	var walk func(w xproto.Window, depth int)
	walk = func(w xproto.Window, depth int) {
		if found != 0 || depth > 4 || seen[w] {
			return
		}
		seen[w] = true
		if w != root {
			if name, ok := wmName(x, w); ok && strings.HasPrefix(name, pattern) {
				found = w
				return
			}
		}
		r, err := xproto.QueryTree(x, w).Reply()
		if err != nil {
			return
		}
		for _, c := range r.Children {
			walk(c, depth+1)
		}
	}
	walk(root, 0)
	if found == 0 {
		return 0, fmt.Errorf("no window titled %q*", pattern)
	}
	return found, nil
}

func wmName(x *xgb.Conn, w xproto.Window) (string, bool) {
	prop, err := xproto.GetProperty(x, false, w, xproto.AtomWmName, 0, 0, 64).Reply()
	if err != nil || prop.Format != 8 || len(prop.Value) == 0 {
		return "", false
	}
	return string(prop.Value), true
}
