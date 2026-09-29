package capture

import (
	"image"
	"testing"
)

func BenchmarkBGRAToRGBA(b *testing.B) {
	const w, h = 1920, 1080
	src := make([]byte, w*h*4)
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	b.SetBytes(w * h * 4)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := BGRAToRGBA(dst, src, w, h, true); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRGBAToBGRA(b *testing.B) {
	const w, h = 1920, 1080
	dst := make([]byte, w*h*4)
	src := image.NewRGBA(image.Rect(0, 0, w, h))
	b.SetBytes(w * h * 4)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := RGBAToBGRA(dst, src, w, h, true); err != nil {
			b.Fatal(err)
		}
	}
}
