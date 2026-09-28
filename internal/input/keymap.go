// Package input implements server-side input injection: translating X11
// keysyms to keycodes and replaying keyboard and pointer events through
// the XTEST extension.
package input

import (
	"fmt"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// X11 keysyms used by the injector. xgb's generated bindings do not ship
// keysym constants; these are from the X11 keysym spec.
const (
	KeyShiftL         xproto.Keysym = 0xffe1
	KeyShiftR         xproto.Keysym = 0xffe2
	KeyControlL       xproto.Keysym = 0xffe3
	KeyControlR       xproto.Keysym = 0xffe4
	KeyCapsLock       xproto.Keysym = 0xffe5
	KeyMetaL          xproto.Keysym = 0xffe7
	KeyMetaR          xproto.Keysym = 0xffe8
	KeyAltL           xproto.Keysym = 0xffe9
	KeyAltR           xproto.Keysym = 0xffea
	KeySuperL         xproto.Keysym = 0xffeb
	KeySuperR         xproto.Keysym = 0xffec
	KeyHyperL         xproto.Keysym = 0xffed
	KeyHyperR         xproto.Keysym = 0xffee
	KeyISOLevel3Shift xproto.Keysym = 0xfe03
)

// modifierKeysyms are pressed/released verbatim — the injector never
// synthesizes modifiers around them.
var modifierKeysyms = map[xproto.Keysym]bool{
	KeyShiftL: true, KeyShiftR: true,
	KeyControlL: true, KeyControlR: true,
	KeyCapsLock: true,
	KeyMetaL:    true, KeyMetaR: true,
	KeyAltL: true, KeyAltR: true,
	KeySuperL: true, KeySuperR: true,
	KeyHyperL: true, KeyHyperR: true,
	KeyISOLevel3Shift: true,
}

// Keymap is the server's keyboard mapping: a flat (keycode × column)
// table of keysyms. Column 0 is unshifted, 1 is shifted, 2/3 exist on
// some layouts (AltGr); 0 means NoSymbol.
type Keymap struct {
	minKC, maxKC xproto.Keycode
	cols         int
	syms         []xproto.Keysym
}

// LoadKeymap reads the server's full keysym table.
func LoadKeymap(x *xgb.Conn) (*Keymap, error) {
	setup := xproto.Setup(x)
	minKC, maxKC := setup.MinKeycode, setup.MaxKeycode
	if maxKC < minKC {
		return nil, fmt.Errorf("input: bad keycode range %d..%d", minKC, maxKC)
	}
	reply, err := xproto.GetKeyboardMapping(x, minKC, byte(maxKC-minKC+1)).Reply()
	if err != nil {
		return nil, fmt.Errorf("input: GetKeyboardMapping: %w", err)
	}
	cols := int(reply.KeysymsPerKeycode)
	if cols < 2 {
		cols = 2 // normalize so indexing never divides by zero
	}
	k := &Keymap{
		minKC: minKC,
		maxKC: maxKC,
		cols:  cols,
		syms:  make([]xproto.Keysym, int(maxKC-minKC+1)*cols),
	}
	copy(k.syms, reply.Keysyms)
	return k, nil
}

// KeysymAt returns the keysym at (keycode, column), or 0.
func (k *Keymap) KeysymAt(kc xproto.Keycode, col int) xproto.Keysym {
	if kc < k.minKC || kc > k.maxKC || col < 0 || col >= k.cols {
		return 0
	}
	return k.syms[(int(kc)-int(k.minKC))*k.cols+col]
}

// FindKeysym locates ks, preferring the lowest keycode at the lowest
// column. needShift is true when the keysym only exists in a shifted
// column (1..3).
func (k *Keymap) FindKeysym(ks xproto.Keysym) (kc xproto.Keycode, needShift bool, ok bool) {
	if ks == 0 {
		return 0, false, false
	}
	for kc := k.minKC; kc <= k.maxKC; kc++ {
		if k.syms[(int(kc)-int(k.minKC))*k.cols] == ks {
			return kc, false, true
		}
	}
	for col := 1; col < k.cols; col++ {
		for kc := k.minKC; kc <= k.maxKC; kc++ {
			if k.syms[(int(kc)-int(k.minKC))*k.cols+col] == ks {
				return kc, true, true
			}
		}
	}
	return 0, false, false
}
