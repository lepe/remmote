package input

import (
	"image"
	"sync"
	"testing"
	"time"

	"github.com/jezek/xgb/xproto"
)

// fakeMap is a scripted WindowMap.
type fakeMap struct {
	origin  image.Point
	wins    []winEntry // topmost first
	top     xproto.Window
	haveTop bool
	dirty   int
}

type winEntry struct {
	id   xproto.Window
	rect image.Rectangle
}

func (m *fakeMap) WindowAt(x, y int) (xproto.Window, bool) {
	for _, w := range m.wins {
		if image.Pt(x, y).In(w.rect) {
			return w.id, true
		}
	}
	return 0, false
}
func (m *fakeMap) CanvasOrigin() image.Point        { return m.origin }
func (m *fakeMap) TopWindow() (xproto.Window, bool) { return m.top, m.haveTop }
func (m *fakeMap) MarkStackDirty()                  { m.dirty++ }

// fakeFocus records raise/focus calls, and stands in for the window
// manager's stacking: under is the root-level window the pointer is over.
type fakeFocus struct {
	raise []xproto.Window
	focus []xproto.Window

	mu    sync.Mutex
	under xproto.Window
}

func (f *fakeFocus) setUnder(w xproto.Window) { f.mu.Lock(); f.under = w; f.mu.Unlock() }
func (f *fakeFocus) getUnder() xproto.Window  { f.mu.Lock(); defer f.mu.Unlock(); return f.under }

func newRouterForTest(t *testing.T, m WindowMap) (*Router, *recorder, *fakeFocus) {
	t.Helper()
	rec := &recorder{}
	ff := &fakeFocus{}
	inj := &Injector{
		km: testKeymap(), sw: 1000, sh: 1000, log: testLogger(),
		held: map[xproto.Keysym]xproto.Keycode{},
		fake: rec.record,
	}
	r := &Router{inj: inj, x: nil, mmap: m, log: testLogger(),
		rootOf: map[xproto.Window]xproto.Window{}}
	r.raiseFn = func(w xproto.Window) error { ff.raise = append(ff.raise, w); return nil }
	r.focusFn = func(w xproto.Window) error { ff.focus = append(ff.focus, w); return nil }
	r.rootFn = func(w xproto.Window) xproto.Window { return w } // unmanaged: its own root ancestor
	r.topFn = ff.getUnder
	if fm, ok := m.(*fakeMap); ok && fm.haveTop {
		ff.setUnder(fm.top) // and stacked under the pointer, as it would be
	}
	return r, rec, ff
}

func TestRouterRootPassthrough(t *testing.T) {
	r, rec, _ := newRouterForTest(t, nil)
	r.MovePointer(10, 20)
	r.Button(1, true)
	r.Button(1, false)
	r.Key(0x61, true)
	r.Wheel(0, -1)
	assertCalls(t, rec.calls, []fakeCall{
		{evMotionNotify, 0, 10, 20},
		{evButtonPress, 1, 0, 0},
		{evButtonRelease, 1, 0, 0},
		{evKeyPress, 8, 0, 0},
		{evButtonPress, 4, 0, 0},
		{evButtonRelease, 4, 0, 0},
	})
}

func TestRouterWindowMode(t *testing.T) {
	m := &fakeMap{
		origin: image.Pt(100, 50),
		wins: []winEntry{ // topmost first
			{id: 7, rect: image.Rect(300, 100, 500, 300)},
			{id: 5, rect: image.Rect(100, 50, 900, 600)},
		},
		top: 7, haveTop: true,
	}
	r, rec, ff := newRouterForTest(t, m)

	// Motion inside window 7 (canvas (250,80) → root (350,130)).
	r.MovePointer(250, 80)
	// Click: hit-test root (350,130) → window 7 → raise+focus+press.
	r.Button(1, true)
	r.Button(1, false)
	// Second press on the same window within the dampening window: no
	// second raise, but focus is re-asserted.
	r.Button(1, true)

	if len(ff.raise) != 1 || ff.raise[0] != 7 {
		t.Fatalf("raises = %v, want one raise of window 7", ff.raise)
	}
	if len(ff.focus) < 2 || ff.focus[0] != 7 {
		t.Fatalf("focuses = %v, want window 7 focused on each press", ff.focus)
	}
	assertCalls(t, []fakeCall{
		rec.calls[0], // motion
		rec.calls[1], // button press
	}, []fakeCall{
		{evMotionNotify, 0, 350, 130},
		{evButtonPress, 1, 0, 0},
	})

	// Keystroke without prior position context is focused at TopWindow.
	r2, rec2, ff2 := newRouterForTest(t, m)
	r2.Key(0x61, true)
	if len(ff2.focus) != 1 || ff2.focus[0] != 7 {
		t.Fatalf("key without click focused %v, want top window 7", ff2.focus)
	}
	assertCalls(t, rec2.calls[:1], []fakeCall{{evKeyPress, 8, 0, 0}})
}

