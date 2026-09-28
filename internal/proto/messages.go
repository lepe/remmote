package proto

import (
	"encoding/binary"
	"fmt"
)

// ClientHello is sent by the client immediately after connecting.
type ClientHello struct {
	Version  uint16
	Reserved uint16
}

func (m *ClientHello) Encode() []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b[0:2], m.Version)
	binary.BigEndian.PutUint16(b[2:4], m.Reserved)
	return b
}

func DecodeClientHello(p []byte) (*ClientHello, error) {
	if len(p) < 4 {
		return nil, fmt.Errorf("%w: ClientHello: %d bytes", ErrShortPayload, len(p))
	}
	return &ClientHello{
		Version:  binary.BigEndian.Uint16(p[0:2]),
		Reserved: binary.BigEndian.Uint16(p[2:4]),
	}, nil
}

// ServerHello is the server's reply to a version-compatible ClientHello.
type ServerHello struct {
	Version uint16
	Width   uint16
	Height  uint16
	Depth   uint8
	Quality uint8
	Name    string
}

func (m *ServerHello) Encode() []byte {
	b := make([]byte, 10+len(m.Name))
	binary.BigEndian.PutUint16(b[0:2], m.Version)
	binary.BigEndian.PutUint16(b[2:4], m.Width)
	binary.BigEndian.PutUint16(b[4:6], m.Height)
	b[6] = m.Depth
	b[7] = m.Quality
	binary.BigEndian.PutUint16(b[8:10], uint16(len(m.Name)))
	copy(b[10:], m.Name)
	return b
}

func DecodeServerHello(p []byte) (*ServerHello, error) {
	if len(p) < 10 {
		return nil, fmt.Errorf("%w: ServerHello: %d bytes", ErrShortPayload, len(p))
	}
	n := int(binary.BigEndian.Uint16(p[8:10]))
	if len(p) < 10+n {
		return nil, fmt.Errorf("%w: ServerHello: name truncated", ErrShortPayload)
	}
	return &ServerHello{
		Version: binary.BigEndian.Uint16(p[0:2]),
		Width:   binary.BigEndian.Uint16(p[2:4]),
		Height:  binary.BigEndian.Uint16(p[4:6]),
		Depth:   p[6],
		Quality: p[7],
		Name:    string(p[10 : 10+n]),
	}, nil
}

// RectUpdate carries one encoded region of the host screen.
type RectUpdate struct {
	Seq   uint32
	X, Y  uint16
	W, H  uint16
	Codec uint8
	Flags uint8 // bit 0 = keyframe (full screen, canvas may reset)
	Data  []byte
}

const rectUpdateHeader = 14 // seq(4) x(2) y(2) w(2) h(2) codec(1) flags(1)

func (m *RectUpdate) Encode() []byte {
	b := make([]byte, rectUpdateHeader+len(m.Data))
	binary.BigEndian.PutUint32(b[0:4], m.Seq)
	binary.BigEndian.PutUint16(b[4:6], m.X)
	binary.BigEndian.PutUint16(b[6:8], m.Y)
	binary.BigEndian.PutUint16(b[8:10], m.W)
	binary.BigEndian.PutUint16(b[10:12], m.H)
	b[12] = m.Codec
	b[13] = m.Flags
	copy(b[rectUpdateHeader:], m.Data)
	return b
}

func DecodeRectUpdate(p []byte) (*RectUpdate, error) {
	if len(p) < rectUpdateHeader {
		return nil, fmt.Errorf("%w: RectUpdate: %d bytes", ErrShortPayload, len(p))
	}
	w := binary.BigEndian.Uint16(p[8:10])
	h := binary.BigEndian.Uint16(p[10:12])
	if w == 0 || h == 0 {
		return nil, fmt.Errorf("%w: RectUpdate: empty %dx%d", ErrBadPayload, w, h)
	}
	if p[12] != CodecJPEG && p[12] != CodecWebP {
		return nil, fmt.Errorf("%w: RectUpdate: unknown codec %d", ErrBadPayload, p[12])
	}
	data := make([]byte, len(p)-rectUpdateHeader)
	copy(data, p[rectUpdateHeader:])
	return &RectUpdate{
		Seq:   binary.BigEndian.Uint32(p[0:4]),
		X:     binary.BigEndian.Uint16(p[4:6]),
		Y:     binary.BigEndian.Uint16(p[6:8]),
		W:     w,
		H:     h,
		Codec: p[12],
		Flags: p[13],
		Data:  data,
	}, nil
}

