package client

import (
	"image"

	xdraw "golang.org/x/image/draw"
)

// upscaler rebuilds host-resolution pixels from a stream the server
// downscaled. -upscale N is the client-side mirror of the server's
// -downscale N: every rect arrives N times smaller, so each decoded
// image is magnified back onto the host's pixel grid before it is
// composited and the canvas is sized like the host screen.
//
// Nearest-neighbour is deliberate, not a shortcut. The server samples
// the host on a fixed grid (pixel x*N, y*N), so replicating each sample
// into an N×N block is the exact inverse: every canvas pixel holds a
// colour the host really had, at the coordinate it really had it. A
// smoothing filter would invent colours and shift text that is already
// missing detail.
//
// The destination buffer is reused across rects — the network reader
// composites each image before decoding the next, so nothing retains it.
type upscaler struct {
	factor int
	buf    *image.RGBA
	row    []byte // scratch for one replicated source row
}

func newUpscaler(factor int) *upscaler {
	if factor < 1 {
		factor = 1
	}
	return &upscaler{factor: factor}
}

// size maps wire dimensions to host dimensions.
func (u *upscaler) size(w, h int) (int, int) { return w * u.factor, h * u.factor }

// rect maps a wire rectangle to the canvas rectangle it covers once
// magnified. Factor 1 returns r untouched.
func (u *upscaler) rect(r image.Rectangle) image.Rectangle {
	if u.factor == 1 {
		return r
	}
	return image.Rect(r.Min.X*u.factor, r.Min.Y*u.factor, r.Max.X*u.factor, r.Max.Y*u.factor)
}

// apply magnifies a decoded wire image and returns it with the canvas
// rect it covers. The two are returned together because they must agree:
// a rect sized for the un-magnified image would composite the wrong
// pixels. Factor 1 returns both arguments unchanged, so the common path
// copies nothing.
func (u *upscaler) apply(r image.Rectangle, img image.Image, w, h int) (image.Rectangle, image.Image) {
	if u.factor == 1 {
		return r, img
	}
	dw, dh := w*u.factor, h*u.factor
	if need := dw * dh * 4; u.buf == nil || cap(u.buf.Pix) < need {
		u.buf = image.NewRGBA(image.Rect(0, 0, dw, dh))
	}
	dst := u.buf
	dst.Rect = image.Rect(0, 0, dw, dh)
	dst.Stride = dw * 4
	dst.Pix = dst.Pix[:dw*dh*4]
	if !u.replicate(dst, img, w, h) {
		// Anything but a plain RGBA source (JPEG's YCbCr, WebP's NRGBA)
		// goes through the general scaler, which handles every source.
		xdraw.NearestNeighbor.Scale(dst, dst.Rect, img, img.Bounds(), xdraw.Src, nil)
	}
	return u.rect(r), dst
}

// replicate magnifies an RGBA source by whole-pixel block replication and
// reports whether it did. For an integer factor, nearest-neighbour *is*
// block replication, but the general scaler spends its time recomputing
// that mapping per pixel; doing it here costs one copy per source pixel
// and makes a full-screen keyframe about four times cheaper.
// TestUpscaleMatchesReferenceScaler pins the two to identical output.
func (u *upscaler) replicate(dst *image.RGBA, img image.Image, w, h int) bool {
	src, ok := img.(*image.RGBA)
	if !ok || w <= 0 || h <= 0 {
		return false
	}
	// The rect claims w×h, but a subimage view (the reused ZRAW buffer)
	// can be smaller, and its rows are Stride apart, not w*4 apart. Only
	// take the fast path when the source really holds the claimed pixels;
	// otherwise let the general scaler, which clips, handle it. The Pix
	// check also keeps a hand-built image with a lying Rect from turning
	// a bad frame into a panic mid-session.
	if src.Rect.Dx() < w || src.Rect.Dy() < h || src.Stride < w*4 ||
		src.Stride <= 0 || len(src.Pix) < src.Rect.Dy()*src.Stride {
		return false
	}
	dw := w * u.factor * 4 // destination row length in bytes
	if len(u.row) < dw {
		u.row = make([]byte, dw)
	}
	row := u.row[:dw]
	for y := 0; y < h; y++ {
		// One source row, bounded to the claimed width: a wider stride
		// means the tail belongs to the next row, not to this rect.
		off := src.PixOffset(src.Rect.Min.X, src.Rect.Min.Y+y)
		s := src.Pix[off : off+w*4]
		// Widen one source row into the factor-times-wider scratch row.
		for x := 0; x < w; x++ {
			for k := 0; k < u.factor; k++ {
				copy(row[(x*u.factor+k)*4:], s[x*4:x*4+4])
			}
		}
		// Then stamp that row down the factor destination rows.
		for dy := 0; dy < u.factor; dy++ {
			copy(dst.Pix[dst.PixOffset(0, y*u.factor+dy):][:dw], row)
		}
	}
	return true
}
