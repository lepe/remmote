package viewer

import (
	"image"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	xdraw "golang.org/x/image/draw"

	"github.com/jezek/xgb/shm"
	"github.com/jezek/xgb/xproto"

	"github.com/lepe/remmote/internal/capture"
	"github.com/lepe/remmote/internal/xconn"
)

// Blitter draws the canvas into the client window, scaled to fit with
// black letterbox bars. Primary path: MIT-SHM PutImage with two
// ping-pong segments tracked by Completion events; fallback: banded core
// PutImage.
//
// Concurrency: the event pump calls RequestResize/Complete; the draw
// goroutine calls draw. All mutable state is guarded by mu; draw holds it
// for the whole frame (a few ms), Resize is rare.
type Blitter struct {
	xc     *xconn.Conn
	canvas *Canvas
	win    xproto.Window
	gc     xproto.Gcontext
	log    *slog.Logger

	mu              sync.Mutex
	winW, winH      int
	scaled          *image.RGBA // winW × winH staging buffer (scaled mode)
	coreBuf         []byte      // reusable core-PutImage buffer
	fastScale       bool        // nearest-neighbor scaling when detail is less important
	fullRequested   atomic.Bool
	completions     chan shm.Seg
	resizeRequested chan image.Point
	forceFull       bool // repaint the whole window (resize/expose)

	segs     [2]*segPair
	inFlight [2]time.Time // when each segment was last submitted

	dirty   chan struct{}
	done    chan struct{}
	stopped chan struct{}
}

type segPair struct {
	local *capture.SHMSegment
	id    shm.Seg
}

func newBlitter(xc *xconn.Conn, win xproto.Window, gc xproto.Gcontext, canvas *Canvas, log *slog.Logger) *Blitter {
	return &Blitter{
		xc:              xc,
		canvas:          canvas,
		win:             win,
		gc:              gc,
		log:             log,
		dirty:           make(chan struct{}, 1),
		done:            make(chan struct{}),
		stopped:         make(chan struct{}),
		resizeRequested: make(chan image.Point, 1),
		completions:     make(chan shm.Seg, 16),
	}
}

// Start launches the draw goroutine (60 fps coalescing).
func (b *Blitter) Start() {
	b.mu.Lock()
	if b.segs[0] == nil && b.hasSHM() {
		b.allocSegments()
	}
	b.mu.Unlock()
	go func() { defer close(b.stopped); b.loop() }()
}

func (b *Blitter) hasSHM() bool { return b.xc.Ext.SHM }

// Stop terminates the draw goroutine and releases segments.
func (b *Blitter) Stop() {
	close(b.done)
	<-b.stopped
	b.mu.Lock()
	b.releaseSegments()
	b.mu.Unlock()
}

// Dirty requests a redraw (coalesced).
func (b *Blitter) Dirty() {
	select {
	case b.dirty <- struct{}{}:
	default:
	}
}

// FullRedraw forces a complete window repaint: expose (the X server
// painted over us) and letterbox-bar changes. Partial canvas updates use
// Dirty instead — the canvas already tracks what changed.
func (b *Blitter) FullRedraw() {
	b.fullRequested.Store(true)
	b.Dirty()
}

// Size returns the current window size (for input mapping).
func (b *Blitter) Size() (int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.winW, b.winH
}

// Resize adapts to a new window size. Caller: event pump.
func (b *Blitter) Resize(w, h int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if w <= 0 || h <= 0 || (w == b.winW && h == b.winH) {
		return
	}
	b.winW, b.winH = w, h
	b.scaled = image.NewRGBA(image.Rect(0, 0, w, h))
	b.forceFull = true // new geometry: bars and everything else repaint
	if b.hasSHM() {
		b.releaseSegments()
		b.allocSegments()
	}
	b.Dirty()
}

