package encode

import (
	"image"
	"image/color"
	"testing"

	"github.com/lepe/remmote/internal/proto"
)

func gradient(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x * 255 / w), G: uint8(y * 255 / h), B: 0x80, A: 0xFF})
		}
	}
	return img
}

func TestJpegEncode(t *testing.T) {
	enc, err := New(proto.CodecJPEG)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if enc.Codec() != proto.CodecJPEG {
		t.Fatalf("codec = %d", enc.Codec())
	}
	for _, q := range []int{1, 50, 75, 100, 0, 150} {
		codec, data, err := enc.Encode(gradient(320, 200), q)
		if err != nil {
			t.Fatalf("quality %d: %v", q, err)
		}
		if codec != proto.CodecJPEG {
			t.Fatalf("quality %d: wire codec = %d", q, codec)
		}
		if len(data) == 0 || len(data) > proto.MaxPayload {
			t.Fatalf("quality %d: %d bytes", q, len(data))
		}
	}
}

func TestUnknownCodec(t *testing.T) {
	if _, err := New(9); err == nil {
		t.Fatal("expected error for unknown codec")
	}
}
