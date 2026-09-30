package viewer

import "testing"

// recordListener captures what the window reports.
type recordListener struct {
	calls int
	w, h  int
}

func (r *recordListener) MouseMove(x, y int)        {}
func (r *recordListener) Button(b uint8, down bool) {}
func (r *recordListener) Key(ks uint32, down bool)  {}
func (r *recordListener) Resize(w, h int) {
	r.calls++
	r.w, r.h = w, h
}

// The size the window was created with is never reported — that
// ConfigureNotify is a window manager's placement, not the user — while
// every later distinct size is, including a return to the original.
func TestReportResizeDeduplicatesAgainstCreatedSize(t *testing.T) {
	w := &Window{winW: 800, winH: 600, sentW: 800, sentH: 600}
	l := &recordListener{}

	w.reportResize(l)
	if l.calls != 0 {
		t.Fatalf("created size reported %d times, want 0", l.calls)
	}

	w.winW, w.winH = 1024, 768
	w.reportResize(l)
	w.reportResize(l) // a ConfigureNotify repeating the same size
	if l.calls != 1 {
		t.Fatalf("1024x768 reported %d times, want 1", l.calls)
	}
	if l.w != 1024 || l.h != 768 {
		t.Errorf("reported %dx%d, want 1024x768", l.w, l.h)
	}

	// Dragging back to the created size is a real change: the host may
	// have been resized in between.
	w.winW, w.winH = 800, 600
	w.reportResize(l)
	if l.calls != 2 {
		t.Fatalf("return to 800x600 reported %d times total, want 2", l.calls)
	}

	// Degenerate sizes from a half-mapped window are dropped.
	w.winW, w.winH = 0, 0
	w.reportResize(l)
	if l.calls != 2 {
		t.Fatalf("zero size was reported (calls=%d)", l.calls)
	}
}
