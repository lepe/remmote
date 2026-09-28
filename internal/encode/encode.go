// Package encode abstracts the image codec used for rect updates: JPEG is
// always available (pure Go); a lossy WebP encoder exists in builds tagged
// `webp` (CGo + libwebp). The wire codec byte in proto.RectUpdate selects
// between them on the client side.
package encode

import (
	"fmt"
	"image"

	"github.com/lepe/remmote/internal/proto"
)

// Encoder encodes one image region at a quality level (1-100).
type Encoder interface {
	// Codec is the proto codec byte the produced bytes are tagged with.
	Codec() uint8
	// Encode returns compressed image data for img at the given quality.
	Encode(img image.Image, quality int) ([]byte, error)
}

// New returns the encoder for a proto codec byte. It fails fast when a codec
// was not compiled into this build (e.g. -codec webp without -tags webp).
func New(codec uint8) (Encoder, error) {
	switch codec {
	case proto.CodecJPEG:
		return newJPEG()
	case proto.CodecWebP:
		return newWebP()
	default:
		return nil, fmt.Errorf("encode: unknown codec %d", codec)
	}
}
