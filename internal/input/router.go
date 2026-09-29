package input

import (
	"image"
	"log/slog"
	"sync"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// xgb v1.3.1 ships no RevertTo* constants; X protocol: 0=None,1=PointerRoot,2=Parent.
const revertToParent = 2

// WindowMap is the structural view of the shared window set, implemented
// by the capture side (Scene). Coordinates are root/screen coordinates.
type WindowMap interface {
	// WindowAt returns the topmost tracked window containing the point.
	WindowAt(x, y int) (xproto.Window, bool)
	// CanvasOrigin translates canvas coordinates to root coordinates.
	CanvasOrigin() image.Point
	// TopWindow returns the topmost tracked window (initial key focus).
	TopWindow() (xproto.Window, bool)
	// MarkStackDirty hints that the stacking snapshot is stale.
	MarkStackDirty()
}

// Router fronts the Injector for window-sharing mode: it maps canvas
// coordinates to the screen, hit-tests clicks against the tracked
// windows, and raises + focuses the target before injecting so XTest
// events land on the right window. With a nil WindowMap it is a pure
// passthrough to the Injector (root mode).
type Router struct {
	inj  *Injector
	x    *xgb.Conn
	mmap WindowMap
	log  *slog.Logger

	mu           sync.Mutex
	lastX, lastY int // last canvas position (for click hit-tests)
	havePos      bool
	lastWin      xproto.Window // focus target from the last press
	raisedAt     time.Time     // raise dampening

	// Injectable for tests; defaults hit the real X requests.
	raiseFn func(w xproto.Window) error
	focusFn func(w xproto.Window) error
}

// NewRouter wraps an Injector. mmap may be nil for root mode.
func NewRouter(inj *Injector, x *xgb.Conn, mmap WindowMap, log *slog.Logger) *Router {
	r := &Router{inj: inj, x: x, mmap: mmap, log: log}
	r.raiseFn = func(w xproto.Window) error {
		xproto.ConfigureWindow(x, w, xproto.ConfigWindowStackMode, []uint32{xproto.StackModeAbove})
		return nil
	}
	r.focusFn = func(w xproto.Window) error {
		xproto.SetInputFocus(x, revertToParent, w, xproto.TimeCurrentTime)
		return nil
	}
	return r
}

// MovePointer positions the host pointer; input is in canvas
// coordinates (identical to screen coordinates in root mode).
func (r *Router) MovePointer(x, y int) {
	r.mu.Lock()
	r.lastX, r.lastY = x, y
	r.havePos = true
	if r.mmap != nil {
		o := r.mmap.CanvasOrigin()
		x += o.X
		y += o.Y
	}
	r.mu.Unlock()
	r.inj.MovePointer(x, y)
}

// Button presses or releases a pointer button. In window mode, presses
// on gaps between windows are swallowed; presses on a tracked window
// raise + focus it first so the injected event cannot land on a foreign
// window stacked above. Releases always pass through.
func (r *Router) Button(b uint8, down bool) {
	if r.mmap == nil {
		r.inj.Button(b, down)
		return
	}
	if !down {
		r.inj.Button(b, false)
		return
	}
	r.mu.Lock()
	if !r.havePos {
		r.mu.Unlock()
		return // never saw a position: cannot hit-test
	}
	cx, cy := r.lastX, r.lastY
	w, ok := r.mmap.WindowAt(cx+r.mmap.CanvasOrigin().X, cy+r.mmap.CanvasOrigin().Y)
	if !ok {
		r.mu.Unlock()
		return // gap: swallow
	}
	r.focusWindow(w)
	r.mu.Unlock()
	r.inj.Button(b, true)
}

// Wheel scrolls; the same raise+focus gating as button presses applies.
func (r *Router) Wheel(dx, dy int) {
	if r.mmap == nil || (dx == 0 && dy == 0) {
		r.inj.Wheel(dx, dy)
		return
	}
	r.mu.Lock()
	if !r.havePos {
		r.mu.Unlock()
		return
	}
	w, ok := r.mmap.WindowAt(r.lastX+r.mmap.CanvasOrigin().X, r.lastY+r.mmap.CanvasOrigin().Y)
	if !ok {
		r.mu.Unlock()
		return
	}
	r.focusWindow(w)
	r.mu.Unlock()
	r.inj.Wheel(dx, dy)
}

// Key presses or releases a keysym. In window mode the keystroke goes to
// the focused tracked window (the last pressed, else the topmost).
func (r *Router) Key(ks xproto.Keysym, down bool) {
	if r.mmap == nil || !down {
		r.inj.Key(ks, down)
		return
	}
	r.mu.Lock()
	w, ok := r.lastWin, r.lastWin != 0
	if !ok {
		w, ok = r.mmap.TopWindow()
	}
	if ok {
		r.focusWindow(w)
	}
	r.mu.Unlock()
	r.inj.Key(ks, true)
}

// focusWindow raises (rate-limited) and focuses the target. Requests are
// ordered on the one connection, so the subsequent FakeInput lands after
// the raise/focus. Caller holds r.mu.
func (r *Router) focusWindow(w xproto.Window) {
	if w != r.lastWin || time.Since(r.raisedAt) > 500*time.Millisecond {
		if err := r.raiseFn(w); err != nil {
			r.log.Debug("raise failed", "window", uint32(w), "err", err)
		} else {
			r.mmap.MarkStackDirty()
			r.raisedAt = time.Now()
		}
	}
	if err := r.focusFn(w); err != nil {
		// BadMatch when the window is mid-unmap; the keystroke may miss.
		r.log.Debug("focus failed", "window", uint32(w), "err", err)
	}
	r.lastWin = w
}
