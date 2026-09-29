package encode

import (
	"bytes"
	"github.com/lepe/remmote/internal/proto"
	"image"
	"testing"
)

func BenchmarkZRAW(b *testing.B) {
	for _, tc := range []struct {
		name string
		img  *image.RGBA
	}{
		{"desktop1080p", flat(1920, 1080)},
		{"delta", flat(1920, 1080).SubImage(image.Rect(17, 23, 337, 223)).(*image.RGBA)},
		{"noise1080p", noise(1920, 1080)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			enc, err := newZRAW()
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(tc.img.Rect.Dx() * tc.img.Rect.Dy() * 4))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, data, err := enc.Encode(tc.img, 75)
				if err != nil {
					b.Fatal(err)
				}
				b.ReportMetric(float64(len(data)), "encoded-B/op")
			}
		})
	}
}

func TestEncodedPayloadSurvivesReuse(t *testing.T) {
	for _, codec := range []uint8{proto.CodecZRAW, CodecHybrid} {
		enc, err := New(codec)
		if err != nil {
			t.Fatal(err)
		}
		src := flat(640, 480).SubImage(image.Rect(7, 11, 327, 211)).(*image.RGBA)
		kind, data, err := enc.Encode(src, 75)
		if err != nil {
			t.Fatal(err)
		}
		saved := bytes.Clone(data)
		for _, img := range []*image.RGBA{noise(640, 480), flat(100, 100), src} {
			if _, _, err := enc.Encode(img, 75); err != nil {
				t.Fatal(err)
			}
		}
		if !bytes.Equal(data, saved) {
			t.Fatal("later encode mutated queued payload")
		}
		if kind != proto.CodecZRAW {
			t.Fatal("desktop did not use ZRAW")
		}
		pixels := zstdDecode(t, data, 320*200*4)
		for y := 0; y < 200; y++ {
			if !bytes.Equal(pixels[y*1280:(y+1)*1280], src.Pix[y*src.Stride:y*src.Stride+1280]) {
				t.Fatalf("row %d differs", y)
			}
		}
	}
}
