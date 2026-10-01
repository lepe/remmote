// testtype types into a window — the driver for interface tests. It
// finds a window by its title, puts the pointer over it (keyboard focus
// follows the pointer when no window manager is running), and sends
// keystrokes through XTEST, the same injector remmote uses for a remote
// user's input.
//
//	testtype -display :987 -window remmote -key Tab -text "host:7677" -key Tab
//
// Exit status: 0 = typed, 1 = no such window or a key it does not know.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"

	"github.com/lepe/remmote/internal/input"
	"github.com/lepe/remmote/internal/xconn"
)

// named keys, for -key.
var special = map[string]xproto.Keysym{
	"Return":    0xff0d,
	"Tab":       0xff09,
	"Escape":    0xff1b,
	"BackSpace": 0xff08,
	"Delete":    0xffff,
	"Up":        0xff52,
	"Down":      0xff54,
	"Left":      0xff51,
	"Right":     0xff53,
	"Home":      0xff50,
	"End":       0xff57,
	"Space":     0x20,
	"Alt_L":     0xffe9,
	"Alt_R":     0xffea,
	"Control_L": 0xffe3,
	"Control_R": 0xffe4,
	"Shift_L":   0xffe1,
	"Shift_R":   0xffe2,
}

func main() {
	var (
		display = flag.String("display", os.Getenv("DISPLAY"), "X display to type into")
		window  = flag.String("window", "", "window whose title starts with this")
		text    = flag.String("text", "", "text to type")
		keys    = flag.String("key", "", "comma-separated keys to press (Return, Tab, Escape, …)")
		click   = flag.Bool("click", false, "click the middle of the window first (webviews want a click before they take keys)")
		at      = flag.String("at", "", "click at these window-relative coordinates first, e.g. 240,143")
		hold    = flag.String("hold", "", "hold these keys down while the rest is typed, e.g. Control_L")
		wait    = flag.Duration("wait", 5*time.Second, "how long to wait for the window")
	)
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	xc, err := xconn.Dial(*display)
	if err != nil {
		fatal(err)
	}
	defer xc.Close()
	km, err := input.LoadKeymap(xc.X)
	if err != nil {
		fatal(err)
	}
	sw, sh := xc.ScreenSize()
	inj, err := input.NewInjector(xc.X, km, sw, sh, log)
	if err != nil {
		fatal(err)
	}

	win, err := waitForWindow(xc.X, xc.Root(), *window, *wait)
	if err != nil {
		fatal(err)
	}
	// Put the pointer over the window and take the keyboard focus:
	// without a window manager, neither happens on its own.
	if x, y, w, h, err := windowBox(xc.X, win); err == nil {
		inj.MovePointer(x+w/2, y+h/2)
		time.Sleep(100 * time.Millisecond)
	}
	if err := xproto.SetInputFocusChecked(xc.X, xproto.InputFocusPointerRoot, win,
		xproto.TimeCurrentTime).Check(); err != nil {
		log.Debug("keyboard focus not set (a click may do it instead)", "err", err)
	}
	if *click {
		inj.Button(1, true)
		inj.Button(1, false)
		time.Sleep(150 * time.Millisecond)
	}
	if *at != "" {
		var cx, cy int
		if _, err := fmt.Sscanf(*at, "%d,%d", &cx, &cy); err != nil {
			fatal(fmt.Errorf("-at wants X,Y: %w", err))
		}
		x, y, _, _, err := windowBox(xc.X, win)
		if err != nil {
			fatal(err)
		}
		inj.MovePointer(x+cx, y+cy)
		time.Sleep(100 * time.Millisecond)
		inj.Button(1, true)
		inj.Button(1, false)
		time.Sleep(250 * time.Millisecond)
	}

	if *keys != "" {
		for _, name := range strings.Split(*keys, ",") {
			name = strings.TrimSpace(name)
			ks, ok := special[name]
			if !ok && len(name) > 0 {
				// A single character is its own keysym: Ctrl+3 is
				// -key Control_L,3.
				ks, ok = xproto.Keysym([]rune(name)[0]), true
			}
			if !ok {
				fatal(fmt.Errorf("unknown key %q", name))
			}
			tap(inj, ks)
		}
	}
	for _, name := range strings.Split(*hold, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		ks, ok := special[name]
		if !ok {
			fatal(fmt.Errorf("unknown key %q", name))
		}
		inj.Key(ks, true)
		defer inj.Key(ks, false)
		time.Sleep(50 * time.Millisecond)
	}

	// The injector resolves keysyms to keycodes and holds Shift for the
	// ones that need it: an ASCII character is its own keysym.
	for _, r := range *text {
		tap(inj, xproto.Keysym(r))
	}
	log.Info("typed", "window", fmt.Sprintf("%#x", uint32(win)),
		"keys", *keys, "text", *text)
}

func tap(inj *input.Injector, ks xproto.Keysym) {
	inj.Key(ks, true)
	inj.Key(ks, false)
	time.Sleep(20 * time.Millisecond)
}

// waitForWindow waits for a window whose title starts with pattern.
func waitForWindow(x *xgb.Conn, root xproto.Window, pattern string, wait time.Duration) (xproto.Window, error) {
	deadline := time.Now().Add(wait)
	for {
		win, err := findWindow(x, root, pattern)
		if err == nil {
			return win, nil
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("no window titled %q*: %w", pattern, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// findWindow walks the tree for a window named pattern. An exact match
// wins over a prefix one: toolkits keep helper windows whose names start
// with the application's, and typing into a 10x10 helper achieves
// nothing.
func findWindow(x *xgb.Conn, parent xproto.Window, pattern string) (xproto.Window, error) {
	tree, err := xproto.QueryTree(x, parent).Reply()
	if err != nil {
		return 0, err
	}
	var prefix xproto.Window
	var problem error
	for _, w := range tree.Children {
		g, err := xproto.GetGeometry(x, xproto.Drawable(w)).Reply()
		if err != nil || g.Width < 50 || g.Height < 50 {
			continue // a helper window, not a place to type
		}
		name := windowName(x, w)
		switch {
		case name == pattern:
			return w, nil
		case strings.HasPrefix(name, pattern) && prefix == 0:
			prefix = w
		}
		if len(name) > 0 {
			problem = fmt.Errorf("saw %q", name)
		}
		if child, err := findWindow(x, w, pattern); err == nil {
			return child, nil
		}
	}
	if prefix != 0 {
		return prefix, nil
	}
	if problem == nil {
		problem = fmt.Errorf("no windows")
	}
	return 0, problem
}

// windowName is a window's title.
func windowName(x *xgb.Conn, w xproto.Window) string {
	r, err := xproto.GetProperty(x, false, w, xproto.AtomWmName, xproto.AtomString, 0, 64).Reply()
	if err != nil {
		return ""
	}
	return string(r.Value)
}

// windowBox is where a window sits on the screen.
func windowBox(x *xgb.Conn, w xproto.Window) (int, int, int, int, error) {
	g, err := xproto.GetGeometry(x, xproto.Drawable(w)).Reply()
	if err != nil {
		return 0, 0, 0, 0, err
	}
	t, err := xproto.TranslateCoordinates(x, w, g.Root, 0, 0).Reply()
	if err != nil {
		return 0, 0, 0, 0, err
	}
	return int(t.DstX), int(t.DstY), int(g.Width), int(g.Height), nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "testtype:", err)
	os.Exit(1)
}
