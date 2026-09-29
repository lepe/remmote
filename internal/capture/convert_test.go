package capture

import (
	"bytes"
	"image"
	"image/color"
	"testing"
)

func TestBGRAToRGBAKnownBytes(t *testing.T) {
	// One pixel R=0x11 G=0x22 B=0x33.
	lsb := []byte{0x33, 0x22, 0x11, 0x00} // [B,G,R,X]
	msb := []byte{0x00, 0x11, 0x22, 0x33} // [X,R,G,B]

	dst := image.NewRGBA(image.Rect(0, 0, 1, 1))
	if err := BGRAToRGBA(dst, lsb, 1, 1, true); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dst.Pix, []byte{0x11, 0x22, 0x33, 0xFF}) {
		t.Fatalf("lsb pix = % x, want 11 22 33 ff", dst.Pix)
	}

	dst = image.NewRGBA(image.Rect(0, 0, 1, 1))
	if err := BGRAToRGBA(dst, msb, 1, 1, false); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dst.Pix, []byte{0x11, 0x22, 0x33, 0xFF}) {
		t.Fatalf("msb pix = % x, want 11 22 33 ff", dst.Pix)
	}
}

func TestRGBAToBGRAKnownBytes(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 1, 1))
	src.SetRGBA(0, 0, color.RGBA{R: 0x11, G: 0x22, B: 0x33, A: 0xFF})

	lsb := make([]byte, 4)
	if err := RGBAToBGRA(lsb, src, 1, 1, true); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(lsb, []byte{0x33, 0x22, 0x11, 0xFF}) {
		t.Fatalf("lsb = % x, want 33 22 11 ff", lsb)
	}

	msb := make([]byte, 4)
	if err := RGBAToBGRA(msb, src, 1, 1, false); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(msb, []byte{0xFF, 0x11, 0x22, 0x33}) {
		t.Fatalf("msb = % x, want ff 11 22 33", msb)
	}
}

// Round-trip through both byte orders must be lossless.
func TestConvertRoundTrip(t *testing.T) {
	const w, h = 17, 9 // odd width
	src := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			src.SetRGBA(x, y, color.RGBA{R: uint8(x * 13), G: uint8(y * 25), B: uint8(x + y), A: 0xFF})
		}
	}
	for _, lsb := range []bool{true, false} {
		buf := make([]byte, w*h*4)
		if err := RGBAToBGRA(buf, src, w, h, lsb); err != nil {
			t.Fatal(err)
		}
		back := image.NewRGBA(image.Rect(0, 0, w, h))
		if err := BGRAToRGBA(back, buf, w, h, lsb); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(back.Pix, src.Pix) {
			t.Fatalf("lsb=%v round trip mismatch", lsb)
		}
	}
}

// BGRAToRGBA must honor dst.Stride when dst is a SubImage of a larger
// canvas (the capture scratch-buffer pattern) and must not touch neighbors.
func TestBGRAToRGBAStride(t *testing.T) {
	big := image.NewRGBA(image.Rect(0, 0, 8, 4))              // stride 32
	sub := big.SubImage(image.Rect(2, 1, 7, 3)).(*image.RGBA) // 5x2 at (2,1)
	w, h := 5, 2
	src := make([]byte, w*h*4)
	for i := 0; i < w*h; i++ {
		src[i*4+0] = byte(i)     // B
		src[i*4+1] = byte(i + 1) // G
		src[i*4+2] = byte(i + 2) // R
		src[i*4+3] = 0x00
	}
	if err := BGRAToRGBA(sub, src, w, h, true); err != nil {
		t.Fatal(err)
	}
	k := 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			want := [4]byte{byte(k + 2), byte(k + 1), byte(k), 0xFF}
			got := big.RGBAAt(2+x, 1+y)
			if got != (color.RGBA{want[0], want[1], want[2], want[3]}) {
				t.Fatalf("pixel (%d,%d) = %v, want %v", x, y, got, want)
			}
			k++
		}
	}
	// Neighbors untouched (canvas starts zeroed).
	for y := 0; y < 4; y++ {
		for x := 0; x < 8; x++ {
			if x >= 2 && x < 7 && y >= 1 && y < 3 {
				continue
			}
			if c := big.RGBAAt(x, y); c != (color.RGBA{}) {
				t.Fatalf("neighbor (%d,%d) = %v, want zero", x, y, c)
			}
		}
	}
}

func TestConvertBounds(t *testing.T) {
	dst := image.NewRGBA(image.Rect(0, 0, 2, 2))
	if err := BGRAToRGBA(dst, make([]byte, 15), 2, 2, true); err == nil {
		t.Error("expected error for short src")
	}
	small := image.NewRGBA(image.Rect(0, 0, 1, 1))
	if err := BGRAToRGBA(small, make([]byte, 16), 2, 2, true); err == nil {
		t.Error("expected error for small dst")
	}
	if err := RGBAToBGRA(make([]byte, 8), dst, 2, 2, true); err == nil {
		t.Error("expected error for short dst")
	}
}

