package viewer

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"sync"

	"github.com/jezek/xgb/shm"
	"github.com/jezek/xgb/xproto"

	"github.com/lepe/remmote/internal/input"
	"github.com/lepe/remmote/internal/xconn"
)

// EventListener receives local input, already mapped to host-screen
// coordinates and keysyms, plus viewer window size changes.
type EventListener interface {
	MouseMove(x, y int) // host screen coordinates
	Button(b uint8, down bool)
	Key(ks uint32, down bool)
	Resize(w, h int) // the viewer window's size, in the same coordinates
}

// Window is the client's viewer window on the local X display.
type Window struct {
	xc     *xconn.Conn
	win    xproto.Window
	gc     xproto.Gcontext
	km     *input.Keymap
	canvas *Canvas
	blit   *Blitter
	log    *slog.Logger

	// serverW/serverH are written by the network reader (ScreenResize /
	// keyframes) and read by the event pump (pointer mapping).
	sizeMu           sync.Mutex
	serverW, serverH int

	winW, winH int // our window size; touched only by the event pump

	// sentW/sentH is the last size reported to the listener — the size
	// the window was created with, so a window manager's initial
	// placement never resizes anything on the host. Event pump only.
	sentW, sentH int

	deleteAtom xproto.Atom
}

const viewerEventMask = xproto.EventMaskExposure | xproto.EventMaskStructureNotify |
	xproto.EventMaskPointerMotion | xproto.EventMaskButtonPress | xproto.EventMaskButtonRelease |
	xproto.EventMaskKeyPress | xproto.EventMaskKeyRelease

// Open creates and maps the viewer window, sized for a host screen of
// serverW×serverH but capped at 80% of the local screen.
func Open(display string, serverW, serverH int, title string, log *slog.Logger, fastScale ...bool) (*Window, error) {
	xc, err := xconn.Dial(display)
	if err != nil {
		return nil, err
	}
	km, err := input.LoadKeymap(xc.X)
	if err != nil {
		xc.Close()
		return nil, fmt.Errorf("viewer: keymap: %w", err)
	}

	w := &Window{
		xc:      xc,
		km:      km,
		serverW: max(1, serverW),
		serverH: max(1, serverH),
		log:     log,
	}

	// Initial size: host screen, shrunk to ≤80% of the local screen.
	sw, sh := xc.ScreenSize()
	w.winW = min(w.serverW, int(sw)*8/10)
	w.winH = min(w.serverH, int(sh)*8/10)
	w.winW, w.winH = max(w.winW, 200), max(w.winH, 150)
	// Already "reported": a size change must be the user's doing, not a
	// window manager's first placement of the window it was born with.
	w.sentW, w.sentH = w.winW, w.winH

	wid, err := xc.X.NewId()
	if err != nil {
		xc.Close()
		return nil, err
	}
	w.win = xproto.Window(wid)
	// Event mask at creation — before MapWindow — so nothing is missed.
	// Visual 0 = CopyFromParent. Value list ordered by ascending mask bit:
	// BackPixel (bit 1) before EventMask (bit 11).
	if err := xproto.CreateWindowChecked(xc.X, xc.Depth(), w.win, xc.Root(),
		0, 0, uint16(w.winW), uint16(w.winH), 0,
		xproto.WindowClassInputOutput, 0,
		xproto.CwEventMask|xproto.CwBackPixel,
		[]uint32{0x00000000, viewerEventMask}).Check(); err != nil {
		xc.Close()
		return nil, fmt.Errorf("viewer: CreateWindow: %w", err)
	}

	if err := w.setWMProperties(title); err != nil {
		log.Warn("window manager properties failed (continuing)", "err", err)
	}

	gcID, err := xc.X.NewId()
	if err != nil {
		xc.Close()
		return nil, err
	}
	w.gc = xproto.Gcontext(gcID)
	if err := xproto.CreateGCChecked(xc.X, w.gc, xproto.Drawable(w.win), 0, nil).Check(); err != nil {
		xc.Close()
		return nil, fmt.Errorf("viewer: CreateGC: %w", err)
	}

	w.canvas = NewCanvas(w.serverW, w.serverH)
	w.blit = newBlitter(xc, w.win, w.gc, w.canvas, log)
	if len(fastScale) > 0 {
		w.blit.fastScale = fastScale[0]
	}
	w.blit.Resize(w.winW, w.winH)
	if err := xproto.MapWindowChecked(xc.X, w.win).Check(); err != nil {
		xc.Close()
		return nil, fmt.Errorf("viewer: MapWindow: %w", err)
	}
	w.blit.Start()
	return w, nil
}

func (w *Window) setWMProperties(title string) error {
	protocols, err := xproto.InternAtom(w.xc.X, false, uint16(len("WM_PROTOCOLS")), "WM_PROTOCOLS").Reply()
	if err != nil {
		return err
	}
	del, err := xproto.InternAtom(w.xc.X, false, uint16(len("WM_DELETE_WINDOW")), "WM_DELETE_WINDOW").Reply()
	if err != nil {
		return err
	}
	w.deleteAtom = del.Atom

	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], uint32(del.Atom))
	if err := xproto.ChangePropertyChecked(w.xc.X, xproto.PropModeReplace, w.win,
		protocols.Atom, xproto.AtomAtom, 32, 1, buf[:]).Check(); err != nil {
		return err
	}
	name := []byte(title)
	return xproto.ChangePropertyChecked(w.xc.X, xproto.PropModeReplace, w.win,
		xproto.AtomWmName, xproto.AtomString, 8, uint32(len(name)), name).Check()
}

