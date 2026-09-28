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