// RegionToBGRA is the client blit's sub-rect path: the window rows it
// converts land at dstStride apart inside a full-window SHM segment, so
// both the pixel order and the row placement must be exact.
func TestRegionToBGRA(t *testing.T) {
	const W, H = 9, 7
	src := image.NewRGBA(image.Rect(0, 0, W, H))
	for y := 0; y < H; y++ {
		for x := 0; x < W; x++ {
			src.SetRGBA(x, y, color.RGBA{R: uint8(x + 1), G: uint8(y + 1), B: uint8(x + y), A: 0xFF})
		}
	}
	r := image.Rect(2, 1, 7, 5) // 5x4 region
	w, h := r.Dx(), r.Dy()
	const stride = 24 // > 20-byte rows: padding between rows, like a segment

	for _, lsb := range []bool{true, false} {
		dst := make([]byte, stride*(h-1)+w*4)
		for i := range dst { // poison padding so an over-write is visible
			dst[i] = 0xEE
		}
		if err := RegionToBGRA(dst, stride, src, r, lsb); err != nil {
			t.Fatalf("lsb=%v: %v", lsb, err)
		}
		for row := 0; row < h; row++ {
			got := dst[row*stride : row*stride+w*4]
			rowRect := image.Rect(r.Min.X, r.Min.Y+row, r.Max.X, r.Min.Y+row+1)
			want := make([]byte, w*4)
			if err := RGBAToBGRA(want, src.SubImage(rowRect).(*image.RGBA), w, 1, lsb); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("lsb=%v row %d = % x, want % x", lsb, row, got, want)
			}
			if row < h-1 {
				for i, b := range dst[row*stride+w*4 : (row+1)*stride] {
					if b != 0xEE {
						t.Fatalf("lsb=%v row %d padding byte %d = %#x, want untouched", lsb, row, i, b)
					}
				}
			}
		}
	}
}

func TestRegionToBGRAErrors(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 8, 8))
	r := image.Rect(2, 2, 6, 6) // 4x4 → needs stride*3 + 16 bytes
	if err := RegionToBGRA(make([]byte, 10), 16, src, r, true); err == nil {
		t.Error("expected error for short dst")
	}
	if err := RegionToBGRA(make([]byte, 256), 16, src, image.Rect(6, 6, 10, 10), true); err == nil {
		t.Error("expected error for region outside src")
	}
	// A stride below one row would make rows overlap and silently corrupt
	// the last pixel of every row — it must be rejected, not accepted.
	if err := RegionToBGRA(make([]byte, 256), 12, src, r, true); err == nil {
		t.Error("expected error for dst stride < row bytes")
	}
	// Empty rects are a no-op, not a panic (words() indexes b[0]).
	if err := RegionToBGRA(nil, 0, src, image.Rect(3, 3, 3, 5), true); err != nil {
		t.Errorf("empty region: %v", err)
	}
}

// Zero-width or zero-height rects must be no-ops across all three
// converters rather than panicking inside the word-wise fast path.
func TestConvertEmptyRectNoOp(t *testing.T) {
	dst := image.NewRGBA(image.Rect(0, 0, 4, 4))
	if err := BGRAToRGBA(dst, nil, 0, 4, true); err != nil {
		t.Errorf("BGRAToRGBA 0x4: %v", err)
	}
	if err := RGBAToBGRA(nil, dst, 4, 0, true); err != nil {
		t.Errorf("RGBAToBGRA 4x0: %v", err)
	}
	for i, b := range dst.Pix {
		if b != 0 {
			t.Fatalf("pixel byte %d = %#x, want untouched", i, b)
		}
	}
}

func TestRegionToBGRANonZeroImageOrigin(t *testing.T) {
	parent := image.NewRGBA(image.Rect(-10, -20, 30, 40))
	for y := -20; y < 40; y++ {
		for x := -10; x < 30; x++ {
			parent.SetRGBA(x, y, color.RGBA{byte(x + 10), byte(y + 20), 37, 255})
		}
	}
	src := parent.SubImage(image.Rect(-3, -7, 20, 30)).(*image.RGBA)
	r := image.Rect(1, 2, 8, 9)
	for _, lsb := range []bool{true, false} {
		got, want := make([]byte, r.Dx()*r.Dy()*4), make([]byte, r.Dx()*r.Dy()*4)
		if err := RegionToBGRA(got, r.Dx()*4, src, r, lsb); err != nil {
			t.Fatal(err)
		}
		if err := RGBAToBGRA(want, parent.SubImage(r).(*image.RGBA), r.Dx(), r.Dy(), lsb); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("incorrect subimage pixels (lsb=%v)", lsb)
		}
	}
}