// ScreenResize announces a new host screen size; a keyframe RectUpdate
// follows immediately after.
type ScreenResize struct {
	Width  uint16
	Height uint16
}

func (m *ScreenResize) Encode() []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b[0:2], m.Width)
	binary.BigEndian.PutUint16(b[2:4], m.Height)
	return b
}

func DecodeScreenResize(p []byte) (*ScreenResize, error) {
	if len(p) < 4 {
		return nil, fmt.Errorf("%w: ScreenResize: %d bytes", ErrShortPayload, len(p))
	}
	return &ScreenResize{
		Width:  binary.BigEndian.Uint16(p[0:2]),
		Height: binary.BigEndian.Uint16(p[2:4]),
	}, nil
}

// Ping / Pong keepalive. Pong echoes the nonce and timestamp verbatim.
type PingPong struct {
	Nonce uint64
	TS    uint64 // unix milliseconds
}

func (m *PingPong) Encode() []byte {
	b := make([]byte, 16)
	binary.BigEndian.PutUint64(b[0:8], m.Nonce)
	binary.BigEndian.PutUint64(b[8:16], m.TS)
	return b
}

func DecodePingPong(p []byte) (*PingPong, error) {
	if len(p) < 16 {
		return nil, fmt.Errorf("%w: Ping/Pong: %d bytes", ErrShortPayload, len(p))
	}
	return &PingPong{
		Nonce: binary.BigEndian.Uint64(p[0:8]),
		TS:    binary.BigEndian.Uint64(p[8:16]),
	}, nil
}

// MouseMove positions the host pointer at absolute screen coordinates.
type MouseMove struct {
	X uint16
	Y uint16
}

func (m *MouseMove) Encode() []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b[0:2], m.X)
	binary.BigEndian.PutUint16(b[2:4], m.Y)
	return b
}

func DecodeMouseMove(p []byte) (*MouseMove, error) {
	if len(p) < 4 {
		return nil, fmt.Errorf("%w: MouseMove: %d bytes", ErrShortPayload, len(p))
	}
	return &MouseMove{X: binary.BigEndian.Uint16(p[0:2]), Y: binary.BigEndian.Uint16(p[2:4])}, nil
}

// MouseButton presses or releases a button. Buttons 1-3 are the pointer
// buttons; 4/5/6/7 are wheel up/down/left/right, which the server replays
// as XTest press+release pairs.
type MouseButton struct {
	Button uint8
	Down   bool
}

func (m *MouseButton) Encode() []byte {
	return []byte{m.Button, boolToU8(m.Down)}
}

func DecodeMouseButton(p []byte) (*MouseButton, error) {
	if len(p) < 2 {
		return nil, fmt.Errorf("%w: MouseButton: %d bytes", ErrShortPayload, len(p))
	}
	if p[0] < 1 || p[0] > 7 {
		return nil, fmt.Errorf("%w: MouseButton: button %d out of range", ErrBadPayload, p[0])
	}
	return &MouseButton{Button: p[0], Down: p[1] != 0}, nil
}

// Wheel carries wheel deltas in notches (reserved for high-resolution
// wheels; X clients normally send MouseButton 4-7 instead).
type Wheel struct {
	DX int16
	DY int16
}

func (m *Wheel) Encode() []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b[0:2], uint16(m.DX))
	binary.BigEndian.PutUint16(b[2:4], uint16(m.DY))
	return b
}

