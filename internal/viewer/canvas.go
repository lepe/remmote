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
// reader and read by the blitter.
type Canvas struct {
	mu  sync.RWMutex
	img *image.RGBA
}

// NewCanvas creates a canvas of the server screen size.
func NewCanvas(w, h int) *Canvas {
	return &Canvas{img: image.NewRGBA(image.Rect(0, 0, w, h))}
}

// Resize reallocates for a new server screen size and clears it black.
func (c *Canvas) Resize(w, h int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.img = image.NewRGBA(image.Rect(0, 0, w, h))
}

// Size returns the canvas dimensions.
func (c *Canvas) Size() (int, int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.img.Rect.Dx(), c.img.Rect.Dy()
}

// Composite draws img at rect r (in canvas coordinates), clipped.
func (c *Canvas) Composite(r image.Rectangle, img image.Image) {
	c.mu.Lock()
	defer c.mu.Unlock()
	draw.Draw(c.img, r.Intersect(c.img.Rect), img, r.Min, draw.Src)
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
