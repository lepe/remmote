package encode

import (
	"bytes"
	"image"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/lepe/remmote/internal/proto"
)

// flat is desktop-like content: mostly flat colors with a few text-ish
// marks. It compresses far past the hybrid gate, so ZRAW wins.
func flat(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*img.Stride + x*4
			img.Pix[i+0], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = 240, 240, 240, 0xFF
			if y%20 < 10 && x%7 < 4 {
				img.Pix[i+0], img.Pix[i+1], img.Pix[i+2] = 30, 30, 30
			}
		}
	}
	return img
}

// noise is incompressible content: the hybrid encoder must fall back to
// JPEG to bound bandwidth.
func noise(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	seed := uint32(12345)
	for i := 0; i < len(img.Pix); i += 4 {
		seed = seed*1664525 + 1013904223
		img.Pix[i+0] = byte(seed >> 24)
		img.Pix[i+1] = byte(seed >> 16)
		img.Pix[i+2] = byte(seed >> 8)
		img.Pix[i+3] = 0xFF
	}
	return img
}

func zstdDecode(t *testing.T, data []byte, want int) []byte {
	t.Helper()
	dec, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := dec.DecodeAll(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != want {
		t.Fatalf("decompressed %d bytes, want %d", len(out), want)
	}
	return out
}

func TestZRAWRoundTrip(t *testing.T) {
	enc, err := New(proto.CodecZRAW)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	src := gradient(320, 200)
	codec, data, err := enc.Encode(src, 75)
	if err != nil {
		t.Fatal(err)
	}
	if codec != proto.CodecZRAW {
		t.Fatalf("wire codec = %d", codec)
	}
	out := zstdDecode(t, data, 320*200*4)
	if !bytes.Equal(out, src.Pix) {
		t.Fatal("round trip mismatch")
	}
}

// A subimage (stride > w*4) must pack rows correctly.
func TestZRAWSubImage(t *testing.T) {
	big := gradient(640, 480)
	sub := big.SubImage(image.Rect(7, 11, 327, 211)).(*image.RGBA)
	enc, err := newZRAW()
	if err != nil {
		t.Fatal(err)
	}
	_, data, err := enc.Encode(sub, 75)
	if err != nil {
		t.Fatal(err)
	}
	w, h := sub.Rect.Dx(), sub.Rect.Dy()
	out := zstdDecode(t, data, w*h*4)
	for row := 0; row < h; row++ {
		want := sub.Pix[row*sub.Stride : row*sub.Stride+w*4]
		if !bytes.Equal(out[row*w*4:(row+1)*w*4], want) {
			t.Fatalf("row %d mismatch", row)
		}
	}
}

// Flat desktop content must pick ZRAW; incompressible content (and
// gradients, which zstd handles poorly but JPEG handles well) pick JPEG.
func TestHybridPicksPerContent(t *testing.T) {
	enc, err := New(CodecHybrid)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if enc.Codec() != CodecHybrid {
		t.Fatalf("codec = %d", enc.Codec())
	}

	codec, data, err := enc.Encode(flat(320, 200), 75)
	if err != nil {
		t.Fatal(err)
	}
	if codec != proto.CodecZRAW {
		t.Fatalf("flat: codec = %s, want zraw", proto.CodecName(codec))
	}
	zstdDecode(t, data, 320*200*4)

	for _, tc := range []struct {
		name string
		img  *image.RGBA
	}{
		{"noise", noise(320, 200)},
		{"gradient", gradient(320, 200)},
	} {
		codec, _, err := enc.Encode(tc.img, 75)
		if err != nil {
			t.Fatal(err)
		}
		if codec != proto.CodecJPEG {
			t.Fatalf("%s: codec = %s, want jpeg", tc.name, proto.CodecName(codec))
		}
	}
}