func DecodeWheel(p []byte) (*Wheel, error) {
	if len(p) < 4 {
		return nil, fmt.Errorf("%w: Wheel: %d bytes", ErrShortPayload, len(p))
	}
	return &Wheel{
		DX: int16(binary.BigEndian.Uint16(p[0:2])),
		DY: int16(binary.BigEndian.Uint16(p[2:4])),
	}, nil
}

// Key presses or releases a keysym (X11 keysym values, e.g. 'a' = 0x61,
// XK_Return = 0xff0d).
type Key struct {
	Down   bool
	Keysym uint32
}

func (m *Key) Encode() []byte {
	b := make([]byte, 5)
	b[0] = boolToU8(m.Down)
	binary.BigEndian.PutUint32(b[1:5], m.Keysym)
	return b
}

func DecodeKey(p []byte) (*Key, error) {
	if len(p) < 5 {
		return nil, fmt.Errorf("%w: Key: %d bytes", ErrShortPayload, len(p))
	}
	return &Key{Down: p[0] != 0, Keysym: binary.BigEndian.Uint32(p[1:5])}, nil
}

// SetQuality adjusts the encoder quality (1-100) at runtime.
type SetQuality struct {
	Quality uint8
}

func (m *SetQuality) Encode() []byte {
	return []byte{m.Quality}
}

func DecodeSetQuality(p []byte) (*SetQuality, error) {
	if len(p) < 1 {
		return nil, fmt.Errorf("%w: SetQuality: %d bytes", ErrShortPayload, len(p))
	}
	return &SetQuality{Quality: p[0]}, nil
}

// Close carries a shutdown reason. Sent by either side before disconnecting.
type Close struct {
	Code   uint8
	Reason string
}

func (m *Close) Encode() []byte {
	b := make([]byte, 3+len(m.Reason))
	b[0] = m.Code
	binary.BigEndian.PutUint16(b[1:3], uint16(len(m.Reason)))
	copy(b[3:], m.Reason)
	return b
}

func DecodeClose(p []byte) (*Close, error) {
	if len(p) < 3 {
		return nil, fmt.Errorf("%w: Close: %d bytes", ErrShortPayload, len(p))
	}
	n := int(binary.BigEndian.Uint16(p[1:3]))
	if len(p) < 3+n {
		return nil, fmt.Errorf("%w: Close: reason truncated", ErrShortPayload)
	}
	return &Close{Code: p[0], Reason: string(p[3 : 3+n])}, nil
}

func boolToU8(v bool) uint8 {
	if v {
		return 1
	}
	return 0
}

// ClipboardData carries UTF-8 clipboard text between the peers, in either
// direction. Oversized payloads are refused at decode time.
type ClipboardData struct {
	Text string
}

// MaxClipboard bounds clipboard payloads (INCR-style transfers are out of
// scope; larger selections are skipped by the watchers).
const MaxClipboard = 256 << 10 // 256 KiB

func (m *ClipboardData) Encode() []byte {
	b := make([]byte, 4+len(m.Text))
	binary.BigEndian.PutUint32(b[0:4], uint32(len(m.Text)))
	copy(b[4:], m.Text)
	return b
}

func DecodeClipboard(p []byte) (*ClipboardData, error) {
	if len(p) < 4 {
		return nil, fmt.Errorf("%w: Clipboard: %d bytes", ErrShortPayload, len(p))
	}
	n := binary.BigEndian.Uint32(p[0:4])
	if uint64(n) > uint64(len(p)-4) {
		return nil, fmt.Errorf("%w: Clipboard: text truncated", ErrShortPayload)
	}
	if n > MaxClipboard {
		return nil, fmt.Errorf("%w: Clipboard: %d bytes > %d", ErrTooLarge, n, MaxClipboard)
	}
	return &ClipboardData{Text: string(p[4 : 4+n])}, nil
}
