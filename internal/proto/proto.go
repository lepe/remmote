// Package proto defines the remmote wire protocol: a small set of binary
// messages exchanged over a plain TCP connection between remmote-client and
// remmote-server. All integers are big-endian (network order).
package proto

import "errors"

const (
	// ProtoVersion is the wire protocol version this build speaks.
	// v2 added the Clipboard message; v3 added the ZRAW codec byte;
	// v4 added the Resize request (client → server).
	//
	// The server refuses mismatched versions, so adding a wire value that
	// older clients cannot decode (ZRAW) requires a bump: without it a v2
	// client would complete the handshake and then drop every rect. v4
	// exists because an *older server* would drop the connection on a
	// message type it does not know — a v4 client must not reach one
	// silently.
	ProtoVersion uint16 = 4

	// HeaderSize is the fixed frame header: 'R','M',type,flags,length u32.
	HeaderSize = 8

	// MaxPayload bounds any single message payload, both as a decode guard
	// and as the encode-side limit that triggers codec quality backoff.
	MaxPayload = 32 << 20 // 32 MiB
)

// MsgType identifies a message within a frame.
type MsgType uint8

const (
	MsgClientHello  MsgType = 0x01
	MsgServerHello  MsgType = 0x02
	MsgRectUpdate   MsgType = 0x03
	MsgScreenResize MsgType = 0x04
	MsgPing         MsgType = 0x05
	MsgPong         MsgType = 0x06
	MsgMouseMove    MsgType = 0x07
	MsgMouseButton  MsgType = 0x08
	MsgWheel        MsgType = 0x09
	MsgKey          MsgType = 0x0A
	MsgSetQuality   MsgType = 0x0B
	MsgClose        MsgType = 0x0C
	MsgClipboard    MsgType = 0x0D
	MsgResize       MsgType = 0x0E
)

func (t MsgType) String() string {
	switch t {
	case MsgClientHello:
		return "ClientHello"
	case MsgServerHello:
		return "ServerHello"
	case MsgRectUpdate:
		return "RectUpdate"
	case MsgScreenResize:
		return "ScreenResize"
	case MsgPing:
		return "Ping"
	case MsgPong:
		return "Pong"
	case MsgMouseMove:
		return "MouseMove"
	case MsgMouseButton:
		return "MouseButton"
	case MsgWheel:
		return "Wheel"
	case MsgKey:
		return "Key"
	case MsgSetQuality:
		return "SetQuality"
	case MsgClose:
		return "Close"
	case MsgClipboard:
		return "Clipboard"
	case MsgResize:
		return "Resize"
	default:
		return "Unknown"
	}
}

// Frame flags. RectUpdate uses bit 0 to mark a keyframe (full-screen update
// a client can rebuild its canvas from).
const FlagKeyframe uint8 = 1 << 0

// Image codecs carried in RectUpdate.Codec and the server -codec flag.
const (
	CodecJPEG uint8 = 1
	CodecWebP uint8 = 2
	// CodecZRAW is zstd-compressed raw RGBA pixels (w*h*4 bytes after
	// decompression). Cheap enough on CPU that it beats JPEG end-to-end
	// for typical desktop content.
	CodecZRAW uint8 = 3
)

func CodecName(c uint8) string {
	switch c {
	case CodecJPEG:
		return "jpeg"
	case CodecWebP:
		return "webp"
	case CodecZRAW:
		return "zraw"
	default:
		return "unknown"
	}
}

// Close codes carried in Close.Codec.
const (
	CloseShutdown   uint8 = 1 // server is shutting down
	CloseVersion    uint8 = 2 // protocol version mismatch
	CloseProtocol   uint8 = 3 // malformed peer
	CloseSlowClient uint8 = 4 // client could not drain its stream
)

var (
	ErrBadMagic     = errors.New("remmote/proto: bad frame magic")
	ErrTooLarge     = errors.New("remmote/proto: frame payload too large")
	ErrTruncated    = errors.New("remmote/proto: truncated frame")
	ErrShortPayload = errors.New("remmote/proto: payload too short")
	ErrBadPayload   = errors.New("remmote/proto: malformed payload")
)
