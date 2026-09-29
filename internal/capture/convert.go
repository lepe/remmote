package capture

import (
	"fmt"
	"image"
	"unsafe"
)

// ZPixmap 4-bytes-per-pixel byte layout, per the X server's ImageByteOrder
// (depth 24 and 32 both scan out as 4 bytes/pixel, 32-bit aligned):
//
//	LSBFirst: [B, G, R, X]
//	MSBFirst: [X, R, G, B]
//
// Go's image.RGBA is always [R, G, B, A] in memory.

// hostLE reports whether the host is little-endian; the word-wise fast
// paths below assume it. Big-endian hosts use the byte loops.
var hostLE = func() bool {
	var x uint16 = 1
	return *(*byte)(unsafe.Pointer(&x)) == 1
}()

// words reinterprets b (len divisible by 4, 4-byte aligned) as uint32s.
// All callers pass image.RGBA rows, SHM segment memory, or xgb reply
// buffers — all 4-byte aligned by construction. An empty b yields nil
// rather than panicking on b[0].
func words(b []byte) []uint32 {
	if len(b) == 0 {
		return nil
	}
	return unsafe.Slice((*uint32)(unsafe.Pointer(&b[0])), len(b)/4)
}

// BGRAToRGBA converts a packed src (w*h*4 bytes, row stride 4*w) into dst,
// honoring dst.Stride so a SubImage of a larger canvas works. Alpha is set
// to 0xFF (the wire codecs are opaque).
func BGRAToRGBA(dst *image.RGBA, src []byte, w, h int, lsbFirst bool) error {
	if w <= 0 || h <= 0 {
		return nil // empty rect: nothing to convert (words() would panic)
	}
	if len(src) < w*h*4 {
		return fmt.Errorf("convert: src %d bytes, need %d", len(src), w*h*4)
	}
	if dst.Rect.Dx() < w || dst.Rect.Dy() < h {
		return fmt.Errorf("convert: dst %v smaller than %dx%d", dst.Rect, w, h)
	}
	for row := 0; row < h; row++ {
		s := src[row*w*4:]
		d := dst.Pix[row*dst.Stride:]
		if hostLE {
			sw, dw := words(s[:w*4]), words(d[:w*4])
			if lsbFirst {
				bgraSwapWords(dw, sw)
			} else {
				for i, v := range sw {
					dw[i] = v>>8 | 0xFF000000
				}
			}
			continue
		}
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
	if w <= 0 || h <= 0 {
		return nil // empty rect: nothing to convert (words() would panic)
	}
	if len(dst) < w*h*4 {
		return fmt.Errorf("convert: dst %d bytes, need %d", len(dst), w*h*4)
	}
	if src.Rect.Dx() < w || src.Rect.Dy() < h {
		return fmt.Errorf("convert: src %v smaller than %dx%d", src.Rect, w, h)
	}
	for row := 0; row < h; row++ {
		s := src.Pix[row*src.Stride:]
		d := dst[row*w*4:]
		rgbaRowToBGRA(d, s, w, lsbFirst)
	}
	return nil
}

// RegionToBGRA converts src's region r into dst, packing rows of r.Dx()
// pixels dstStride bytes apart. dstStride must be at least r.Dx()*4 — a
// smaller stride would make consecutive rows overlap and silently
// corrupt the last pixel of each row. dst must hold
// dstStride*(r.Dy()-1)+r.Dx()*4 bytes. It feeds the client blit path when
// only a sub-rect of the window changes: convert just those rows, in
// place, straight into SHM memory.
func RegionToBGRA(dst []byte, dstStride int, src *image.RGBA, r image.Rectangle, lsbFirst bool) error {
	w, h := r.Dx(), r.Dy()
	if w <= 0 || h <= 0 {
		return nil
	}
	if !r.In(src.Rect) {
		return fmt.Errorf("convert: region %v outside src %v", r, src.Rect)
	}
	if dstStride < w*4 {
		return fmt.Errorf("convert: dst stride %d < row %d bytes", dstStride, w*4)
	}
	if len(dst) < dstStride*(h-1)+w*4 {
		return fmt.Errorf("convert: dst %d bytes, need %d", len(dst), dstStride*(h-1)+w*4)
	}
	for row := 0; row < h; row++ {
		sy := r.Min.Y + row
		s := src.Pix[src.PixOffset(r.Min.X, sy):]
		d := dst[row*dstStride:]
		rgbaRowToBGRA(d, s, w, lsbFirst)
	}
	return nil
}

// rgbaRowToBGRA swaps R/B and forces alpha on w pixels of one row.
func rgbaRowToBGRA(d, s []byte, w int, lsbFirst bool) {
	if hostLE {
		sw, dw := words(s[:w*4]), words(d[:w*4])
		if lsbFirst {
			bgraSwapWords(dw, sw)
		} else {
			for i, v := range sw {
				dw[i] = v<<8 | 0xFF
			}
		}
		return
	}
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

// bgraSwapWords swaps the R and B bytes of every 32-bit pixel and forces
// alpha to 0xFF. It is the LSBFirst transform in both directions (an
// involution): [B,G,R,X] ↔ [R,G,B,0xFF].
func bgraSwapWords(dst, src []uint32) {
	for i, v := range src {
		dst[i] = v&0x0000FF00 | (v&0xFF)<<16 | (v>>16)&0xFF | 0xFF000000
	}
}