// Complete queues an acknowledgement without blocking the input event pump.
// Match the segment id so late completions after resize cannot free new buffers.
func (b *Blitter) Complete(id shm.Seg) {
	select {
	case b.completions <- id:
	default:
		// Only two current segments can be in flight; an overflow is old
		// resize traffic. Keep the most recent acknowledgements.
		select {
		case <-b.completions:
		default:
		}
		select {
		case b.completions <- id:
		default:
		}
	}
}

// RequestResize keeps geometry work on the draw goroutine.
func (b *Blitter) RequestResize(w, h int) {
	select {
	case <-b.resizeRequested:
	default:
	}
	b.resizeRequested <- image.Pt(w, h)
	b.Dirty()
}

// consumeCompletions requires b.mu held.
func (b *Blitter) consumeCompletions() {
	for {
		select {
		case id := <-b.completions:
			for i, seg := range b.segs {
				if seg != nil && seg.id == id {
					b.inFlight[i] = time.Time{}
				}
			}
		default:
			return
		}
	}
}

// allocSegments and releaseSegments require b.mu held.
func (b *Blitter) allocSegments() {
	for i := 0; i < 2; i++ {
		seg, err := capture.NewSHMSegment(b.winW * b.winH * 4)
		if err != nil {
			b.log.Warn("viewer SHM segment failed; using core PutImage", "err", err)
			b.releaseSegments()
			return
		}
		segID, err := b.xc.X.NewId()
		if err != nil {
			seg.Close()
			b.releaseSegments()
			return
		}
		if err := shm.AttachChecked(b.xc.X, shm.Seg(segID), uint32(seg.ID()), false).Check(); err != nil {
			seg.Close()
			b.log.Warn("viewer SHM attach failed; using core PutImage", "err", err)
			b.releaseSegments()
			return
		}
		b.segs[i] = &segPair{local: seg, id: shm.Seg(segID)}
	}
}

func (b *Blitter) releaseSegments() {
	for i := 0; i < 2; i++ {
		if b.segs[i] != nil {
			_ = shm.DetachChecked(b.xc.X, b.segs[i].id).Check()
			b.segs[i].local.Close()
			b.segs[i] = nil
		}
		b.inFlight[i] = time.Time{}
	}
}

func (b *Blitter) loop() {
	// Draw immediately after an idle gap, but cap sustained work (including
	// retries while SHM segments are busy) at 60 Hz. A rearmed dirty signal
	// must not spin continuously waiting for a completion event.
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	var lastDraw time.Time
	for {
		select {
		case <-b.done:
			return
		case <-b.dirty:
		}
		if wait := time.Second/60 - time.Since(lastDraw); wait > 0 {
			timer.Reset(wait)
			select {
			case <-b.done:
				return
			case <-timer.C:
			}
		}
		select {
		case size := <-b.resizeRequested:
			b.Resize(size.X, size.Y)
		default:
		}
		lastDraw = time.Now()
		b.draw()
	}
}

