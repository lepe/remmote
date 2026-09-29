package viewer

import (
	"image"
	"image/color"
	"testing"
)

// Regression: decoded rect payloads are 0-based images; compositing a
// delta at a non-zero canvas origin must draw the payload's top-left at
// the rect's top-left (previously sp=r.Min drew the wrong source pixels
// or nothing at all, so only full-screen keyframes ever appeared).
func TestCompositeNonZeroOrigin(t *testing.T) {
	c := NewCanvas(64, 64)
	_ = c.TakeDirty() // clear the initial full dirty

	tile := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for i := 0; i < len(tile.Pix); i += 4 {
		tile.Pix[i], tile.Pix[i+1], tile.Pix[i+2], tile.Pix[i+3] = 0, 0xFF, 0, 0xFF
	}
	tile.Pix[0], tile.Pix[1], tile.Pix[2] = 0xFF, 0, 0 // red top-left

	r := image.Rect(10, 10, 26, 26)
	c.Composite(r, tile)

	var px [3]color.RGBA
	c.withView(func(img *image.RGBA) {
		px[0] = img.RGBAAt(10, 10) // want red (tile top-left)
		px[1] = img.RGBAAt(20, 20) // want green (tile interior)
		px[2] = img.RGBAAt(30, 30) // want untouched (outside rect)
	})
	if px[0] != (color.RGBA{0xFF, 0, 0, 0xFF}) {
		t.Fatalf("(10,10) = %v, want red", px[0])
	}
	if px[1] != (color.RGBA{0, 0xFF, 0, 0xFF}) {
		t.Fatalf("(20,20) = %v, want green", px[1])
	}
	if px[2] != (color.RGBA{}) {
		t.Fatalf("(30,30) = %v, want untouched zero", px[2])
	}
}

// Dirty tracking: unions composited rects, resets on take.
func TestDirtyTracking(t *testing.T) {
	c := NewCanvas(64, 64)
	if got := c.TakeDirty(); got != image.Rect(0, 0, 64, 64) {
		t.Fatalf("initial dirty = %v, want full canvas", got)
	}
	if got := c.TakeDirty(); !got.Empty() {
		t.Fatalf("dirty after take = %v, want empty", got)
	}
	tile := image.NewRGBA(image.Rect(0, 0, 4, 4))
	c.Composite(image.Rect(10, 10, 14, 14), tile)
	c.Composite(image.Rect(20, 5, 24, 9), tile)
	if got := c.TakeDirty(); got != image.Rect(10, 5, 24, 14) {
		t.Fatalf("dirty union = %v, want (10,5)-(24,14)", got)
	}
	if got := c.TakeDirty(); !got.Empty() {
		t.Fatalf("dirty after take = %v, want empty", got)
	}
	// Resize marks the whole canvas dirty again.
	c.Resize(32, 32)
	if got := c.TakeDirty(); got != image.Rect(0, 0, 32, 32) {
		t.Fatalf("dirty after resize = %v, want full", got)
	}
}

func TestCompositeClippedSource(t *testing.T) {
	c := NewCanvas(4, 4)
	tile := image.NewRGBA(image.Rect(10, 20, 14, 24))
	for y := 20; y < 24; y++ {
		for x := 10; x < 14; x++ {
			tile.SetRGBA(x, y, color.RGBA{byte(x), byte(y), 0, 255})
		}
	}
	c.Composite(image.Rect(-2, -1, 2, 3), tile)
	c.withView(func(img *image.RGBA) {
		for y := 0; y < 3; y++ {
			for x := 0; x < 2; x++ {
				if got, want := img.RGBAAt(x, y), tile.RGBAAt(x+12, y+21); got != want {
					t.Fatalf("(%d,%d): %v != %v", x, y, got, want)
				}
			}
		}
	})
}
