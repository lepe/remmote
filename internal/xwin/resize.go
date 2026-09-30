package xwin

import (
	"encoding/binary"
	"fmt"

	"github.com/jezek/xgb/xproto"
)

// wmStateAction is the action field of an _NET_WM_STATE client message
// (EWMH 4.1).
const (
	wmStateRemove uint32 = 0
	wmStateAdd    uint32 = 1
)

// Resize sets w's interior size to width×height, leaving it where it is.
// With a window manager the request goes out as EWMH
// _NET_MOVERESIZE_WINDOW — the route a pager takes, and the only one a
// WM is told to obey rather than second-guess — after a maximized state
// has been cleared, because a maximized window's geometry belongs to the
// WM and would swallow the new size. Without a WM nobody reads EWMH (and
// on bare Xvfb nobody redirects a ConfigureRequest either), so the window
// is resized outright. Like Maximize, it reports how the request was made
// ("ewmh" or "resize"): both are fire-and-forget, and the geometry
// follows asynchronously via ConfigureNotify.
func (c *Client) Resize(w xproto.Window, width, height int) (string, error) {
	if width < 1 || height < 1 || width > 0xffff || height > 0xffff {
		return "", fmt.Errorf("xwin: resize %dx%d out of range", width, height)
	}
	if !c.HasWM() {
		return "resize", c.configureSize(w, width, height)
	}
	moves := c.Atom("_NET_MOVERESIZE_WINDOW")
	if moves == 0 {
		// A WM that is not EWMH: the plain request is the best there is.
		return "resize", c.configureSize(w, width, height)
	}
	if err := c.clearMaximized(w); err != nil {
		return "ewmh", err
	}
	return "ewmh", xproto.SendEventChecked(c.x, false, c.root,
		xproto.EventMaskSubstructureRedirect|xproto.EventMaskSubstructureNotify,
		string(moveResizeClientMessage(w, moves, width, height))).Check()
}

// configureSize resizes w without going through a window manager.
func (c *Client) configureSize(w xproto.Window, width, height int) error {
	return xproto.ConfigureWindowChecked(c.x, w,
		xproto.ConfigWindowWidth|xproto.ConfigWindowHeight,
		[]uint32{uint32(width), uint32(height)}).Check()
}

// clearMaximized removes _NET_WM_STATE_MAXIMIZED_{VERT,HORZ} from w when
// it carries them — no-op (no round trip beyond the property read) when
// it does not, which is the common case.
func (c *Client) clearMaximized(w xproto.Window) error {
	vert := c.Atom("_NET_WM_STATE_MAXIMIZED_VERT")
	horz := c.Atom("_NET_WM_STATE_MAXIMIZED_HORZ")
	state := c.Atom("_NET_WM_STATE")
	if state == 0 || vert == 0 || horz == 0 {
		return nil
	}
	data, typ, err := c.prop(w, "_NET_WM_STATE")
	if err != nil || typ != state || len(data) < 4 {
		return nil
	}
	has := func(a xproto.Atom) bool {
		for i := 0; i+4 <= len(data); i += 4 {
			if xproto.Atom(binary.LittleEndian.Uint32(data[i:i+4])) == a {
				return true
			}
		}
		return false
	}
	if !has(vert) && !has(horz) {
		return nil
	}
	return xproto.SendEventChecked(c.x, false, c.root,
		xproto.EventMaskSubstructureRedirect|xproto.EventMaskSubstructureNotify,
		string(wmStateClientMessage(w, state, wmStateRemove, vert, horz))).Check()
}

// moveResizeClientMessage builds the 32-byte _NET_MOVERESIZE_WINDOW
// ClientMessage for a size-only change: only the width and height
// presence bits are set, x/y stay 0 and unannounced, so the window keeps
// exactly where it is. EWMH 4.2 reads those bits as "bits 8 to 11
// indicate the presence of x, y, width and height" — bit 10 is width,
// bit 11 height — and that is how openbox implements it: verified
// against a live WM, the interior size lands exactly and the position
// neither moves nor drifts over repeated resizes. Announcing x/y instead
// is what drifts: the receiving side may treat the coordinates as
// frame-relative rather than client-relative, and re-sending the client's
// own position through a frame-relative reading re-anchors the window a
// decoration's width and height on every resize. A manager reading the
// bits as pairs simply applies no size — degraded, not damaged. Source
// indication 2 is "pager" (EWMH 9.11): the value a window manager is
// expected to obey rather than refuse.
//
// The event is built by hand for the same reason as maximizeClientMessage:
// xgb's ClientMessageData union only serialises its raw Data8 bytes, and
// little-endian because xgb negotiates an LSB-first connection.
func moveResizeClientMessage(w xproto.Window, moves xproto.Atom, width, height int) []byte {
	b := make([]byte, 32)
	b[0] = 33 // ClientMessage
	b[1] = 32 // format: 32-bit
	binary.LittleEndian.PutUint32(b[4:8], uint32(w))
	binary.LittleEndian.PutUint32(b[8:12], uint32(moves))
	binary.LittleEndian.PutUint32(b[12:16],
		(0xC<<8)|(2<<12)) // presence bits 10-11 (width, height) + source: pager
	binary.LittleEndian.PutUint32(b[24:28], uint32(width))
	binary.LittleEndian.PutUint32(b[28:32], uint32(height))
	return b
}
