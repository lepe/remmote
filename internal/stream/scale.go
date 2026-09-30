package stream

import (
	"fmt"
	"image"
	"image/png"
	"os"
)

// scaledSource reduces the wire framebuffer without changing the capture
// backend's window geometry. Delta edges round out to the same sampling grid
// as keyframes, so partial updates never shift or leave stale edge pixels.
type scaledSource struct {
	StreamSource
	factor int
	pixels *image.RGBA // capture-loop owned
}

func (s *scaledSource) ScreenRect() image.Rectangle {
	r := s.StreamSource.ScreenRect()
	return image.Rect(0, 0, (r.Dx()+s.factor-1)/s.factor, (r.Dy()+s.factor-1)/s.factor)
}

func (s *scaledSource) TakePending() []image.Rectangle {
	rects := s.StreamSource.TakePending()
	bounds := s.StreamSource.ScreenRect()
	out := rects[:0]
	for _, r := range rects {
		r = r.Intersect(bounds)
		if !r.Empty() {
			out = append(out, image.Rect(r.Min.X/s.factor, r.Min.Y/s.factor, (r.Max.X+s.factor-1)/s.factor, (r.Max.Y+s.factor-1)/s.factor))
		}
	}
	return out
}

func (s *scaledSource) Capture(r image.Rectangle) (image.Image, error) {
	r = r.Intersect(s.ScreenRect())
	if r.Empty() {
		return nil, fmt.Errorf("scale: empty capture rectangle")
	}
	sourceRect := image.Rect(r.Min.X*s.factor, r.Min.Y*s.factor, r.Max.X*s.factor, r.Max.Y*s.factor).Intersect(s.StreamSource.ScreenRect())
	img, err := s.StreamSource.Capture(sourceRect)
	if err != nil {
		return nil, err
	}
	src, ok := img.(*image.RGBA)
	if !ok {
		return nil, fmt.Errorf("scale: expected RGBA, got %T", img)
	}
	size := r.Dx() * r.Dy() * 4
	if s.pixels == nil || cap(s.pixels.Pix) < size {
		s.pixels = &image.RGBA{Pix: make([]byte, size)}
	}
	s.pixels.Pix = s.pixels.Pix[:size]
	s.pixels.Stride = r.Dx() * 4
	s.pixels.Rect = r
	for y := 0; y < r.Dy(); y++ {
		// Capture returns source-coordinate bounds, including subimages.
		row := src.Pix[src.PixOffset(sourceRect.Min.X, sourceRect.Min.Y+y*s.factor):]
		dst := s.pixels.Pix[y*s.pixels.Stride : (y+1)*s.pixels.Stride]
		for x := 0; x < r.Dx(); x++ {
			copy(dst[x*4:x*4+4], row[x*s.factor*4:x*s.factor*4+4])
		}
	}
	return s.pixels, nil
}

func (s *scaledSource) DumpFrame(path string) error {
	img, err := s.Capture(s.ScreenRect())
	if err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}