// draw pushes what changed since the last draw into the window: the
// canvas's dirty region is scaled (when needed), converted, and
// PutImage'd — only a full redraw touches the whole window. At 1:1 the
// canvas converts straight into the SHM segment with no scaler or staging
// copy at all. Runs on the draw goroutine only.
func (b *Blitter) draw() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.winW == 0 || b.winH == 0 || b.scaled == nil {
		return
	}
	b.consumeCompletions()
	full := b.fullRequested.Swap(false) || b.forceFull
	b.forceFull = false
	dirty := b.canvas.TakeDirty()
	if dirty.Empty() && !full {
		return
	}
	// rearm re-arms the intent of this draw when it could not complete
	// (segments in flight, request error) so no update is lost.
	rearm := func() {
		if full {
			b.forceFull = true
		} else {
			b.canvas.markDirty(dirty)
		}
		b.Dirty()
	}

	var failed bool
	b.canvas.withView(func(src *image.RGBA) {
		cw, ch := src.Rect.Dx(), src.Rect.Dy()
		if cw == 0 || ch == 0 {
			return
		}
		fit := FitRect(b.winW, b.winH, cw, ch)
		oneToOne := cw == b.winW && ch == b.winH // fit == full window

		// dstR is the window region this draw covers.
		var dstR image.Rectangle
		if full {
			dstR = image.Rect(0, 0, b.winW, b.winH)
		} else {
			region := dirty
			if !oneToOne && !b.fastScale {
				region = region.Inset(-1).Intersect(src.Rect)
			}
			dstR = scaleOut(region, cw, ch, fit).Intersect(fit)
			if dstR.Empty() {
				return
			}
		}

		// convSrc/convRect is the RGBA region to convert into the
		// segment. Scaled mode draws the region through the scaler
		// into staging first; 1:1 converts the canvas region in
		// place — no scaler, no staging copy.
		convSrc, convRect := src, dstR
		if !oneToOne {
			if full {
				fillBlack(b.scaled)
			}
			scaleRegion(b.scaled, src, fit, dstR, b.fastScale)
			convSrc = b.scaled
		}

		if b.hasSHM() && b.segs[0] != nil {
			i := b.takeSegment()
			if i < 0 {
				failed = true // both in flight; retry next tick
				return
			}
			seg := b.segs[i]
			off := (dstR.Min.Y*b.winW + dstR.Min.X) * 4
			if err := capture.RegionToBGRA(seg.local.Mem()[off:], b.winW*4, convSrc, convRect, b.xc.LSBFirst()); err != nil {
				b.log.Warn("viewer convert", "err", err)
				b.dropPending(i)
				failed = true
				return
			}
			if err := shm.PutImageChecked(b.xc.X, xproto.Drawable(b.win), b.gc,
				uint16(b.winW), uint16(b.winH),
				uint16(dstR.Min.X), uint16(dstR.Min.Y),
				uint16(dstR.Dx()), uint16(dstR.Dy()),
				int16(dstR.Min.X), int16(dstR.Min.Y),
				24, xproto.ImageFormatZPixmap, 1, seg.id, 0).Check(); err != nil {
				b.log.Warn("viewer PutImage", "err", err)
				b.dropPending(i)
				failed = true
			}
			return
		}

		// Core fallback: banded PutImage of the region.
		w, h := dstR.Dx(), dstR.Dy()
		if cap(b.coreBuf) < w*h*4 {
			b.coreBuf = make([]byte, w*h*4)
		}
		buf := b.coreBuf[:w*h*4]
		if err := capture.RegionToBGRA(buf, w*4, convSrc, convRect, b.xc.LSBFirst()); err != nil {
			b.log.Warn("viewer convert", "err", err)
			failed = true
			return
		}
		const maxBandBytes = 4 << 20
		bandH := maxBandBytes / (w * 4)
		if bandH < 1 {
			bandH = 1
		}
		for y0 := 0; y0 < h; y0 += bandH {
			bh := min(bandH, h-y0)
			if err := xproto.PutImageChecked(b.xc.X, xproto.ImageFormatZPixmap, xproto.Drawable(b.win), b.gc,
				uint16(w), uint16(bh), int16(dstR.Min.X), int16(dstR.Min.Y+y0), 0, 24,
				buf[y0*w*4:(y0+bh)*w*4]).Check(); err != nil {
				b.log.Warn("viewer core PutImage", "err", err)
				failed = true
				return
			}
		}
	})
	if failed {
		rearm()
	}
}

// scaleRegion keeps the full-frame transform and clips destination writes.
// Rescaling a cropped source independently changes pixel alignment on deltas.
func scaleRegion(dst, src *image.RGBA, fit, clip image.Rectangle, fast bool) {
	if fast {
		nearestRegion(dst, src, fit, clip)
		return
	}
	xdraw.ApproxBiLinear.Scale(dst.SubImage(clip).(*image.RGBA), fit, src, src.Rect, xdraw.Src, nil)
}

