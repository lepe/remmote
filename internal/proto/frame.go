package proto

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	magic0 byte = 'R'
	magic1 byte = 'M'
)

// WriteMsg frames and writes one message. The caller owns payload; WriteMsg
// does not retain it. For efficiency callers should pass a *bufio.Writer.
func WriteMsg(w io.Writer, t MsgType, flags uint8, payload []byte) error {
	var hdr [HeaderSize]byte
	hdr[0] = magic0
	hdr[1] = magic1
	hdr[2] = byte(t)
	hdr[3] = flags
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// ReadMsg reads one framed message. The returned payload is freshly
// allocated; the caller owns it. Any error means the stream is unusable.
func ReadMsg(r io.Reader) (t MsgType, flags uint8, payload []byte, err error) {
	var hdr [HeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, 0, nil, err
	}
	if hdr[0] != magic0 || hdr[1] != magic1 {
		return 0, 0, nil, ErrBadMagic
	}
	t = MsgType(hdr[2])
	flags = hdr[3]
	n := binary.BigEndian.Uint32(hdr[4:8])
	if n > MaxPayload {
		return t, flags, nil, fmt.Errorf("%w: %d bytes (max %d)", ErrTooLarge, n, MaxPayload)
	}
	payload = make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return t, flags, nil, fmt.Errorf("%w: %v", ErrTruncated, err)
	}
	return t, flags, payload, nil
}
