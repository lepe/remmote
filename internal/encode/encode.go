// Package encode abstracts the image codec used for rect updates. The
// default is Hybrid: zstd-compressed raw pixels (ZRAW) when they compress
// well — cheap enough that encode+decode nearly vanish next to JPEG — with
// a JPEG fallback for photographic content. JPEG is always available; a
// lossy WebP encoder exists in builds tagged `webp` (CGo + libwebp). The
// wire codec byte in proto.RectUpdate selects the decoder per rect, so one
// stream may mix codecs.
package encode

import (
	"fmt"
	"image"

	"github.com/lepe/remmote/internal/proto"
)

// Encoder encodes one image region at a quality level (1-100).
type Encoder interface {
	// Codec is the codec byte the encoder was configured for (logging,
	// startup banners). Per-rect wire bytes come from Encode.
	Codec() uint8
	// Encode returns the wire codec byte and compressed data for img at
	// the given quality. Hybrid encoders pick per rect.
	Encode(img image.Image, quality int) (codec uint8, data []byte, err error)
}

// New returns the encoder for a proto codec byte. It fails fast when a codec
// was not compiled into this build (e.g. -codec webp without -tags webp).
// proto.CodecZRAW selects the forced raw encoder; CodecHybrid picks per rect.
func New(codec uint8) (Encoder, error) {
	switch codec {
	case proto.CodecJPEG:
		return newJPEG()
	case proto.CodecWebP:
		return newWebP()
	case proto.CodecZRAW:
		return newZRAW()
	case CodecHybrid:
		return newHybrid()
	default:
		return nil, fmt.Errorf("encode: unknown codec %d", codec)
	}
}
