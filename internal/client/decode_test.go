package client

import (
	"bytes"
	"image"
	"image/color"
	"runtime"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/lepe/remmote/internal/encode"
	"github.com/lepe/remmote/internal/proto"
)

func flatImg(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0x40, A: 0xFF})
		}
	}
	return img
}

// ZRAW payloads must decode back to the exact pixel bytes, and the
// buffer must survive repeated decodes (the reader reuses it).
func TestDecoderZRAW(t *testing.T) {
	enc, err := encode.New(proto.CodecZRAW)
	if err != nil {
		t.Fatal(err)
	}
	src := flatImg(64, 48)
	_, data, err := enc.Encode(src, 75)
	if err != nil {
		t.Fatal(err)
	}

	d := newDecoder()
	defer d.close()
	for i := 0; i < 3; i++ {
		img, err := d.decode(proto.CodecZRAW, data, 64, 48)
		if err != nil {
			t.Fatalf("decode %d: %v", i, err)
		}
		rgba, ok := img.(*image.RGBA)
		if !ok {
			t.Fatalf("got %T", img)
		}
		if rgba.Rect != src.Rect || rgba.Stride != src.Stride {
			t.Fatalf("geom %v stride %d", rgba.Rect, rgba.Stride)
		}
		if !bytes.Equal(rgba.Pix, src.Pix) {
			t.Fatalf("round trip mismatch on decode %d", i)
		}
	}
}

func TestDecoderErrors(t *testing.T) {
	d := newDecoder()
	defer d.close()
	if _, err := d.decode(proto.CodecZRAW, []byte("garbage"), 8, 8); err == nil {
		t.Fatal("expected error for corrupt zraw")
	}
	if _, err := d.decode(42, nil, 8, 8); err == nil {
		t.Fatal("expected error for unknown codec")
	}
	enc, _ := encode.New(proto.CodecZRAW)
	_, data, _ := enc.Encode(flatImg(4, 4), 75)
	if _, err := d.decode(proto.CodecZRAW, data, 5, 5); err == nil {
		t.Fatal("expected size-mismatch error")
	}
}

// JPEG must keep decoding through the same path.
func TestDecoderJPEG(t *testing.T) {
	enc, err := encode.New(proto.CodecJPEG)
	if err != nil {
		t.Fatal(err)
	}
	_, data, err := enc.Encode(flatImg(32, 32), 90)
	if err != nil {
		t.Fatal(err)
	}
	d := newDecoder()
	defer d.close()
	img, err := d.decode(proto.CodecJPEG, data, 32, 32)
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != 32 || img.Bounds().Dy() != 32 {
		t.Fatalf("bounds %v", img.Bounds())
	}
}

// w and h arrive from the wire as uint16 and a ZRAW frame decompresses
// by orders of magnitude, so the claimed rect size must be bounded
// *before* DecodeAll allocates — otherwise a corrupt or hostile stream
// exhausts the viewer's memory (measured: a 106 KB payload grew the heap
// by 1.3 GiB before failing).
func TestDecoderRejectsOversizedRectBeforeAllocating(t *testing.T) {
	enc, err := encode.New(proto.CodecZRAW)
	if err != nil {
		t.Fatal(err)
	}
	// A small, entirely valid payload ...
	_, small, err := enc.Encode(flatImg(4, 4), 75)
	if err != nil {
		t.Fatal(err)
	}
	d := newDecoder()
	defer d.close()

	// ... must still decode at its true size.
	if _, err := d.decode(proto.CodecZRAW, small, 4, 4); err != nil {
		t.Fatalf("legit frame rejected: %v", err)
	}

	// A real bomb: 72 MiB of zeros compresses to a tiny legal payload.
	zr, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		t.Fatal(err)
	}
	var bomb bytes.Buffer
	zr.Reset(&bomb)
	chunk := make([]byte, 1<<20)
	for i := 0; i < 72; i++ {
		if _, err := zr.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := zr.Close(); err != nil {
		t.Fatal(err)
	}
	zr.Close()
	if bomb.Len() > 1<<20 {
		t.Fatalf("bomb payload %d bytes, want a small one", bomb.Len())
	}

	// Claimed rect: 8192×2049 px = 64 MiB + 4 KiB, just over the cap.
	const w, h = 8192, 2049
	if int64(w)*int64(h)*4 <= maxZRAWBytes {
		t.Fatalf("test rect is not over the cap: %d", int64(w)*int64(h)*4)
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if _, err := d.decode(proto.CodecZRAW, bomb.Bytes(), w, h); err == nil {
		t.Fatalf("%dx%d: expected rejection", w, h)
	}
	runtime.ReadMemStats(&after)
	// Without the pre-check this frame decompresses to 72 MiB before the
	// size check rejects it.
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
		t.Errorf("rejected frame allocated %d bytes; the size bound is not applied before DecodeAll", grew)
	}

	// Degenerate rects are rejected too (and never index an empty slice).
	for _, tc := range [][2]int{{0, 8}, {8, 0}, {0, 0}} {
		if _, err := d.decode(proto.CodecZRAW, small, tc[0], tc[1]); err == nil {
			t.Errorf("%dx%d: expected rejection", tc[0], tc[1])
		}
	}
}