// nearestRegion computes horizontal samples once per region, avoiding the
// general scaler's integer division for every pixel on every row.
func nearestRegion(dst, src *image.RGBA, fit, clip image.Rectangle) {
	clip = clip.Intersect(fit).Intersect(dst.Rect)
	if clip.Empty() || src.Rect.Empty() {
		return
	}
	var offsets [2048]int
	xs := offsets[:min(clip.Dx(), len(offsets))]
	if clip.Dx() > len(offsets) {
		xs = make([]int, clip.Dx())
	}
	for x := range xs {
		xs[x] = int((2*uint64(clip.Min.X+x-fit.Min.X)+1)*uint64(src.Rect.Dx())/(2*uint64(fit.Dx()))) * 4
	}
	for y := clip.Min.Y; y < clip.Max.Y; y++ {
		sy := src.Rect.Min.Y + int((2*uint64(y-fit.Min.Y)+1)*uint64(src.Rect.Dy())/(2*uint64(fit.Dy())))
		row := src.Pix[src.PixOffset(src.Rect.Min.X, sy):]
		d := dst.Pix[dst.PixOffset(clip.Min.X, y):][:clip.Dx()*4]
		for x, sx := range xs {
			copy(d[x*4:x*4+4], row[sx:sx+4])
		}
	}
}

// scaleOut maps a canvas region to window coordinates, rounded outward,
// intersected with the fit rect.
func scaleOut(r image.Rectangle, cw, ch int, fit image.Rectangle) image.Rectangle {
	sx := float64(fit.Dx()) / float64(cw)
	sy := float64(fit.Dy()) / float64(ch)
	return image.Rect(
		fit.Min.X+int(math.Floor(float64(r.Min.X)*sx)),
		fit.Min.Y+int(math.Floor(float64(r.Min.Y)*sy)),
		fit.Min.X+int(math.Ceil(float64(r.Max.X)*sx)),
		fit.Min.Y+int(math.Ceil(float64(r.Max.Y)*sy)),
	)
}

// scaleIn maps a window region back to canvas coordinates, rounded
// outward, intersected with the canvas: the source the scaler needs to
// produce scaleOut's result.
func scaleIn(r image.Rectangle, cw, ch int, fit image.Rectangle) image.Rectangle {
	ix := float64(cw) / float64(fit.Dx())
	iy := float64(ch) / float64(fit.Dy())
	return image.Rect(
		int(math.Floor(float64(r.Min.X-fit.Min.X)*ix)),
		int(math.Floor(float64(r.Min.Y-fit.Min.Y)*iy)),
		int(math.Ceil(float64(r.Max.X-fit.Min.X)*ix)),
		int(math.Ceil(float64(r.Max.Y-fit.Min.Y)*iy)),
	)
}

// takeSegment requires b.mu held. Never reuse shared memory until X has
// acknowledged it; elapsed time alone does not make an in-flight buffer safe.
func (b *Blitter) takeSegment() int {
	for i := range b.segs {
		if b.segs[i] != nil && b.inFlight[i].IsZero() {
			b.inFlight[i] = time.Now()
			return i
		}
	}
	return -1
}

// dropPending releases a submission rejected by X (no completion will arrive).
func (b *Blitter) dropPending(i int) { b.inFlight[i] = time.Time{} }

// FitRect computes the letterbox rect of an imgW×imgH image inside a
// winW×winH window (at least 1×1).
func FitRect(winW, winH, imgW, imgH int) image.Rectangle {
	if imgW <= 0 || imgH <= 0 || winW <= 0 || winH <= 0 {
		return image.Rectangle{}
	}
	scale := min(float64(winW)/float64(imgW), float64(winH)/float64(imgH))
	w := max(1, int(float64(imgW)*scale+0.5))
	h := max(1, int(float64(imgH)*scale+0.5))
	w, h = min(w, winW), min(h, winH)
	x := (winW - w) / 2
	y := (winH - h) / 2
	return image.Rect(x, y, x+w, y+h)
}

func fillBlack(img *image.RGBA) {
	for i := 0; i < len(img.Pix); i += 4 {
		p := img.Pix[i : i+4 : i+4]
		p[0], p[1], p[2], p[3] = 0, 0, 0, 0xFF
	}
}
