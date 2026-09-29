package proto

import (
	"testing"
)

// codecMinVersion is the oldest protocol version whose clients can decode
// each codec byte. The server refuses a mismatched ClientHello.version,
// so a codec that old clients cannot decode must not be emitted below
// its version — that is why adding ZRAW required the bump to v3.
var codecMinVersion = map[uint8]uint16{
	CodecJPEG: 1,
	CodecWebP: 1,
	CodecZRAW: 3,
}

func TestProtoVersionCoversCodecSet(t *testing.T) {
	for codec, min := range codecMinVersion {
		if ProtoVersion < min {
			t.Errorf("codec %s needs protocol v%d, ProtoVersion is %d",
				CodecName(codec), min, ProtoVersion)
		}
	}
}

// Every codec the encoders can produce must survive the wire codec
// check, and anything else must be rejected — otherwise a mixed stream
// fails at the client instead of at the boundary.
func TestRectUpdateCodecBytes(t *testing.T) {
	for codec := range codecMinVersion {
		ru := &RectUpdate{Seq: 7, X: 1, Y: 2, W: 8, H: 9, Codec: codec, Flags: FlagKeyframe, Data: []byte{1, 2, 3}}
		m, err := DecodeRectUpdate(ru.Encode())
		if err != nil {
			t.Fatalf("codec %s: %v", CodecName(codec), err)
		}
		if m.Codec != codec || m.W != 8 || m.H != 9 || len(m.Data) != 3 {
			t.Fatalf("codec %s round trip: %+v", CodecName(codec), m)
		}
	}
	for _, codec := range []uint8{0, 4, 0xFF, 200} {
		ru := &RectUpdate{Seq: 1, W: 4, H: 4, Codec: codec, Data: []byte{1}}
		p := ru.Encode()
		// Encode does not validate, so patch the byte the way the wire
		// would carry an unknown codec.
		p[12] = codec
		if _, err := DecodeRectUpdate(p); err == nil {
			t.Errorf("codec %d accepted, want rejection", codec)
		}
	}
}