// Canvas exposes the composited framebuffer.
func (w *Window) Canvas() *Canvas { return w.canvas }

// Dirty requests a redraw (after canvas compositing).
func (w *Window) Dirty() { w.blit.Dirty() }

// UpdateServerSize adapts to a host screen resize. The canvas resize
// changes the fit rect, so letterbox bars and the whole window must be
// repainted — not just the new canvas's dirty region.
func (w *Window) UpdateServerSize(sw, sh int) {
	w.sizeMu.Lock()
	sw, sh = max(1, sw), max(1, sh)
	if sw == w.serverW && sh == w.serverH {
		w.sizeMu.Unlock()
		return
	}
	w.serverW, w.serverH = sw, sh
	w.sizeMu.Unlock()
	w.canvas.Resize(sw, sh)
	w.blit.FullRedraw()
}

// Close tears the window and the local X connection down.
func (w *Window) Close() {
	w.blit.Stop()
	w.xc.Close()
}

// Pump runs the X event loop until the window is closed or the
// connection dies. It is the only WaitForEvent caller on this
// connection. Returns nil when the user closed the window. xgb delivers
// events as values, so every case matches the value type.
func (w *Window) Pump(l EventListener) error {
	for {
		ev, err := w.xc.X.WaitForEvent()
		if err != nil {
			return fmt.Errorf("viewer: X connection lost: %w", err)
		}
		if ev == nil && err == nil {
			return fmt.Errorf("viewer: X connection closed")
		}
		switch e := ev.(type) {
		case xproto.ExposeEvent:
			// The X server repainted over us: the whole window needs
			// pushing again, not just the canvas's dirty region.
			w.blit.FullRedraw()

		case xproto.ConfigureNotifyEvent:
			w.winW, w.winH = int(e.Width), int(e.Height)
			w.blit.RequestResize(w.winW, w.winH)
			w.reportResize(l)

		case xproto.MotionNotifyEvent:
			if l != nil {
				if sx, sy, ok := w.mapPointer(int(e.EventX), int(e.EventY)); ok {
					l.MouseMove(sx, sy)
				}
			}

		case xproto.ButtonPressEvent:
			if l != nil {
				if sx, sy, ok := w.mapPointer(int(e.EventX), int(e.EventY)); ok {
					l.MouseMove(sx, sy)
				}
				l.Button(uint8(e.Detail), true)
			}

		case xproto.ButtonReleaseEvent:
			if l != nil {
				l.Button(uint8(e.Detail), false)
			}

		case xproto.KeyPressEvent:
			if l != nil {
				if ks := w.keysym(e.Detail, e.State); ks != 0 {
					l.Key(uint32(ks), true)
				}
			}

		case xproto.KeyReleaseEvent:
			if l != nil {
				if ks := w.keysym(e.Detail, e.State); ks != 0 {
					l.Key(uint32(ks), false)
				}
			}

		case shm.CompletionEvent:
			w.blit.Complete(e.Shmseg)

		case xproto.ClientMessageEvent:
			if e.Type == w.deleteAtom {
				return nil // user closed the window
			}

		case xproto.DestroyNotifyEvent:
			return nil
		}
	}
}

// reportResize tells the listener the window changed size — once per
// size, so a ConfigureNotify storm during an interactive drag cannot
// turn into a stream of host-side resize requests (the listener
// debounces on top of this). Deduplication is against the size last
// reported, which Open seeds with the window's created size: the first
// ConfigureNotify a window manager emits is usually a pure placement,
// and following it would resize the host without the user having
// touched anything. Event pump only.
func (w *Window) reportResize(l EventListener) {
	if l == nil || w.winW <= 0 || w.winH <= 0 {
		return
	}
	if w.winW == w.sentW && w.winH == w.sentH {
		return
	}
	w.sentW, w.sentH = w.winW, w.winH
	l.Resize(w.winW, w.winH)
}

// mapPointer converts window coordinates to host screen coordinates
// through the letterbox fit. ok=false when the pointer is on a bar.
func (w *Window) mapPointer(winX, winY int) (int, int, bool) {
	w.sizeMu.Lock()
	serverW, serverH := w.serverW, w.serverH
	w.sizeMu.Unlock()
	fit := FitRect(w.winW, w.winH, serverW, serverH)
	if fit.Empty() {
		return 0, 0, false
	}
	if winX < fit.Min.X || winX >= fit.Max.X || winY < fit.Min.Y || winY >= fit.Max.Y {
		return 0, 0, false
	}
	sx := (winX - fit.Min.X) * serverW / fit.Dx()
	sy := (winY - fit.Min.Y) * serverH / fit.Dy()
	return clampInt(sx, 0, serverW-1), clampInt(sy, 0, serverH-1), true
}

// keysym resolves a keycode+modifier-state to a keysym: column 1 when
// Shift is held, column 2 for AltGr (Mod5), falling back to column 0.
func (w *Window) keysym(kc xproto.Keycode, state uint16) xproto.Keysym {
	col := 0
	if state&xproto.ModMaskShift != 0 {
		col = 1
	} else if state&xproto.ModMask5 != 0 {
		col = 2
	}
	ks := w.km.KeysymAt(kc, col)
	if ks == 0 && col != 0 {
		ks = w.km.KeysymAt(kc, 0)
	}
	return ks
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
