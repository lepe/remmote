// testfill is a development and integration-test helper. It either paints
// a fullscreen solid-color window on a display (-color) so remmote has
// deterministic content to capture, or verifies a snapshot PNG against an
// expected color (-check).
package main

import (
	"context"
	"flag"
	"fmt"
	"image"
	"image/color"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	// Registers the PNG decoder for -check's image.Decode.
	_ "image/png"

	"github.com/jezek/xgb/xproto"

	"github.com/lepe/remmote/internal/clipboard"
	"github.com/lepe/remmote/internal/xconn"
)

func main() {
	var (
		display       = flag.String("display", os.Getenv("DISPLAY"), "X display to paint")
		colStr        = flag.String("color", "#20c040", "fill color when painting")
		hold          = flag.Duration("hold", 30*time.Second, "how long to keep the window up")
		check         = flag.String("check", "", "verify this PNG instead of painting")
		expect        = flag.String("expect", "#20c040", "expected color for -check")
		minPct        = flag.Int("min-match", 90, "minimum percentage of matching pixels")
		expectRegions = flag.String("expect-region", "", "with -check: comma list WxH+X+Y:#rrggbb")
		clipSet       = flag.String("clip-set", "", "act as a user copying this text (hold the selection)")
		clipWatch     = flag.Bool("clip-watch", false, "print clipboard changes as CLIP:<text> lines")
		size          = flag.String("size", "", "windowed mode: size WxH (default fullscreen OR)")
		pos           = flag.String("pos", "0+0", "windowed mode: position X+Y")
		wmClass       = flag.String("wm-class", "remmote-testfill:RemmoteTestfill", "windowed mode: WM_CLASS INST:CLASS (\"\" = none)")
		windows       = flag.String("windows", "", "paint several windows: comma list #rrggbb@WxH+X+Y")
	)
	flag.Parse()

	switch {
	case *check != "":
		if err := checkPNG(*check, *expect, *minPct); err != nil {
			fmt.Fprintln(os.Stderr, "FAIL:", err)
			os.Exit(1)
		}
		fmt.Printf("OK: %s matches %s (≥%d%%)\n", *check, *expect, *minPct)
		if *expectRegions != "" {
			if err := checkRegions(*check, *expectRegions); err != nil {
				fmt.Fprintln(os.Stderr, "FAIL:", err)
				os.Exit(1)
			}
			fmt.Println("OK: regions match")
		}
		return

	case *clipSet != "":
		if err := clipSetRun(*display, *clipSet); err != nil {
			fmt.Fprintln(os.Stderr, "FAIL:", err)
			os.Exit(1)
		}

	case *clipWatch:
		if err := clipWatchRun(*display); err != nil {
			fmt.Fprintln(os.Stderr, "FAIL:", err)
			os.Exit(1)
		}

	case *windows != "":
		if err := paintWindows(*display, *windows, *wmClass, *hold); err != nil {
			fmt.Fprintln(os.Stderr, "FAIL:", err)
			os.Exit(1)
		}

	default:
		if err := paintOne(*display, *colStr, *size, *pos, *wmClass, *hold); err != nil {
			fmt.Fprintln(os.Stderr, "FAIL:", err)
			os.Exit(1)
		}
	}
}

// clipSetRun installs text as the display's clipboard and serves it,
// behaving exactly like a user who copied something. Blocks until
// killed.
func clipSetRun(display, text string) error {
	w, err := clipboard.New(display, 0, discardLogger())
	if err != nil {
		return err
	}
	defer w.Close()
	w.SetRemote(text)
	fmt.Println("clip-set: serving", strconv.Quote(text))
	ctx := context.Background()
	onLocal := func(string) {} // our own text must not bounce anywhere
	w.Run(ctx, onLocal)
	return nil
}

// clipWatchRun prints every clipboard change it observes.
func clipWatchRun(display string) error {
	w, err := clipboard.New(display, 0, discardLogger())
	if err != nil {
		return err
	}
	defer w.Close()
	fmt.Println("clip-watch: watching")
	ctx := context.Background()
	w.Run(ctx, func(text string) {
		fmt.Println("CLIP:" + text)
	})
	return nil
}

