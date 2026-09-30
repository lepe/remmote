package proto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
)

// roundTrip encodes m, frames it, reads the frame back, and decodes it into
// a new value that must deep-equal the original.
func roundTrip(t *testing.T, typ MsgType, m any, encode func() []byte, decode func([]byte) (any, error)) {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteMsg(&buf, typ, 0, encode()); err != nil {
		t.Fatalf("frame write: %v", err)
	}
	gotType, _, payload, err := ReadMsg(&buf)
	if err != nil {
		t.Fatalf("frame read: %v", err)
	}
	if gotType != typ {
		t.Fatalf("type = %v, want %v", gotType, typ)
	}
	got, err := decode(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(got, m) {
		t.Fatalf("round trip mismatch:\n got  %#v\n want %#v", got, m)
	}
}

func TestMessagesRoundTrip(t *testing.T) {
	hello := &ClientHello{Version: 1, Reserved: 0}
	roundTrip(t, MsgClientHello, hello, hello.Encode,
		func(p []byte) (any, error) { return DecodeClientHello(p) })

	srv := &ServerHello{Version: 1, Width: 1920, Height: 1052, Depth: 24, Quality: 75, Name: "host.example"}
	roundTrip(t, MsgServerHello, srv, srv.Encode,
		func(p []byte) (any, error) { return DecodeServerHello(p) })
	srv2 := &ServerHello{Version: 1, Width: 640, Height: 480, Depth: 32, Quality: 90, Name: ""}
	roundTrip(t, MsgServerHello, srv2, srv2.Encode,
		func(p []byte) (any, error) { return DecodeServerHello(p) })

	resize := &ScreenResize{Width: 2560, Height: 1440}
	roundTrip(t, MsgScreenResize, resize, resize.Encode,
		func(p []byte) (any, error) { return DecodeScreenResize(p) })

	req := &Resize{Width: 1600, Height: 900}
	roundTrip(t, MsgResize, req, req.Encode,
		func(p []byte) (any, error) { return DecodeResize(p) })

	ping := &PingPong{Nonce: 0xDEADBEEFCAFEBABE, TS: 1725999999999}
	roundTrip(t, MsgPing, ping, ping.Encode,
		func(p []byte) (any, error) { return DecodePingPong(p) })

	move := &MouseMove{X: 1919, Y: 1051}
	roundTrip(t, MsgMouseMove, move, move.Encode,
		func(p []byte) (any, error) { return DecodeMouseMove(p) })

	btn := &MouseButton{Button: 4, Down: true}
	roundTrip(t, MsgMouseButton, btn, btn.Encode,
		func(p []byte) (any, error) { return DecodeMouseButton(p) })

	wheel := &Wheel{DX: -2, DY: 3}
	roundTrip(t, MsgWheel, wheel, wheel.Encode,
		func(p []byte) (any, error) { return DecodeWheel(p) })

	key := &Key{Down: true, Keysym: 0xff0d}
	roundTrip(t, MsgKey, key, key.Encode,
		func(p []byte) (any, error) { return DecodeKey(p) })

	q := &SetQuality{Quality: 60}
	roundTrip(t, MsgSetQuality, q, q.Encode,
		func(p []byte) (any, error) { return DecodeSetQuality(p) })

	closeMsg := &Close{Code: CloseSlowClient, Reason: "write deadline exceeded"}
	roundTrip(t, MsgClose, closeMsg, closeMsg.Encode,
		func(p []byte) (any, error) { return DecodeClose(p) })

	clip := &ClipboardData{Text: "héllo remmote 🖥"}
	roundTrip(t, MsgClipboard, clip, clip.Encode,
		func(p []byte) (any, error) { return DecodeClipboard(p) })
	clipEmpty := &ClipboardData{Text: ""}
	roundTrip(t, MsgClipboard, clipEmpty, clipEmpty.Encode,
		func(p []byte) (any, error) { return DecodeClipboard(p) })
}

func TestDecodeClipboardBad(t *testing.T) {
	if _, err := DecodeClipboard(make([]byte, 3)); !errors.Is(err, ErrShortPayload) {
		t.Errorf("short: err = %v", err)
	}
	if _, err := DecodeClipboard([]byte{0, 0, 0, 9, 'a'}); !errors.Is(err, ErrShortPayload) {
		t.Errorf("truncated text: err = %v", err)
	}
	// Genuinely oversize: > MaxClipboard bytes actually present.
	big := make([]byte, 4+MaxClipboard+16)
	binary.BigEndian.PutUint32(big, uint32(MaxClipboard+16))
	if _, err := DecodeClipboard(big); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversize: err = %v", err)
	}
}

