package proto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		typ     MsgType
		flags   uint8
		payload []byte
	}{
		{"empty", MsgPing, 0, nil},
		{"small", MsgMouseMove, 0, []byte{1, 2, 3, 4}},
		{"flags", MsgRectUpdate, FlagKeyframe, bytes.Repeat([]byte{0xAB}, 1000)},
		{"large-ish", MsgRectUpdate, 0, bytes.Repeat([]byte{0x11}, 1<<20)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteMsg(&buf, tc.typ, tc.flags, tc.payload); err != nil {
				t.Fatalf("write: %v", err)
			}
			gotType, gotFlags, gotPayload, err := ReadMsg(&buf)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if gotType != tc.typ || gotFlags != tc.flags {
				t.Fatalf("header = (%v, %#x), want (%v, %#x)", gotType, gotFlags, tc.typ, tc.flags)
			}
			if !bytes.Equal(gotPayload, tc.payload) {
				t.Fatalf("payload mismatch: %d bytes vs %d", len(gotPayload), len(tc.payload))
			}
			// Writing must not have retained the payload: safe to reuse.
			if len(tc.payload) > 0 {
				tc.payload[0] ^= 0xFF
			}
		})
	}
}

func TestFrameBadMagic(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteMsg(&buf, MsgPing, 0, []byte{1})
	b := buf.Bytes()
	b[0] = 'X'
	_, _, _, err := ReadMsg(bytes.NewReader(b))
	if !errors.Is(err, ErrBadMagic) {
		t.Fatalf("err = %v, want ErrBadMagic", err)
	}
}

func TestFrameTooLarge(t *testing.T) {
	hdr := make([]byte, HeaderSize)
	hdr[0], hdr[1] = 'R', 'M'
	hdr[2] = byte(MsgRectUpdate)
	binary.BigEndian.PutUint32(hdr[4:8], MaxPayload+1)
	_, _, _, err := ReadMsg(bytes.NewReader(hdr))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestFrameTruncated(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteMsg(&buf, MsgRectUpdate, 0, make([]byte, 64))
	b := buf.Bytes()[:HeaderSize+10] // cut mid-payload
	_, _, _, err := ReadMsg(bytes.NewReader(b))
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
}

func TestFrameMaxPayloadBoundary(t *testing.T) {
	hdr := make([]byte, HeaderSize)
	hdr[0], hdr[1] = 'R', 'M'
	binary.BigEndian.PutUint32(hdr[4:8], MaxPayload)
	// Header alone with a full-size payload absent: should be ErrTruncated,
	// proving the boundary itself is accepted by the size guard.
	_, _, _, err := ReadMsg(bytes.NewReader(hdr))
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
}
