package input

import (
	"testing"

	"github.com/jezek/xgb/xproto"
)

// The keycode is a byte and 255 is a legal maximum: a search that counts
// in keycodes wraps there and walks off the table. It used to panic on
// the first keysym that was not in column 0 — a capital, a colon — and
// it is the path every viewer keystroke takes on the host.
func TestFindKeysymAtTheTopOfTheKeycodeRange(t *testing.T) {
	const cols = 2
	syms := make([]xproto.Keysym, (255-8+1)*cols)
	// 'a' unshifted on the very last keycode; 'A' shifted just before it.
	syms[(255-8)*cols] = 'a'
	syms[(254-8)*cols+1] = 'A'
	k := &Keymap{minKC: 8, maxKC: 255, cols: cols, syms: syms}

	if kc, shift, ok := k.FindKeysym('a'); !ok || kc != 255 || shift {
		t.Fatalf("a: %d %v %v, want 255 false true", kc, shift, ok)
	}
	if kc, shift, ok := k.FindKeysym('A'); !ok || kc != 254 || !shift {
		t.Fatalf("A: %d %v %v, want 254 true true", kc, shift, ok)
	}
	if _, _, ok := k.FindKeysym(':'); ok {
		t.Fatal("a keysym the map does not have was found")
	}
}
