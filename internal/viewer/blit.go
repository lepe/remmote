package viewer

import (
	"image"
	"log/slog"
	"sync"
	"time"

	xdraw "golang.org/x/image/draw"

	"github.com/jezek/xgb/shm"
	"github.com/jezek/xgb/xproto"

	"github.com/lepe/remmote/internal/capture"
	"github.com/lepe/remmote/internal/xconn"
)

// completionWatchdog frees a segment the X server never acknowledged.
const completionWatchdog = 250 * time.Millisecond

// Blitter draws the canvas into the client window, scaled to fit with
// black letterbox bars. Primary path: MIT-SHM PutImage with two
// ping-pong segments tracked by Completion events; fallback: banded core
// PutImage.
//
// Concurrency: the event pump calls Resize/CompleteNext; the draw
// goroutine calls draw. All mutable state is guarded by mu; draw holds it
// for the whole frame (a few ms), Resize is rare.
type Blitter struct {
	xc     *xconn.Conn
	canvas *Canvas
	win    xproto.Window
	gc     xproto.Gcontext
	log    *slog.Logger

	mu         sync.Mutex
	winW, winH int
	scaled     *image.RGBA // winW × winH staging buffer

	segs     [2]*segPair
	inFlight [2]time.Time // when each segment was last submitted
	pending  []int        // submitted-but-not-completed, FIFO

	dirty chan struct{}
	done  chan struct{}
}

type segPair struct {
	local *capture.SHMSegment
	id    shm.Seg
}

func newBlitter(xc *xconn.Conn, win xproto.Window, gc xproto.Gcontext, canvas *Canvas, log *slog.Logger) *Blitter {
	return &Blitter{
		xc:     xc,
		canvas: canvas,
		win:    win,
		gc:     gc,
		log:    log,
		dirty:  make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
}

// Start launches the draw goroutine (60 fps coalescing).
func (b *Blitter) Start() {
	b.mu.Lock()
	b.allocSegments()
	b.mu.Unlock()
	go b.loop()
}

func (b *Blitter) hasSHM() bool { return b.xc.Ext.SHM }

// Stop terminates the draw goroutine and releases segments.
func (b *Blitter) Stop() {
	close(b.done)
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
	if b.hasSHM() {
		b.releaseSegments()
		b.allocSegments()
	}
	b.pending = b.pending[:0]
	b.Dirty()
}

// CompleteNext frees the oldest in-flight segment. Completion events do
// not identify their segment, but X delivers them in request order —
// and we submit segments in order — so FIFO matching is exact.
// Caller: event pump.
func (b *Blitter) CompleteNext() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pending) == 0 {
		return
	}
	i := b.pending[0]
	b.pending = b.pending[1:]
	b.inFlight[i] = time.Time{}
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
	b.pending = b.pending[:0]
}

func (b *Blitter) loop() {
	ticker := time.NewTicker(time.Second / 60)
	defer ticker.Stop()
	for {
		select {
		case <-b.done:
			return
		case <-b.dirty:
		case <-ticker.C:
			// Redraw only when something changed since last time.
			select {
			case <-b.dirty:
			default:
				continue
			}
		}
		b.draw()
	}
}

// draw composites the canvas into the staging buffer and pushes it to
// the window. Runs on the draw goroutine only.
func (b *Blitter) draw() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.winW == 0 || b.winH == 0 || b.scaled == nil {
		return
	}
	b.canvas.withView(func(src *image.RGBA) {
		fit := FitRect(b.winW, b.winH, src.Rect.Dx(), src.Rect.Dy())
		fillBlack(b.scaled)
		xdraw.ApproxBiLinear.Scale(b.scaled, fit, src, src.Rect, xdraw.Src, nil)
	})

	if b.hasSHM() && b.segs[0] != nil {
		i := b.takeSegment()
		if i < 0 {
			return // both in flight; next tick will retry
		}
		seg := b.segs[i]
		if err := capture.RGBAToBGRA(seg.local.Mem(), b.scaled, b.winW, b.winH, b.xc.LSBFirst()); err != nil {
			b.log.Warn("viewer convert", "err", err)
			b.dropPending(i)
			return
		}
		if err := shm.PutImageChecked(b.xc.X, xproto.Drawable(b.win), b.gc,
			uint16(b.winW), uint16(b.winH), 0, 0, uint16(b.winW), uint16(b.winH), 0, 0,
			24, xproto.ImageFormatZPixmap, 1, seg.id, 0).Check(); err != nil {
			b.log.Warn("viewer PutImage", "err", err)
			b.dropPending(i)
		}
		return
	}

	// Core fallback: banded PutImage of the whole staging buffer.
	buf := make([]byte, b.winW*b.winH*4)
	if err := capture.RGBAToBGRA(buf, b.scaled, b.winW, b.winH, b.xc.LSBFirst()); err != nil {
		b.log.Warn("viewer convert", "err", err)
		return
	}
	const maxBandBytes = 4 << 20
	bandH := maxBandBytes / (b.winW * 4)
	if bandH < 1 {
		bandH = 1
	}
	for y0 := 0; y0 < b.winH; y0 += bandH {
		bh := min(bandH, b.winH-y0)
		if err := xproto.PutImageChecked(b.xc.X, xproto.ImageFormatZPixmap, xproto.Drawable(b.win), b.gc,
			uint16(b.winW), uint16(bh), 0, int16(y0), 0, 24,
			buf[y0*b.winW*4:(y0+bh)*b.winW*4]).Check(); err != nil {
			b.log.Warn("viewer core PutImage", "err", err)
			return
		}
	}
}

// takeSegment returns a free segment index, or -1. A segment whose
// Completion event is overdue (broken SHM server) is force-freed.
// Requires b.mu held; itself appends to pending.
func (b *Blitter) takeSegment() int {
	for i := 0; i < 2; i++ {
		if b.segs[i] == nil {
			continue
		}
		free := b.inFlight[i].IsZero()
		if !free && time.Since(b.inFlight[i]) > completionWatchdog {
			free = true // watchdog kick
		}
		if free {
			b.inFlight[i] = time.Now()
			b.pending = append(b.pending, i)
			return i
		}
	}
	return -1
}

// dropPending removes i from the completion FIFO (submission failed, so
// no Completion will arrive for it). Requires b.mu held.
func (b *Blitter) dropPending(i int) {
	b.inFlight[i] = time.Time{}
	for j, v := range b.pending {
		if v == i {
			b.pending = append(b.pending[:j], b.pending[j+1:]...)
			return
		}
	}
}

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
