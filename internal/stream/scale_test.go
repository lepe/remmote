package stream

import (
	"github.com/lepe/remmote/internal/encode"
	"image"
	"image/color"
	"testing"
)

type imageSource struct {
	StreamSource
	img     *image.RGBA
	pending []image.Rectangle
}

func (s *imageSource) ScreenRect() image.Rectangle                    { return s.img.Rect }
func (s *imageSource) Capture(r image.Rectangle) (image.Image, error) { return s.img.SubImage(r), nil }
func (s *imageSource) TakePending() []image.Rectangle {
	return append([]image.Rectangle(nil), s.pending...)
}

func TestScaledSourceDeltaMatchesKeyframe(t *testing.T) {
	for _, factor := range []int{2, 4} {
		img := image.NewRGBA(image.Rect(0, 0, 37, 29))
		for y := 0; y < 29; y++ {
			for x := 0; x < 37; x++ {
				img.SetRGBA(x, y, color.RGBA{byte(x * 5), byte(y * 7), byte(x + y), 255})
			}
		}
		base := &imageSource{img: img, pending: []image.Rectangle{image.Rect(3, 5, 18, 21), image.Rect(35, 27, 40, 40), image.Rect(-4, -4, 1, 1)}}
		s := &scaledSource{StreamSource: base, factor: factor}
		want := image.Rect(0, 0, (37+factor-1)/factor, (29+factor-1)/factor)
		if s.ScreenRect() != want {
			t.Fatalf("screen %v != %v", s.ScreenRect(), want)
		}
		for _, r := range append(s.TakePending(), want) {
			got, err := s.Capture(r)
			if err != nil {
				t.Fatal(err)
			}
			for y := r.Min.Y; y < r.Max.Y; y++ {
				for x := r.Min.X; x < r.Max.X; x++ {
					if got.At(x, y) != img.At(x*factor, y*factor) {
						t.Fatalf("factor %d pixel %d,%d mismatch", factor, x, y)
					}
				}
			}
		}
		if _, err := s.Capture(image.Rect(100, 100, 101, 101)); err == nil {
			t.Fatal("accepted empty capture")
		}
	}
}

func BenchmarkScaledEncode(b *testing.B) {
	img := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
	for y := 0; y < 1080; y++ {
		for x := 0; x < 1920; x++ {
			i := y*img.Stride + x*4
			v := byte(240)
			if y%20 < 10 && x%7 < 4 {
				v = 30
			}
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = v, v, v, 255
		}
	}
	for _, tc := range []struct {
		name   string
		factor int
	}{{"native", 1}, {"half", 2}, {"quarter", 4}} {
		b.Run(tc.name, func(b *testing.B) {
			var src StreamSource = &imageSource{img: img}
			if tc.factor > 1 {
				src = &scaledSource{StreamSource: src, factor: tc.factor}
			}
			enc, err := encode.New(encode.CodecHybrid)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				frame, err := src.Capture(src.ScreenRect())
				if err != nil {
					b.Fatal(err)
				}
				_, data, err := enc.Encode(frame, 75)
				if err != nil {
					b.Fatal(err)
				}
				b.ReportMetric(float64(len(data)), "encoded-B/op")
			}
		})
	}
}