func TestRectUpdateRoundTrip(t *testing.T) {
	m := &RectUpdate{
		Seq: 42, X: 100, Y: 200, W: 640, H: 480,
		Codec: CodecJPEG, Flags: FlagKeyframe,
		Data: bytes.Repeat([]byte{0x42}, 2048),
	}
	var buf bytes.Buffer
	if err := WriteMsg(&buf, MsgRectUpdate, 0, m.Encode()); err != nil {
		t.Fatalf("write: %v", err)
	}
	gotType, _, payload, err := ReadMsg(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if gotType != MsgRectUpdate {
		t.Fatalf("type = %v", gotType)
	}
	got, err := DecodeRectUpdate(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(got, m) {
		t.Fatalf("mismatch:\n got  %+v\n want %+v", got, m)
	}
}

func TestRectUpdateGolden(t *testing.T) {
	m := &RectUpdate{Seq: 1, X: 2, Y: 3, W: 4, H: 5, Codec: CodecJPEG, Flags: 0, Data: []byte{0xAA, 0xBB}}
	want := []byte{
		0x00, 0x00, 0x00, 0x01, // seq
		0x00, 0x02, // x
		0x00, 0x03, // y
		0x00, 0x04, // w
		0x00, 0x05, // h
		0x01,       // codec jpeg
		0x00,       // flags
		0xAA, 0xBB, // data
	}
	if got := m.Encode(); !bytes.Equal(got, want) {
		t.Fatalf("golden mismatch:\n got  % x\n want % x", got, want)
	}
}

func TestDecodeRejectsBadPayloads(t *testing.T) {
	cases := []struct {
		name string
		fn   func([]byte) error
		in   []byte
		want error
	}{
		{"clienthello short", func(p []byte) error { _, err := DecodeClientHello(p); return err }, make([]byte, 3), ErrShortPayload},
		{"serverhello short", func(p []byte) error { _, err := DecodeServerHello(p); return err }, make([]byte, 9), ErrShortPayload},
		{"serverhello bad namelen", func(p []byte) error { _, err := DecodeServerHello(p); return err }, []byte{0, 1, 0, 2, 0, 3, 24, 75, 0, 9, 'a'}, ErrShortPayload},
		{"rectupdate short", func(p []byte) error { _, err := DecodeRectUpdate(p); return err }, make([]byte, 12), ErrShortPayload},
		{"rectupdate zero size", func(p []byte) error { _, err := DecodeRectUpdate(p); return err }, make([]byte, 14), ErrBadPayload},
		{"rectupdate bad codec", func(p []byte) error { _, err := DecodeRectUpdate(p); return err }, []byte{0, 0, 0, 1, 0, 0, 0, 0, 0, 1, 0, 1, 9, 0}, ErrBadPayload},
		{"resize short", func(p []byte) error { _, err := DecodeScreenResize(p); return err }, make([]byte, 3), ErrShortPayload},
		{"resize request short", func(p []byte) error { _, err := DecodeResize(p); return err }, make([]byte, 3), ErrShortPayload},
		{"resize request zero width", func(p []byte) error { _, err := DecodeResize(p); return err }, []byte{0, 0, 0, 100}, ErrBadPayload},
		{"resize request zero height", func(p []byte) error { _, err := DecodeResize(p); return err }, []byte{0, 100, 0, 0}, ErrBadPayload},
		{"ping short", func(p []byte) error { _, err := DecodePingPong(p); return err }, make([]byte, 15), ErrShortPayload},
		{"mousemove short", func(p []byte) error { _, err := DecodeMouseMove(p); return err }, make([]byte, 3), ErrShortPayload},
		{"mousebutton short", func(p []byte) error { _, err := DecodeMouseButton(p); return err }, make([]byte, 1), ErrShortPayload},
		{"mousebutton range", func(p []byte) error { _, err := DecodeMouseButton(p); return err }, []byte{8, 1}, ErrBadPayload},
		{"mousebutton zero", func(p []byte) error { _, err := DecodeMouseButton(p); return err }, []byte{0, 1}, ErrBadPayload},
		{"wheel short", func(p []byte) error { _, err := DecodeWheel(p); return err }, make([]byte, 3), ErrShortPayload},
		{"key short", func(p []byte) error { _, err := DecodeKey(p); return err }, make([]byte, 4), ErrShortPayload},
		{"setquality short", func(p []byte) error { _, err := DecodeSetQuality(p); return err }, nil, ErrShortPayload},
		{"close short", func(p []byte) error { _, err := DecodeClose(p); return err }, make([]byte, 2), ErrShortPayload},
		{"close bad reasonlen", func(p []byte) error { _, err := DecodeClose(p); return err }, []byte{1, 0, 9, 'a'}, ErrShortPayload},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.fn(tc.in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}
