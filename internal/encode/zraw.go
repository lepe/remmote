package encode

import (
	"bytes"
	"fmt"
	"image"

	"github.com/klauspost/compress/zstd"

	"github.com/lepe/remmote/internal/proto"
)

// CodecHybrid is the pseudo-codec for -codec hybrid: per rect, ZRAW when
// it compresses well, JPEG otherwise. It never appears on the wire.
const CodecHybrid uint8 = 0xFF

// zrawRatio gates the hybrid choice: ZRAW wins when its output is at most
// this fraction of the raw pixel bytes (4:1). Desktop content (flat
// colors, text) compresses 10-100x; photographic content does not, and
// falls back to JPEG to bound bandwidth.
const zrawRatio = 4

// ZRAW is the zstd-compressed raw-RGBA encoder (pure Go). Encodes in a
// fraction of JPEG's CPU time and decodes ~5x faster, which is what makes
// sub-frame input echo feel local on a LAN.
type ZRAW struct {
	enc        *zstd.Encoder
	compressed []byte // scratch output; copied before it is queued
	packed     []byte // reused only for strided input; never exposed to callers
}

func newZRAW() (*ZRAW, error) {
	// Concurrency 1: each Encode is one short stream on the capture-loop
	// goroutine, so GOMAXPROCS-wide block encoders only add goroutine
	// handoff (measured ~10% on delta-sized rects) and take cores away
	// from capture and input injection.
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedFastest),
		zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, fmt.Errorf("encode: zstd writer: %w", err)
	}
	return &ZRAW{enc: enc}, nil
}

func (ZRAW) Codec() uint8 { return proto.CodecZRAW }

// Encode packs img's rows (RGBA, w*h*4 bytes) through zstd. A full-width
// image compresses without packing; narrower subimages use reusable
// packed storage so compression runs without streaming buffer copies.
// The returned output is fresh per call: queued frames share it read-only.
func (z *ZRAW) Encode(img image.Image, _ int) (uint8, []byte, error) {
	codec, data, err := z.encode(img)
	return codec, bytes.Clone(data), err
}

// encode borrows the output until the next call. Hybrid can discard a
// rejected ZRAW candidate without allocating an owned payload.
func (z *ZRAW) encode(img image.Image) (uint8, []byte, error) {
	rgba, ok := img.(*image.RGBA)
	if !ok {
		return 0, nil, fmt.Errorf("encode: zraw needs *image.RGBA, got %T", img)
	}
	w, h := rgba.Rect.Dx(), rgba.Rect.Dy()
	if w <= 0 || h <= 0 {
		return 0, nil, fmt.Errorf("encode: zraw: empty rect %v", rgba.Rect)
	}
	rowBytes := w * 4
	var pixels []byte
	if rgba.Stride == rowBytes {
		pixels = rgba.Pix[:rowBytes*h]
	} else {
		n := rowBytes * h
		if cap(z.packed) < n {
			z.packed = make([]byte, n)
		}
		pixels = z.packed[:n]
		for row := 0; row < h; row++ {
			copy(pixels[row*rowBytes:(row+1)*rowBytes], rgba.Pix[row*rgba.Stride:row*rgba.Stride+rowBytes])
		}
	}
	data := z.enc.EncodeAll(pixels, z.compressed[:0])
	z.compressed = data
	if len(data) > proto.MaxPayload {
		return 0, nil, fmt.Errorf("encode: zraw: %d bytes over wire limit", len(data))
	}
	return proto.CodecZRAW, data, nil
}

// Hybrid picks ZRAW or JPEG per rect: try ZRAW (cheap) and keep it when
// the ratio is good; spend JPEG's CPU only on photographic content, where
// raw pixels would blow the bandwidth budget.
type Hybrid struct {
	zraw *ZRAW
	jpeg Encoder
}

func newHybrid() (*Hybrid, error) {
	j, err := newJPEG()
	if err != nil {
		return nil, err
	}
	z, err := newZRAW()
	if err != nil {
		return nil, err
	}
	return &Hybrid{zraw: z, jpeg: j}, nil
}

func (Hybrid) Codec() uint8 { return CodecHybrid }

func (h *Hybrid) Encode(img image.Image, quality int) (uint8, []byte, error) {
	// ZRAW first: it is cheap enough that JPEG is only spent on content
	// zstd cannot pack (photos, gradients, noise). A non-RGBA image
	// fails in zraw.encode and lands on the JPEG path below.
	codec, data, err := h.zraw.encode(img)
	if err == nil {
		if r, ok := img.(*image.RGBA); ok {
			raw := int64(r.Rect.Dx()) * int64(r.Rect.Dy()) * 4
			if int64(len(data)) <= raw/zrawRatio {
				return codec, bytes.Clone(data), nil
			}
		}
	}
	return h.jpeg.Encode(img, quality)
}
