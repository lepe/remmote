//go:build webp

package encode

import (
	"bytes"
	"image"

	"github.com/kolesa-team/go-webp/encoder"
	"github.com/kolesa-team/go-webp/webp"

	"github.com/lepe/remmote/internal/proto"
)

// WebP is the lossy libwebp encoder. Only compiled into `-tags webp`
// builds (CGo + libwebp-dev): libwebp encode calls are thread-safe, so
// the server's single capture-loop encode path needs no locking.
type WebP struct{}

func newWebP() (Encoder, error) { return WebP{}, nil }

func (WebP) Codec() uint8 { return proto.CodecWebP }

func (WebP) Encode(img image.Image, quality int) ([]byte, error) {
	q := clampQuality(quality)
	for {
		opts, err := encoder.NewLossyEncoderOptions(encoder.PresetDefault, float32(q))
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		if err := webp.Encode(&buf, img, opts); err != nil {
			return nil, err
		}
		if buf.Len() <= proto.MaxPayload || q <= 5 {
			return buf.Bytes(), nil
		}
		q /= 2
	}
}
