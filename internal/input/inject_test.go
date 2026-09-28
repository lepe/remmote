package input

import (
	"io"
	"log/slog"
	"testing"

	"github.com/jezek/xgb/xproto"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testKeymap: keycode 8 = a/A, 9 = Shift_L, 10 = b/B, 11 = F1 (col 0 only).
func testKeymap() *Keymap {
	return &Keymap{
		minKC: 8, maxKC: 12, cols: 2,
		syms: []xproto.Keysym{
			0x61, 0x41, // 8: a A
			KeyShiftL, 0, // 9: Shift_L
			0x62, 0x42, // 10: b B
			0xffbe, 0, // 11: F1
			0, 0, // 12: unbound
		},
	}
}

type recorder struct {
	calls []fakeCall
	err   error
}

func (r *recorder) record(typ, detail byte, x, y int16) error {
	r.calls = append(r.calls, fakeCall{typ, detail, x, y})
	return r.err
}

func newTestInjector(t *testing.T) (*Injector, *recorder) {
	t.Helper()
	km := testKeymap()
	rec := &recorder{}
	inj := &Injector{
		km: km, sw: 100, sh: 50, log: testLogger(),
		held: map[xproto.Keysym]xproto.Keycode{},
		fake: rec.record,
	}
	return inj, rec
}

func TestKeyShiftChoreography(t *testing.T) {
	inj, rec := newTestInjector(t)
	inj.Key(0x41, true) // 'A' down
	inj.Key(0x41, false)

	want := []fakeCall{
		{evKeyPress, byte(9), 0, 0},   // synthetic Shift down
		{evKeyPress, byte(8), 0, 0},   // 'a' keycode with shift = 'A'
		{evKeyRelease, byte(8), 0, 0}, // 'A' up
		{evKeyRelease, byte(9), 0, 0}, // synthetic Shift up
	}
	assertCalls(t, rec.calls, want)
}

func TestKeyPlain(t *testing.T) {
	inj, rec := newTestInjector(t)
	inj.Key(0x61, true) // 'a' down — no shift
	inj.Key(0x61, false)
	assertCalls(t, rec.calls, []fakeCall{
		{evKeyPress, byte(8), 0, 0},
		{evKeyRelease, byte(8), 0, 0},
	})
}

func TestKeyUnknownDropped(t *testing.T) {
	inj, rec := newTestInjector(t)
	inj.Key(0x1234, true)
	inj.Key(0x1234, false)
	inj.Key(0, true) // NoSymbol
	assertCalls(t, rec.calls, nil)
}

func TestModifierDirect(t *testing.T) {
	inj, rec := newTestInjector(t)
	inj.Key(KeyShiftL, true) // user holds shift themselves
	inj.Key(0x41, true)      // 'A' — we must NOT press a second shift
	inj.Key(0x41, false)
	inj.Key(KeyShiftL, false)
	assertCalls(t, rec.calls, []fakeCall{
		{evKeyPress, byte(9), 0, 0},
		{evKeyPress, byte(8), 0, 0},
		{evKeyRelease, byte(8), 0, 0},
		{evKeyRelease, byte(9), 0, 0},
	})
}

func TestShiftHeldAcrossKeys(t *testing.T) {
	inj, rec := newTestInjector(t)
	inj.Key(0x41, true) // A down (shift+8 down)
	inj.Key(0x42, true) // B down (shift already held)
	inj.Key(0x42, false)
	inj.Key(0x41, false) // last key up → shift released
	assertCalls(t, rec.calls, []fakeCall{
		{evKeyPress, byte(9), 0, 0},
		{evKeyPress, byte(8), 0, 0},
		{evKeyPress, byte(10), 0, 0},
		{evKeyRelease, byte(10), 0, 0},
		{evKeyRelease, byte(8), 0, 0},
		{evKeyRelease, byte(9), 0, 0},
	})
}

func TestReleaseWithoutPressIsNoop(t *testing.T) {
	inj, rec := newTestInjector(t)
	inj.Key(0x61, false) // release for a key we never pressed
	assertCalls(t, rec.calls, nil)
}

func TestWheel(t *testing.T) {
	inj, rec := newTestInjector(t)
	inj.Wheel(0, -2) // two notches up
	inj.Wheel(1, 0)  // one notch right
	assertCalls(t, rec.calls, []fakeCall{
		{evButtonPress, 4, 0, 0}, {evButtonRelease, 4, 0, 0},
		{evButtonPress, 4, 0, 0}, {evButtonRelease, 4, 0, 0},
		{evButtonPress, 7, 0, 0}, {evButtonRelease, 7, 0, 0},
	})
}

func TestButton(t *testing.T) {
	inj, rec := newTestInjector(t)
	inj.Button(1, true)
	inj.Button(3, false)
	assertCalls(t, rec.calls, []fakeCall{
		{evButtonPress, 1, 0, 0},
		{evButtonRelease, 3, 0, 0},
	})
}

func TestMovePointerClamp(t *testing.T) {
	inj, rec := newTestInjector(t)
	inj.MovePointer(500, -3)
	inj.MovePointer(10, 20)
	assertCalls(t, rec.calls, []fakeCall{
		{evMotionNotify, 0, 99, 0},
		{evMotionNotify, 0, 10, 20},
	})
}

func TestFindKeysym(t *testing.T) {
	km := testKeymap()
	cases := []struct {
		ks        xproto.Keysym
		wantKC    xproto.Keycode
		wantShift bool
		wantOK    bool
	}{
		{0x61, 8, false, true},
		{0x41, 8, true, true},
		{0x62, 10, false, true},
		{0x42, 10, true, true},
		{0xffbe, 11, false, true},
		{KeyShiftL, 9, false, true},
		{0x1234, 0, false, false},
		{0, 0, false, false},
	}
	for _, tc := range cases {
		kc, shift, ok := km.FindKeysym(tc.ks)
		if kc != tc.wantKC || shift != tc.wantShift || ok != tc.wantOK {
			t.Errorf("FindKeysym(%#x) = (%d,%v,%v), want (%d,%v,%v)",
				tc.ks, kc, shift, ok, tc.wantKC, tc.wantShift, tc.wantOK)
		}
	}
}

func TestKeysymAtBounds(t *testing.T) {
	km := testKeymap()
	if got := km.KeysymAt(8, 1); got != 0x41 {
		t.Errorf("KeysymAt(8,1) = %#x", got)
	}
	if got := km.KeysymAt(7, 0); got != 0 { // below minKC
		t.Errorf("KeysymAt(7,0) = %#x", got)
	}
	if got := km.KeysymAt(12, 0); got != 0 { // unbound keycode
		t.Errorf("KeysymAt(12,0) = %#x", got)
	}
	if got := km.KeysymAt(8, 5); got != 0 { // column beyond table
		t.Errorf("KeysymAt(8,5) = %#x", got)
	}
}

func assertCalls(t *testing.T, got, want []fakeCall) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("call %d = %+v, want %+v (all: %v)", i, got[i], want[i], got)
		}
	}
}
