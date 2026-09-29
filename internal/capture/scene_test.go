package capture

import (
	"image"
	"testing"
)

func TestQuantizeOutward(t *testing.T) {
	cases := []struct {
		in, want image.Rectangle
	}{
		{image.Rect(50, 60, 450, 260), image.Rect(32, 32, 480, 288)},
		{image.Rect(32, 0, 480, 288), image.Rect(32, 0, 480, 288)}, // already aligned
		{image.Rect(0, 0, 100, 100), image.Rect(0, 0, 128, 128)},
		{image.Rect(1, 1, 31, 31), image.Rect(0, 0, 32, 32)},
		{image.Rect(-10, -10, 10, 10), image.Rect(-32, -32, 32, 32)}, // windows offscreen top-left
	}
	for i, tc := range cases {
		if got := quantizeOutward(tc.in); got != tc.want {
			t.Errorf("case %d: quantizeOutward(%v) = %v, want %v", i, tc.in, got, tc.want)
		}
	}
}

// setMapped is the single place the mapped flag changes. It must report a
// change exactly once per transition: a repeated reading of the same state
// must not keep re-dirtying the window (rescan re-reads every 2 s), while
// a real transition must schedule a repaint so the canvas bbox and the
// pixels both follow.
func TestSetMappedReportsTransitionsOnce(t *testing.T) {
	var w trackedWin
	if !setMapped(&w, true) {
		t.Fatal("first map must report a change")
	}
	if !w.mapped {
		t.Fatal("mapped flag not set")
	}
	if !w.stale || w.dirty.Empty() {
		t.Fatal("a map must mark the window stale and dirty")
	}
	if setMapped(&w, true) {
		t.Fatal("re-reading the same state must not report a change")
	}

	w.dirty = image.Rectangle{}
	if !setMapped(&w, false) {
		t.Fatal("unmap must report a change")
	}
	if w.mapped {
		t.Fatal("mapped flag not cleared")
	}
	if !w.stale || w.dirty.Empty() {
		t.Fatal("an unmap must mark the window stale and dirty")
	}
	if setMapped(&w, false) {
		t.Fatal("re-reading the same state must not report a change again")
	}
}

// The window→canvas mapping math used by TakePending and Capture must
// be exact inverses of each other.
func TestWindowCanvasMapping(t *testing.T) {
	canvas := image.Rect(32, 32, 480, 288) // root coords
	win := image.Rect(50, 60, 350, 260)    // root coords
	dirty := image.Rect(10, 20, 30, 40)    // window-local

	// window-local → canvas (TakePending path)
	got := dirty.Add(win.Min).Sub(canvas.Min)
	want := image.Rect(10+50-32, 20+60-32, 30+50-32, 40+60-32)
	if got != want {
		t.Fatalf("local→canvas = %v, want %v", got, want)
	}

	// canvas → window-local (Capture path) must invert it.
	back := got.Add(canvas.Min).Sub(win.Min)
	if back != dirty {
		t.Fatalf("canvas→local = %v, want %v", back, dirty)
	}
}
