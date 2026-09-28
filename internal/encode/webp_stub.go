//go:build !webp

package encode

import "errors"

// newWebP fails fast: the WebP encoder is only compiled into builds tagged
// `webp` (CGo + libwebp). Failing at startup, never mid-stream, keeps the
// default CGo-free build honest.
func newWebP() (Encoder, error) {
	return nil, errors.New("encode: webp codec not built in; rebuild with `-tags webp` (requires CGo and libwebp-dev)")
}
