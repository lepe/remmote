// Package viewer owns the client's X11 window: a server-resolution canvas
// composited from decoded rect updates, a scale-to-fit blitter drawing it
// into the window via MIT-SHM PutImage, and the event pump that turns
// local keyboard/pointer input into remmote protocol messages.
package viewer

import (
	"image"
	"image/draw"
	"image/png"
	"os"
	"sync"
)

// Canvas is the server-resolution framebuffer, composited by the network
// reader and read by the blitter. It tracks the union of composited rects
// since the last TakeDirty so the blitter can redraw only what changed.
type Canvas struct {
	mu    sync.RWMutex
	img   *image.RGBA
	dirty image.Rectangle
}

// NewCanvas creates a canvas of the server screen size.
func NewCanvas(w, h int) *Canvas {
	c := &Canvas{img: image.NewRGBA(image.Rect(0, 0, w, h))}
	c.dirty = c.img.Rect // first draw is a full draw
	return c
}

// Resize reallocates for a new server screen size and clears it black.
func (c *Canvas) Resize(w, h int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.img = image.NewRGBA(image.Rect(0, 0, w, h))
	c.dirty = c.img.Rect
}

// Size returns the canvas dimensions.
func (c *Canvas) Size() (int, int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.img.Rect.Dx(), c.img.Rect.Dy()
}

// Composite draws img at rect r (in canvas coordinates), clipped. img is
// the decoded rect payload: its bounds are 0-based, so the source point is
// img.Bounds().Min — never r.Min (which lives in canvas coordinates).
func (c *Canvas) Composite(r image.Rectangle, img image.Image) {
	c.mu.Lock()
	defer c.mu.Unlock()
	clip := r.Intersect(c.img.Rect)
	if clip.Empty() {
		return
	}
	draw.Draw(c.img, clip, img, img.Bounds().Min.Add(clip.Min.Sub(r.Min)), draw.Src)
	c.dirty = c.dirty.Union(clip)
}

// TakeDirty returns the union of everything composited since the last
// call and resets it. The blitter redraws exactly this region.
func (c *Canvas) TakeDirty() image.Rectangle {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.dirty
	c.dirty = image.Rectangle{}
	return r
}

// markDirty re-arms a region the blitter consumed but failed to draw.
func (c *Canvas) markDirty(r image.Rectangle) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dirty = c.dirty.Union(r)
}

// Snapshot writes the current canvas to path as PNG.
func (c *Canvas) Snapshot(path string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, c.img)
}

// withView runs fn with a read-locked view of the canvas; fn must not
// retain the image beyond the call.
func (c *Canvas) withView(fn func(*image.RGBA)) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	fn(c.img)
}
