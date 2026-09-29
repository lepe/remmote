package viewer

import (
	"bytes"
	xdraw "golang.org/x/image/draw"
	"image"
	"testing"
	"time"
)

// The blitter's partial-draw path maps a canvas dirty rect into window
// coordinates (scaleOut) and back to the source the scaler needs
// (scaleIn). A wrong mapping shows up as a corrupted or stale window,
// which no other test can see: the e2e delta test snapshots the canvas,
// not the window. These are pure functions, so assert the invariants
// directly.

func TestScaleOutIn1ToOne(t *testing.T) {
	const w, h = 800, 600
	fit := FitRect(w, h, w, h) // 1:1 window: fit is the whole window
	if fit != image.Rect(0, 0, w, h) {
		t.Fatalf("fit = %v, want full window", fit)
	}
	for _, r := range []image.Rectangle{
		image.Rect(0, 0, w, h),
		image.Rect(10, 20, 30, 40),
		image.Rect(0, 0, 1, 1),
		image.Rect(w-1, h-1, w, h),
	} {
		if got := scaleOut(r, w, h, fit); got != r {
			t.Errorf("scaleOut(%v) = %v, want identity", r, got)
		}
		if got := scaleIn(r, w, h, fit); got != r {
			t.Errorf("scaleIn(%v) = %v, want identity", r, got)
		}
	}
}

// scaleOut must stay inside the letterboxed fit rect, and scaleIn must
// return a source region that fully covers it — otherwise scaling a
// partial region samples short of the pixels it must produce.
func TestScaleOutInRoundTrip(t *testing.T) {
	cases := []struct {
		winW, winH int // window
		cw, ch     int // canvas
	}{
		{800, 600, 1920, 1080}, // downscale, 16:9 into 4:3 → bars top/bottom
		{800, 600, 800, 600},   // 1:1
		{800, 600, 600, 800},   // portrait canvas → bars left/right
		{640, 480, 1024, 768},  // downscale, same aspect
		{1920, 1080, 800, 600}, // upscale
		{137, 89, 641, 379},    // odd sizes, near 16:9
		{100, 100, 4000, 40},   // extreme aspect
	}
	dirty := []image.Rectangle{
		{image.Point{}, image.Point{}},
		image.Rect(1, 1, 2, 2),
		image.Rect(10, 10, 11, 11),        // single pixel
		image.Rect(0, 0, 3, 7),            // top-left corner
		image.Rect(7, 5, 13, 9),           // interior
		image.Rect(1000, 700, 1920, 1080), // bottom-right corner
		image.Rect(0, 0, 1921, 1081),      // whole canvas + overflow
		image.Rect(640, 540, 1920, 1080),  // bottom half
	}
	for _, c := range cases {
		fit := FitRect(c.winW, c.winH, c.cw, c.ch)
		if fit.Empty() {
			t.Fatalf("FitRect(%d,%d,%d,%d) empty", c.winW, c.winH, c.cw, c.ch)
		}
		canvasRect := image.Rect(0, 0, c.cw, c.ch)
		for _, raw := range dirty {
			r := raw.Intersect(canvasRect)
			if r.Empty() {
				continue
			}
			out := scaleOut(r, c.cw, c.ch, fit).Intersect(fit)
			if out.Empty() {
				t.Errorf("win %dx%d canvas %dx%d: scaleOut(%v) empty", c.winW, c.winH, c.cw, c.ch, r)
				continue
			}
			if !out.In(fit) {
				t.Errorf("win %dx%d canvas %dx%d: scaleOut(%v)=%v escapes fit %v",
					c.winW, c.winH, c.cw, c.ch, r, out, fit)
			}
			in := scaleIn(out, c.cw, c.ch, fit)
			if !r.In(in) {
				t.Errorf("win %dx%d canvas %dx%d: scaleIn(scaleOut(%v)=%v)=%v does not cover %v",
					c.winW, c.winH, c.cw, c.ch, r, out, in, r)
			}
			if !in.In(canvasRect) {
				t.Errorf("win %dx%d canvas %dx%d: scaleIn(%v)=%v escapes canvas %v",
					c.winW, c.winH, c.cw, c.ch, out, in, canvasRect)
			}
		}
	}
}

// Scaling the whole canvas must reproduce the whole fit rect, bars
// included — that is what the full-draw path pushes.
func TestScaleOutFullCanvas(t *testing.T) {
	for _, c := range [][4]int{{800, 600, 1920, 1080}, {1920, 1080, 800, 600}, {640, 480, 640, 480}} {
		winW, winH, cw, ch := c[0], c[1], c[2], c[3]
		fit := FitRect(winW, winH, cw, ch)
		got := scaleOut(image.Rect(0, 0, cw, ch), cw, ch, fit).Intersect(fit)
		if got != fit {
			t.Errorf("canvas %dx%d in window %dx%d: scaleOut(full)=%v, want fit %v",
				cw, ch, winW, winH, got, fit)
		}
	}
}

func TestRenderNotificationsDoNotBlockInput(t *testing.T) {
	b := newBlitter(nil, 0, 0, nil, nil)
	b.mu.Lock() // model a slow scaler or X request on the drawing goroutine
	done := make(chan struct{})
	go func() { b.FullRedraw(); b.Complete(17); b.RequestResize(800, 600); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		b.mu.Unlock()
		t.Fatal("input pump blocked behind rendering")
	}
	b.mu.Unlock()
	b.segs[0] = &segPair{id: 18}
	b.inFlight[0] = time.Now()
	b.consumeCompletions()
	if b.inFlight[0].IsZero() {
		t.Fatal("old completion freed a new segment")
	}
	b.Complete(18)
	b.consumeCompletions()
	if !b.inFlight[0].IsZero() {
		t.Fatal("completion did not free its segment")
	}
}

func TestScaleRegionMatchesFullFrame(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 83, 59))
	for i := range src.Pix {
		src.Pix[i] = byte(i * 17)
	}
	for _, fast := range []bool{false, true} {
		for _, size := range []image.Point{image.Pt(137, 89), image.Pt(41, 37)} {
			fit := FitRect(size.X, size.Y, 83, 59)
			full := image.NewRGBA(image.Rectangle{Max: size})
			partial := image.NewRGBA(full.Rect)
			if fast {
				xdraw.NearestNeighbor.Scale(full, fit, src, src.Rect, xdraw.Src, nil)
			} else {
				xdraw.ApproxBiLinear.Scale(full, fit, src, src.Rect, xdraw.Src, nil)
			}
			// Arbitrary tiled redraws must produce exactly the full-frame result.
			for y := 0; y < size.Y; y += 7 {
				for x := 0; x < size.X; x += 9 {
					r := image.Rect(x, y, min(x+9, size.X), min(y+7, size.Y))
					scaleRegion(partial, src, fit, r, fast)
				}
			}
			if !bytes.Equal(full.Pix, partial.Pix) {
				t.Fatalf("partial transform differs: fast=%v size=%v", fast, size)
			}
		}
	}
}

func BenchmarkViewerScale(b *testing.B) {
	src := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
	dst := image.NewRGBA(image.Rect(0, 0, 1536, 864))
	for _, fast := range []bool{false, true} {
		name := "smooth"
		if fast {
			name = "nearest"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				scaleRegion(dst, src, dst.Rect, dst.Rect, fast)
			}
		})
	}
}
