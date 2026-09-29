package encode

import (
	"bytes"
	"image"
	"image/jpeg"

	"github.com/lepe/remmote/internal/proto"
)

// Jpeg is the pure-Go default encoder.
type Jpeg struct{}

func newJPEG() (Encoder, error) { return Jpeg{}, nil }

func (Jpeg) Codec() uint8 { return proto.CodecJPEG }

func (Jpeg) Encode(img image.Image, quality int) (uint8, []byte, error) {
	q := clampQuality(quality)
	// One size backoff: huge rects (4K keyframes) can exceed the wire limit
	// at high quality; halving quality once is always enough headroom.
	for {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
			return 0, nil, err
		}
		if buf.Len() <= proto.MaxPayload || q <= 5 {
			return proto.CodecJPEG, buf.Bytes(), nil
		}
		q /= 2
	}
}

func clampQuality(q int) int {
	if q < 1 {
		return 1
	}
	if q > 100 {
		return 100
	}
	return q
}