func TestRouterGapSwallowed(t *testing.T) {
	m := &fakeMap{origin: image.Pt(0, 0),
		wins: []winEntry{{id: 5, rect: image.Rect(0, 0, 100, 100)}}}
	r, rec, ff := newRouterForTest(t, m)
	r.MovePointer(500, 500) // gap
	r.Button(1, true)
	if len(ff.raise)+len(ff.focus) != 0 {
		t.Fatal("gap click raised/focused a window")
	}
	assertCalls(t, rec.calls, []fakeCall{{evMotionNotify, 0, 500, 500}})
}

func TestRouterNoPositionSwallowsClick(t *testing.T) {
	m := &fakeMap{wins: []winEntry{{id: 5, rect: image.Rect(0, 0, 900, 900)}}}
	r, rec, _ := newRouterForTest(t, m)
	r.Button(1, true)
	assertCalls(t, rec.calls, nil)
}

func TestRouterReleaseWithoutPressPasses(t *testing.T) {
	m := &fakeMap{}
	r, rec, _ := newRouterForTest(t, m)
	r.Button(1, false) // release: passthrough even with no history
	assertCalls(t, rec.calls, []fakeCall{{evButtonRelease, 1, 0, 0}})
}

// With a window manager every client is reparented into a frame, and X
// stacks frames: raising the client raises it inside its own frame and
// moves nothing, so the XTest click lands on whatever window is stacked
// above. The raise goes to the root-level ancestor; the focus stays with
// the client, which is where keystrokes are delivered.
func TestRouterRaisesTheFrameAndFocusesTheClient(t *testing.T) {
	m := &fakeMap{origin: image.Pt(0, 0),
		wins: []winEntry{{id: 5, rect: image.Rect(0, 0, 100, 100)}},
		top:  5, haveTop: true}
	r, _, ff := newRouterForTest(t, m)
	asks := 0
	r.rootFn = func(w xproto.Window) xproto.Window { asks++; return 50 } // client 5 lives in frame 50
	ff.setUnder(50)                                                      // ... which is already on top

	r.MovePointer(50, 50)
	r.Button(1, true)
	r.Key(0x61, true)

	if len(ff.raise) != 1 || ff.raise[0] != 50 {
		t.Fatalf("raises = %v, want one raise of the frame 50", ff.raise)
	}
	if len(ff.focus) < 2 {
		t.Fatalf("focuses = %v, want the client 5 focused on press and key", ff.focus)
	}
	for _, w := range ff.focus {
		if w != 5 {
			t.Fatalf("focused %v, want the client 5", ff.focus)
		}
	}
	if asks != 1 {
		t.Fatalf("the root-level ancestor was asked of X %d times, want 1 (remembered)", asks)
	}
}

// A window manager applies a raise some time after it is asked: the click
// must not be injected while another window is still stacked over the
// target, or it goes to that window instead — the viewer itself, when the
// shared session is watched on the host's own screen.
func TestRouterClickWaitsUntilTheRaiseHasLanded(t *testing.T) {
	m := &fakeMap{origin: image.Pt(0, 0),
		wins: []winEntry{{id: 5, rect: image.Rect(0, 0, 100, 100)}},
		top:  5, haveTop: true}
	r, rec, ff := newRouterForTest(t, m)
	r.rootFn = func(w xproto.Window) xproto.Window { return 50 } // client 5 lives in frame 50
	ff.setUnder(99)                                              // something else is on top of it

	var atPress []xproto.Window
	r.inj.fake = func(typ, detail byte, x, y int16) error {
		if typ == evButtonPress {
			atPress = append(atPress, ff.getUnder())
		}
		return rec.record(typ, detail, x, y)
	}
	// The window manager gets to the raise a moment later.
	time.AfterFunc(15*time.Millisecond, func() { ff.setUnder(50) })

	r.MovePointer(50, 50)
	r.Button(1, true)

	if len(atPress) == 0 {
		t.Fatal("the click was never injected")
	}
	if atPress[0] != 50 {
		t.Fatalf("the click was injected while %#x was still on top; want it after the frame 50 landed",
			uint32(atPress[0]))
	}
}
