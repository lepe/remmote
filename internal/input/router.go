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
// windows, and lifts + focuses the target before injecting so XTest
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
	lastWin      xproto.Window                   // focus target from the last press
	raisedAt     time.Time                       // raise dampening
	rootOf       map[xproto.Window]xproto.Window // → root-level ancestor, per window

	// Injectable for tests; defaults hit the real X requests.
	raiseFn func(w xproto.Window) error
	focusFn func(w xproto.Window) error
	rootFn  func(w xproto.Window) xproto.Window
	topFn   func() xproto.Window
}

// How long a click waits for the window manager to stack the shared
// window over its neighbours. Long enough for a loaded compositor to get
// to the request, short enough that a click never feels dead.
const (
	liftTries = 25
	liftPoll  = 2 * time.Millisecond
)

// NewRouter wraps an Injector. mmap may be nil for root mode.
func NewRouter(inj *Injector, x *xgb.Conn, mmap WindowMap, log *slog.Logger) *Router {
	root := xproto.Setup(x).DefaultScreen(x).Root
	r := &Router{inj: inj, x: x, mmap: mmap, log: log,
		rootOf: make(map[xproto.Window]xproto.Window)}
	r.raiseFn = func(w xproto.Window) error {
		xproto.ConfigureWindow(x, w, xproto.ConfigWindowStackMode, []uint32{xproto.StackModeAbove})
		return nil
	}
	r.focusFn = func(w xproto.Window) error {
		xproto.SetInputFocus(x, revertToParent, w, xproto.TimeCurrentTime)
		return nil
	}
	r.rootFn = func(w xproto.Window) xproto.Window {
		// Bounded walk: reparenting stops at the root, and a loop that
		// never does is a bug worth failing to notice.
		for i := 0; i < 8; i++ {
			q, err := xproto.QueryTree(x, w).Reply()
			if err != nil || q.Parent == 0 || q.Parent == root {
				return w
			}
			w = q.Parent
		}
		return w
	}
	r.topFn = func() xproto.Window {
		q, err := xproto.QueryPointer(x, root).Reply()
		if err != nil {
			return 0
		}
		return q.Child
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
// lift it above whatever is stacked over it and focus it first, so the
// injected event cannot land on a foreign window. Releases always pass
// through.
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
	r.liftTo(w) // the click must land on w, whatever is stacked over it
	r.focusWindow(w)
	r.mu.Unlock()
	r.inj.Button(b, true)
}

// Wheel scrolls; the same lift+focus gating as button presses applies.
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
	r.liftTo(w) // scrolling must reach w too, whatever is stacked over it
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

// rootAncestor reports the window's root-level ancestor — the unit X
// stacks windows in. A window manager reparents every client into a frame
// of its own, so raising the client raises it inside that frame: no other
// window moves, and the XTest click still lands on whatever is stacked
// above. Asked of X once per window and then remembered — reparenting
// happens when a window is mapped, long before it is ever shared.
// Caller holds r.mu.
func (r *Router) rootAncestor(w xproto.Window) xproto.Window {
	if top, ok := r.rootOf[w]; ok {
		return top
	}
	top := r.rootFn(w)
	r.rootOf[w] = top
	return top
}

// underPointer reports the root-level window stacked over the host
// pointer — the one an XTest click is delivered to. Caller holds r.mu.
func (r *Router) underPointer() xproto.Window {
	return r.topFn()
}

// liftTo raises the target's root-level ancestor over whatever else is
// stacked at the pointer and waits, briefly, for the window manager to
// get to it. A restack of a top-level window is a *request* to the window
// manager — X redirects it — so the raise lands some time after it is
// issued, and injecting the click first gives it to whatever is still
// stacked above: the viewer window itself, when the shared session is
// watched on the host's own screen. The wait is bounded, so a window
// manager that never agrees cannot make the click feel dead. Caller
// holds r.mu.
func (r *Router) liftTo(w xproto.Window) {
	top := r.rootAncestor(w)
	if r.underPointer() == top {
		return // already the window the click would land on
	}
	if err := r.raiseFn(top); err != nil {
		r.log.Debug("raise failed", "window", uint32(top), "err", err)
	} else {
		r.mmap.MarkStackDirty()
		r.raisedAt = time.Now()
	}
	for i := 0; i < liftTries; i++ {
		if r.underPointer() == top {
			return
		}
		time.Sleep(liftPoll)
	}
	r.log.Debug("the window manager did not raise the shared window; this click may land on another window",
		"window", uint32(top))
}

// focusWindow raises (rate-limited) and focuses the target. Requests are
// ordered on the one connection, so the subsequent FakeInput lands after
// the raise/focus. Caller holds r.mu.
func (r *Router) focusWindow(w xproto.Window) {
	if w != r.lastWin || time.Since(r.raisedAt) > 500*time.Millisecond {
		top := r.rootAncestor(w)
		if err := r.raiseFn(top); err != nil {
			r.log.Debug("raise failed", "window", uint32(top), "err", err)
		} else {
			r.mmap.MarkStackDirty()
			r.raisedAt = time.Now()
		}
	}
	// Focus goes to the client itself: the root-level ancestor is where
	// stacking happens, the client is where keystrokes are delivered.
	if err := r.focusFn(w); err != nil {
		// BadMatch when the window is mid-unmap; the keystroke may miss.
		r.log.Debug("focus failed", "window", uint32(w), "err", err)
	}
	r.lastWin = w
}
