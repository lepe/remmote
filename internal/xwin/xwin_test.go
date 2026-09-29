package xwin

import (
	"encoding/binary"
	"testing"

	"github.com/jezek/xgb/xproto"
)

// The maximization request is a synthetic ClientMessage, and the fields
// are packed by hand (xgb's ClientMessageData union only serialises raw
// Data8 bytes). A misplaced field is invisible until a window manager
// silently ignores the request, so the wire layout is pinned here.
func TestMaximizeClientMessageLayout(t *testing.T) {
	const (
		win   xproto.Window = 0x12345678
		state xproto.Atom   = 0x0A
		vert  xproto.Atom   = 0x0B
		horz  xproto.Atom   = 0x0C
	)
	b := maximizeClientMessage(win, state, vert, horz)
	if len(b) != 32 {
		t.Fatalf("event length = %d, want 32", len(b))
	}
	if b[0] != 33 {
		t.Errorf("event code = %d, want 33 (ClientMessage)", b[0])
	}
	if b[1] != 32 {
		t.Errorf("format = %d, want 32", b[1])
	}
	u32 := func(off int) uint32 { return binary.LittleEndian.Uint32(b[off : off+4]) }
	if got := u32(4); got != uint32(win) {
		t.Errorf("window = %#x, want %#x", got, uint32(win))
	}
	if got := u32(8); got != uint32(state) {
		t.Errorf("type = %#x, want %#x (_NET_WM_STATE)", got, uint32(state))
	}
	// data = [action, arg1, arg2, source, pad]
	if got := u32(12); got != 1 {
		t.Errorf("action = %d, want 1 (_NET_WM_STATE_ADD)", got)
	}
	if got := u32(16); got != uint32(vert) {
		t.Errorf("arg1 = %#x, want %#x (_NET_WM_STATE_MAXIMIZED_VERT)", got, uint32(vert))
	}
	if got := u32(20); got != uint32(horz) {
		t.Errorf("arg2 = %#x, want %#x (_NET_WM_STATE_MAXIMIZED_HORZ)", got, uint32(horz))
	}
	if got := u32(24); got != 1 {
		t.Errorf("source = %d, want 1 (application)", got)
	}
	if got := u32(28); got != 0 {
		t.Errorf("pad = %d, want 0", got)
	}
}
