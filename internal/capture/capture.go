// Package capture grabs rectangles of an X11 root window, using MIT-SHM
// GetImage when available and a banded core GetImage fallback otherwise.
// Dirty-rect tracking (XDamage) and screen-resize detection (RANDR) are
// folded in: a single event-pump goroutine inside the Capturer merges
// damage notifications into one pending bounding box that the capture
// loop drains at its own pace.
package capture

import (
	"fmt"
	"image"
	"image/png"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/jezek/xgb/damage"
	"github.com/jezek/xgb/shm"
	"github.com/jezek/xgb/xproto"

	"github.com/lepe/remmote/internal/xconn"
	"github.com/lepe/remmote/internal/xwin"
)

// Options configures a Capturer.
type Options struct {
	FPS         int           // frame cap; damage arriving within one interval merges
	FullRefresh time.Duration // periodic full-frame keyframe interval
}

// Capturer captures rects of the root window. Concurrency contract:
// HandleEvent is called from the internal pump goroutine only; everything
// else is called from the single capture-loop goroutine.
type Capturer struct {
	xc   *xconn.Conn
	xq   *xwin.Client // RANDR helpers (screen resize)
	log  *slog.Logger
	opts Options

	seg     *SHMSegment
	segID   shm.Seg
	scratch *image.RGBA // full-screen buffer; Capture returns subimages of it

	hasSHM    bool
	hasDamage bool
	dmg       damage.Damage

	mu      sync.Mutex
	pending image.Rectangle

	changed chan struct{}
	resized chan struct{}

	shmWarned bool
}

// New wires up SHM, damage, and the event pump.
func New(xc *xconn.Conn, opts Options, log *slog.Logger) (*Capturer, error) {
	if opts.FPS <= 0 {
		opts.FPS = 30
	}
	if opts.FullRefresh <= 0 {
		opts.FullRefresh = 2 * time.Second
	}
	c := &Capturer{
		xc:      xc,
		xq:      xwin.NewClient(xc.X, xc.Root()),
		log:     log,
		opts:    opts,
		changed: make(chan struct{}, 1),
		resized: make(chan struct{}, 1),
	}
	if err := c.allocBuffers(); err != nil {
		return nil, err
	}
	if xc.Ext.DAMAGE {
		if err := c.initDamage(); err != nil {
			return nil, fmt.Errorf("capture: damage init: %w", err)
		}
		c.hasDamage = true
	} else {
		log.Warn("X DAMAGE extension missing; falling back to periodic full refresh only")
	}
	go c.pump()
	return c, nil
}

// FPS is the configured frame cap.
func (c *Capturer) FPS() int { return c.opts.FPS }

// FullRefresh is the configured keyframe interval.
func (c *Capturer) FullRefresh() time.Duration { return c.opts.FullRefresh }

// ScreenRect is the current full-screen capture target.
func (c *Capturer) ScreenRect() image.Rectangle {
	w, h := c.xc.ScreenSize()
	return image.Rect(0, 0, int(w), int(h))
}

// HasDamage reports whether dirty-rect tracking is active.
func (c *Capturer) HasDamage() bool { return c.hasDamage }

// ShouldFullRefresh reports whether a dirty bbox is large enough that
// encoding the whole screen beats encoding just the bbox.
func ShouldFullRefresh(r, screen image.Rectangle) bool { return shouldFullRefresh(r, screen) }