func discardLogger() *slog.Logger {
	if os.Getenv("REMMOTE_CLIP_DEBUG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func parseColor(s string) (color.RGBA, error) {
	if len(s) == 7 && s[0] == '#' {
		r, err1 := strconv.ParseUint(s[1:3], 16, 8)
		g, err2 := strconv.ParseUint(s[3:5], 16, 8)
		b, err3 := strconv.ParseUint(s[5:7], 16, 8)
		if err1 == nil && err2 == nil && err3 == nil {
			return color.RGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: 0xFF}, nil
		}
	}
	return color.RGBA{}, fmt.Errorf("bad color %q (want #rrggbb)", s)
}

// paintOne paints one colored window (fullscreen OR by default, or a
// managed window with -size/-pos/-wm-class).
func paintOne(display, colStr, size, pos, wmClass string, hold time.Duration) error {
	xc, err := xconn.Dial(display)
	if err != nil {
		return err
	}
	defer xc.Close()

	c, err := parseColor(colStr)
	if err != nil {
		return err
	}
	sw, sh := xc.ScreenSize()

	// Windowed mode: a normal managed window with WM_CLASS and WM_STATE
	// at the requested geometry. Legacy mode: fullscreen override-redirect
	// (backwards compatible with the root-mode integration test).
	windowed := size != "" || wmClass != ""
	x, y, w, h := 0, 0, int(sw), int(sh)
	if windowed {
		var err error
		if size != "" {
			if w, h, err = parseSize(size); err != nil {
				return err
			}
		}
		if x, y, err = parsePos(pos); err != nil {
			return err
		}
	}
	win, err := createWindow(xc, c, x, y, w, h, !windowed, wmClass)
	if err != nil {
		return err
	}
	fmt.Printf("WIN:0x%x\n", uint32(win))
	fmt.Printf("testfill: %s painted %dx%d+%d+%d for %s\n", colStr, w, h, x, y, hold)
	time.Sleep(hold)
	return nil
}

// paintWindows paints several colored windows sharing one WM_CLASS.
func paintWindows(display, spec, wmClass string, hold time.Duration) error {
	xc, err := xconn.Dial(display)
	if err != nil {
		return err
	}
	defer xc.Close()
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// #rrggbb@WxH+X+Y
		at := strings.IndexByte(part, '@')
		if at < 0 || len(part) < 8 {
			return fmt.Errorf("bad window spec %q (want #rrggbb@WxH+X+Y)", part)
		}
		c, err := parseColor(part[:at])
		if err != nil {
			return err
		}
		var w, h, x, y int
		if n, err := fmt.Sscanf(part[at+1:], "%dx%d+%d+%d", &w, &h, &x, &y); err != nil || n != 4 {
			return fmt.Errorf("bad geometry in %q", part)
		}
		win, err := createWindow(xc, c, x, y, w, h, false, wmClass)
		if err != nil {
			return err
		}
		fmt.Printf("WIN:0x%x\n", uint32(win))
	}
	fmt.Printf("testfill: painted windows for %s\n", hold)
	time.Sleep(hold)
	return nil
}

