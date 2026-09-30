package xwin

import (
	"encoding/binary"
	"testing"

	"github.com/jezek/xgb/xproto"
)

func TestWMStateClientMessageRemoveLayout(t *testing.T) {
	const (
		win   xproto.Window = 0xABCDEF01
		state xproto.Atom   = 0x0A
		vert  xproto.Atom   = 0x0B
		horz  xproto.Atom   = 0x0C
	)
	b := wmStateClientMessage(win, state, wmStateRemove, vert, horz)
	u32 := func(off int) uint32 { return binary.LittleEndian.Uint32(b[off : off+4]) }
	if len(b) != 32 || b[0] != 33 || b[1] != 32 {
		t.Fatalf("event = %v (len %d), want a 32-byte ClientMessage", b[:2], len(b))
	}
	if got := u32(4); got != uint32(win) {
		t.Errorf("window = %#x, want %#x", got, uint32(win))
	}
	if got := u32(8); got != uint32(state) {
		t.Errorf("type = %#x, want _NET_WM_STATE %#x", got, uint32(state))
	}
	if got := u32(12); got != 0 {
		t.Errorf("action = %d, want 0 (_NET_WM_STATE_REMOVE)", got)
	}
	if got := u32(16); got != uint32(vert) || u32(20) != uint32(horz) {
		t.Errorf("args = %#x,%#x, want MAXIMIZED_VERT/HORZ", u32(16), u32(20))
	}
	if got := u32(24); got != 1 {
		t.Errorf("source = %d, want 1 (application)", got)
	}
}

// _NET_MOVERESIZE_WINDOW is the only request that resizes a window a
// window manager owns, so its hand-packed layout is pinned: presence bits
// 10/11 (width, height) set, x/y deliberately absent so the window stays
// put, source "pager".
func TestMoveResizeClientMessageLayout(t *testing.T) {
	const (
		win   xproto.Window = 0x10203040
		moves xproto.Atom   = 0x21
	)
	b := moveResizeClientMessage(win, moves, 1600, 900)
	if len(b) != 32 {
		t.Fatalf("event length = %d, want 32", len(b))
	}
	if b[0] != 33 || b[1] != 32 {
		t.Fatalf("header = %d/%d, want ClientMessage/32", b[0], b[1])
	}
	u32 := func(off int) uint32 { return binary.LittleEndian.Uint32(b[off : off+4]) }
	if got := u32(4); got != uint32(win) {
		t.Errorf("window = %#x, want %#x", got, uint32(win))
	}
	if got := u32(8); got != uint32(moves) {
		t.Errorf("type = %#x, want _NET_MOVERESIZE_WINDOW %#x", got, uint32(moves))
	}
	flags := u32(12)
	if flags>>8&0xC != 0xC {
		t.Errorf("presence bits = %#b, want bits 10-11 (width, height) set", flags>>8)
	}
	if flags>>8&0x3 != 0 {
		t.Errorf("x/y presence bits = %#b, want clear (the window must not move)", flags>>8)
	}
	if flags>>12 != 2 {
		t.Errorf("source = %d, want 2 (pager)", flags>>12)
	}
	if flags&0xFF != 0 {
		t.Errorf("gravity = %d, want 0", flags&0xFF)
	}
	if got := u32(16); got != 0 || u32(20) != 0 {
		t.Errorf("x/y = %d/%d, want 0/0 (unused)", got, u32(20))
	}
	if got := u32(24); got != 1600 || u32(28) != 900 {
		t.Errorf("width/height = %dx%d, want 1600x900", got, u32(28))
	}
}

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