// DumpFrame captures the full screen once and writes it as a PNG.
func (c *Capturer) DumpFrame(path string) error {
	img, err := c.Capture(c.ScreenRect())
	if err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

// Changed is signaled (coalesced) whenever new damage is pending.
func (c *Capturer) Changed() <-chan struct{} { return c.changed }

// Resized is signaled when RANDR reports a screen change.
func (c *Capturer) Resized() <-chan struct{} { return c.resized }

// TakePending swaps out the accumulated damage bounding box. Root mode
// yields zero or one rect; the multi-window Scene yields one per dirty
// window.
func (c *Capturer) TakePending() []image.Rectangle {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.pending
	c.pending = image.Rectangle{}
	if r.Empty() {
		return nil
	}
	return []image.Rectangle{r}
}

// allocBuffers (re)sizes the scratch canvas and SHM segment for the
// current screen size. Called from New and ApplyResize.
func (c *Capturer) allocBuffers() error {
	w, h := c.xc.ScreenSize()
	size := int(w) * int(h) * 4
	c.scratch = image.NewRGBA(image.Rect(0, 0, int(w), int(h)))

	c.releaseSegment()
	if c.xc.Ext.SHM {
		seg, err := NewSHMSegment(size)
		if err != nil {
			c.log.Warn("SHM segment unavailable; using core GetImage fallback", "err", err)
			return nil
		}
		segID, err := c.xc.X.NewId()
		if err != nil {
			_ = seg.Close()
			return fmt.Errorf("capture: xid for shm segment: %w", err)
		}
		if err := shm.AttachChecked(c.xc.X, shm.Seg(segID), uint32(seg.ID()), false).Check(); err != nil {
			_ = seg.Close()
			c.log.Warn("X rejected SHM attach; using core GetImage fallback", "err", err)
			return nil
		}
		c.seg, c.segID, c.hasSHM = seg, shm.Seg(segID), true
	} else {
		c.log.Warn("MIT-SHM extension missing; using core GetImage fallback")
	}
	return nil
}

func (c *Capturer) releaseSegment() {
	if c.segID != 0 {
		_ = shm.DetachChecked(c.xc.X, c.segID).Check()
		c.segID = 0
	}
	if c.seg != nil {
		_ = c.seg.Close()
		c.seg = nil
	}
	c.hasSHM = false
}

// Capture reads r (clamped to the screen) into the scratch canvas and
// returns the subimage holding it. The result is only valid until the
// next Capture call — encode it first. Must be called from the capture
// loop goroutine only.
func (c *Capturer) Capture(r image.Rectangle) (image.Image, error) {
	r = clampRect(r, c.ScreenRect())
	if r.Empty() {
		return nil, fmt.Errorf("capture: empty rect after clamp (%v)", r)
	}
	// Clear accumulated damage *before* reading pixels: damage arriving
	// during the capture then survives to trigger the next frame. Requests
	// on one connection are ordered, so this is race-free.
	if c.hasDamage {
		damage.Subtract(c.xc.X, c.dmg, 0, 0) // 0 = None: clear accumulated damage
	}
	w, h := r.Dx(), r.Dy()

	if c.hasSHM {
		cookie := shm.GetImage(c.xc.X, xproto.Drawable(c.xc.Root()),
			int16(r.Min.X), int16(r.Min.Y), uint16(w), uint16(h),
			0xFFFFFFFF, xproto.ImageFormatZPixmap, c.segID, 0)
		reply, err := cookie.Reply()
		if err == nil && (reply.Depth == 24 || reply.Depth == 32) {
			sub := c.scratch.SubImage(r).(*image.RGBA)
			if err := BGRAToRGBA(sub, c.seg.Mem(), w, h, c.xc.LSBFirst()); err != nil {
				return nil, err
			}
			return sub, nil
		}
		if err != nil && !c.shmWarned {
			c.shmWarned = true
			c.log.Warn("SHM GetImage failed; falling back to core GetImage", "err", err)
		}
	}

	// Core fallback, banded so each request stays far below the server's
	// 16 MiB request ceiling (a 4K frame is ~33 MiB).
	const maxBandBytes = 4 << 20
	bandH := maxBandBytes / (w * 4)
	if bandH < 1 {
		bandH = 1
	}
	for y0 := 0; y0 < h; y0 += bandH {
		bh := min(bandH, h-y0)
		reply, err := xproto.GetImage(c.xc.X, xproto.ImageFormatZPixmap,
			xproto.Drawable(c.xc.Root()),
			int16(r.Min.X), int16(r.Min.Y+y0), uint16(w), uint16(bh), 0xFFFFFFFF).Reply()
		if err != nil {
			return nil, fmt.Errorf("capture: GetImage(%v): %w", r, err)
		}
		band := c.scratch.SubImage(image.Rect(r.Min.X, r.Min.Y+y0, r.Min.X+w, r.Min.Y+y0+bh)).(*image.RGBA)
		if err := BGRAToRGBA(band, reply.Data, w, bh, c.xc.LSBFirst()); err != nil {
			return nil, err
		}
	}
	return c.scratch.SubImage(r).(*image.RGBA), nil
}

// ApplyResize re-reads the screen size after a RANDR notification and
// rebuilds the buffers and damage object. Must be called from the capture
// loop goroutine.
func (c *Capturer) ApplyResize() error {
	geom, err := xproto.GetGeometry(c.xc.X, xproto.Drawable(c.xc.Root())).Reply()
	if err != nil {
		return fmt.Errorf("capture: GetGeometry: %w", err)
	}
	c.xc.SetScreenSize(geom.Width, geom.Height)
	if c.hasDamage {
		damage.Destroy(c.xc.X, c.dmg)
		c.hasDamage = false
	}
	if err := c.allocBuffers(); err != nil {
		return err
	}
	if c.xc.Ext.DAMAGE {
		if err := c.initDamage(); err != nil {
			return fmt.Errorf("capture: damage re-init: %w", err)
		}
	}
	w, h := c.xc.ScreenSize()
	c.log.Info("screen resized", "width", w, "height", h)
	return nil
}

// ResizeTo resizes the shared screen — the whole desktop — to w×h. The
// request itself is RANDR best effort (see xwin.ResizeScreen): when the
// display accepts it the change comes back through the ordinary RANDR
// notification, which rebuilds the buffers and broadcasts ScreenResize
// plus a keyframe; when it refuses (an Xvfb screen can never be
// resized), the error says why and the viewer keeps letterboxing.
// Capture-loop only.
func (c *Capturer) ResizeTo(w, h int) error {
	aw, ah, err := c.xq.ResizeScreen(w, h)
	if err != nil {
		return err
	}
	if aw != w || ah != h {
		return fmt.Errorf("capture: display limited the screen to %dx%d (asked for %dx%d)",
			aw, ah, w, h)
	}
	return nil
}

// Close releases the SHM segment and damage object. It does not close the
// X connection; the owner does that after all users are done.
func (c *Capturer) Close() error {
	c.releaseSegment()
	if c.hasDamage {
		damage.Destroy(c.xc.X, c.dmg)
		c.hasDamage = false
	}
	return nil
}

// pump is the single goroutine allowed to call WaitForEvent.
func (c *Capturer) pump() {
	for {
		ev, err := c.xc.X.WaitForEvent()
		if err != nil {
			return // connection closed
		}
		if ev == nil && err == nil {
			return // both nil: connection closed (xgb contract)
		}
		c.HandleEvent(ev)
	}
}

func (c *Capturer) signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
