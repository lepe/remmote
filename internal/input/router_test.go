package input

import (
	"image"
	"testing"

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

// fakeFocus records raise/focus calls.
type fakeFocus struct {
	raise []xproto.Window
	focus []xproto.Window
}

func newRouterForTest(t *testing.T, m WindowMap) (*Router, *recorder, *fakeFocus) {
	t.Helper()
	rec := &recorder{}
	ff := &fakeFocus{}
	inj := &Injector{
		km: testKeymap(), sw: 1000, sh: 1000, log: testLogger(),
		held: map[xproto.Keysym]xproto.Keycode{},
		fake: rec.record,
	}
	r := &Router{inj: inj, x: nil, mmap: m, log: testLogger()}
	r.raiseFn = func(w xproto.Window) error { ff.raise = append(ff.raise, w); return nil }
	r.focusFn = func(w xproto.Window) error { ff.focus = append(ff.focus, w); return nil }
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