// createWindow maps one colored window. overrideRedirect keeps the
// legacy fullscreen behavior; windowed windows get WM_CLASS + WM_STATE
// so they look like real client windows to remmote's window detection.
func createWindow(xc *xconn.Conn, c color.RGBA, x, y, w, h int, overrideRedirect bool, wmClass string) (xproto.Window, error) {
	// Root pixel value for a TrueColor depth-24 visual: R<<16|G<<8|B.
	pixel := uint32(c.R)<<16 | uint32(c.G)<<8 | uint32(c.B)

	wid, err := xc.X.NewId()
	if err != nil {
		return 0, err
	}
	win := xproto.Window(wid)
	mask := uint32(xproto.CwBackPixel)
	values := []uint32{pixel}
	if overrideRedirect {
		mask |= xproto.CwOverrideRedirect
		values = append(values, 1)
	}
	if err := xproto.CreateWindowChecked(xc.X, xc.Depth(), win, xc.Root(),
		int16(x), int16(y), uint16(w), uint16(h), 0,
		xproto.WindowClassInputOutput, 0, mask, values).Check(); err != nil {
		return 0, err
	}
	name := []byte("remmote-testfill")
	_ = xproto.ChangePropertyChecked(xc.X, xproto.PropModeReplace, win,
		xproto.AtomWmName, xproto.AtomString, 8, uint32(len(name)), name).Check()
	if wmClass != "" {
		inst, class, ok := strings.Cut(wmClass, ":")
		if !ok {
			inst, class = wmClass, wmClass
		}
		data := append(append([]byte(inst), 0), append([]byte(class), 0)...)
		_ = xproto.ChangePropertyChecked(xc.X, xproto.PropModeReplace, win,
			xproto.AtomWmClass, xproto.AtomString, 8, uint32(len(data)), data).Check()
	}
	if !overrideRedirect {
		// WM_STATE = [Normal(1), icon=None] as LE u32s — the EWMH mark of
		// a managed client window (no WM on Xvfb would ever set it).
		r, err := xproto.InternAtom(xc.X, false, 8, "WM_STATE").Reply()
		if err == nil {
			state := []byte{1, 0, 0, 0, 0, 0, 0, 0}
			_ = xproto.ChangePropertyChecked(xc.X, xproto.PropModeReplace, win,
				r.Atom, r.Atom, 32, 2, state).Check()
		}
	}
	if err := xproto.MapWindowChecked(xc.X, win).Check(); err != nil {
		return 0, err
	}
	_ = xproto.ConfigureWindowChecked(xc.X, win, xproto.ConfigWindowStackMode,
		[]uint32{xproto.StackModeAbove}).Check()
	return win, nil
}

func parseSize(s string) (w, h int, err error) {
	if n, e := fmt.Sscanf(s, "%dx%d", &w, &h); e != nil || n != 2 || w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("bad size %q (want WxH)", s)
	}
	return w, h, nil
}

func parsePos(s string) (x, y int, err error) {
	if n, e := fmt.Sscanf(s, "%d+%d", &x, &y); e != nil || n != 2 || x < 0 || y < 0 {
		return 0, 0, fmt.Errorf("bad position %q (want X+Y)", s)
	}
	return x, y, nil
}

// checkRegions verifies colored regions of a snapshot:
// "WxH+X+Y:#rrggbb,..." with the same tolerance as checkPNG.
func checkRegions(path, spec string) error {
	img, _, err := openPNG(path)
	if err != nil {
		return err
	}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var w, h, x, y int
		var colStr string
		if n, e := fmt.Sscanf(part, "%dx%d+%d+%d:%s", &w, &h, &x, &y, &colStr); e != nil || n != 5 {
			return fmt.Errorf("bad region %q (want WxH+X+Y:#rrggbb)", part)
		}
		want, err := parseColor(colStr)
		if err != nil {
			return err
		}
		const tol = 24
		match, total := 0, 0
		for py := y; py < y+h && py < img.Bounds().Max.Y; py++ {
			for px := x; px < x+w && px < img.Bounds().Max.X; px++ {
				r, g, b, _ := img.At(px, py).RGBA()
				total++
				if abs8(uint8(r>>8), want.R) <= tol &&
					abs8(uint8(g>>8), want.G) <= tol &&
					abs8(uint8(b>>8), want.B) <= tol {
					match++
				}
			}
		}
		if total == 0 || match*100/total < 90 {
			return fmt.Errorf("region %s: only %d/%d pixels match", part, match, total)
		}
	}
	return nil
}

func openPNG(path string) (image.Image, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	return image.Decode(f)
}

// checkPNG verifies that at least minPct% of the pixels are within a
// small tolerance of the expected color.
func checkPNG(path, expectStr string, minPct int) error {
	want, err := parseColor(expectStr)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return err
	}
	b := img.Bounds()
	total, match := 0, 0
	const tol = 24
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			total++
			if abs8(uint8(r>>8), want.R) <= tol &&
				abs8(uint8(g>>8), want.G) <= tol &&
				abs8(uint8(bl>>8), want.B) <= tol {
				match++
			}
		}
	}
	if total == 0 {
		return fmt.Errorf("empty image")
	}
	pct := match * 100 / total
	if pct < minPct {
		return fmt.Errorf("only %d%% of pixels match %s (want ≥%d%%)", pct, expectStr, minPct)
	}
	return nil
}

func abs8(a, b uint8) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}
