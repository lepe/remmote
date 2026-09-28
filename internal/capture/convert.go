package capture

import (
	"fmt"
	"image"
)

// ZPixmap 4-bytes-per-pixel byte layout, per the X server's ImageByteOrder
// (depth 24 and 32 both scan out as 4 bytes/pixel, 32-bit aligned):
//
//	LSBFirst: [B, G, R, X]
//	MSBFirst: [X, R, G, B]
//
// Go's image.RGBA is always [R, G, B, A] in memory.

// BGRAToRGBA converts a packed src (w*h*4 bytes, row stride 4*w) into dst,
// honoring dst.Stride so a SubImage of a larger canvas works. Alpha is set
// to 0xFF (the wire codecs are opaque).
func BGRAToRGBA(dst *image.RGBA, src []byte, w, h int, lsbFirst bool) error {
	if len(src) < w*h*4 {
		return fmt.Errorf("convert: src %d bytes, need %d", len(src), w*h*4)
	}
	if dst.Rect.Dx() < w || dst.Rect.Dy() < h {
		return fmt.Errorf("convert: dst %v smaller than %dx%d", dst.Rect, w, h)
	}
	for row := 0; row < h; row++ {
		s := src[row*w*4:]
		d := dst.Pix[row*dst.Stride:]
		if lsbFirst {
			for i := 0; i < w; i++ {
				p := i * 4
				d[p+0] = s[p+2] // R
				d[p+1] = s[p+1] // G
				d[p+2] = s[p+0] // B
				d[p+3] = 0xFF
			}
		} else {
			for i := 0; i < w; i++ {
				p := i * 4
				d[p+0] = s[p+1]
				d[p+1] = s[p+2]
				d[p+2] = s[p+3]
				d[p+3] = 0xFF
			}
		}
	}
	return nil
}

// RGBAToBGRA converts src (only its top-left w×h region is read) into a
// packed dst buffer (w*h*4 bytes) in the X server's byte order — the client
// blit path feeding PutImage.
func RGBAToBGRA(dst []byte, src *image.RGBA, w, h int, lsbFirst bool) error {
	if len(dst) < w*h*4 {
		return fmt.Errorf("convert: dst %d bytes, need %d", len(dst), w*h*4)
	}
	if src.Rect.Dx() < w || src.Rect.Dy() < h {
		return fmt.Errorf("convert: src %v smaller than %dx%d", src.Rect, w, h)
	}
	for row := 0; row < h; row++ {
		s := src.Pix[row*src.Stride:]
		d := dst[row*w*4:]
		if lsbFirst {
			for i := 0; i < w; i++ {
				p := i * 4
				d[p+0] = s[p+2] // B
				d[p+1] = s[p+1] // G
				d[p+2] = s[p+0] // R
				d[p+3] = 0xFF
			}
		} else {
			for i := 0; i < w; i++ {
				p := i * 4
				d[p+0] = 0xFF
				d[p+1] = s[p+0] // R
				d[p+2] = s[p+1] // G
				d[p+3] = s[p+2] // B
			}
		}
	}
	return nil
}
