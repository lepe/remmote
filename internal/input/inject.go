package input

import (
	"log/slog"
	"sync"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"
)

// FakeInput event types (xproto ships no constants for these).
const (
	evKeyPress      = 2
	evKeyRelease    = 3
	evButtonPress   = 4
	evButtonRelease = 5
	evMotionNotify  = 6
)

// fakeCall records one FakeInput request for tests.
type fakeCall struct {
	typ, detail byte
	x, y        int16
}

// Injector replays keyboard and pointer events through XTEST. All events
// for one client must be serialized; the server does this with one
// Injector per process guarded internally, preserving press/release order.
type Injector struct {
	x      *xgb.Conn
	km     *Keymap
	sw, sh uint16 // screen size for pointer clamping
	log    *slog.Logger

	mu        sync.Mutex
	held      map[xproto.Keysym]xproto.Keycode // keysyms currently down
	shiftHeld int                              // Shift presses outstanding (user's or ours)
	autoShift bool                             // true while the outstanding Shift is ours

	// fake is the XTEST indirection; tests replace it with a recorder.
	fake func(typ, detail byte, x, y int16) error
}

// NewInjector verifies XTEST and returns an Injector clamping pointer
// coordinates to the given screen size.
func NewInjector(x *xgb.Conn, km *Keymap, screenW, screenH uint16, log *slog.Logger) (*Injector, error) {
	if _, err := xtest.GetVersion(x, 2, 2).Reply(); err != nil {
		return nil, err
	}
	i := &Injector{
		x: x, km: km, sw: screenW, sh: screenH, log: log,
		held: make(map[xproto.Keysym]xproto.Keycode),
	}
	setup := xproto.Setup(x)
	root := setup.DefaultScreen(x).Root
	i.fake = func(typ, detail byte, px, py int16) error {
		return xtest.FakeInputChecked(i.x, typ, detail, 0, root, px, py, 0).Check()
	}
	return i, nil
}

// MovePointer warps the pointer to absolute screen coordinates (clamped).
func (i *Injector) MovePointer(x, y int) {
	x = clampInt(x, 0, int(i.sw)-1)
	y = clampInt(y, 0, int(i.sh)-1)
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.fake(evMotionNotify, 0, int16(x), int16(y)); err != nil {
		i.log.Warn("FakeInput: motion", "x", x, "y", y, "err", err)
	}
}

// Button presses (down) or releases a button. 1-3 are pointer buttons;
// 4-7 are wheel up/down/left/right — XTest delivers them as plain button
// events, so the caller sends press and its release.
func (i *Injector) Button(b uint8, down bool) {
	typ := byte(evButtonPress)
	if !down {
		typ = evButtonRelease
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.fake(typ, b, 0, 0); err != nil {
		i.log.Warn("FakeInput: button", "button", b, "down", down, "err", err)
	}
}

// Wheel scrolls by notches: dy<0 = up (button 4), dy>0 = down (5),
// dx<0 = left (6), dx>0 = right (7).
func (i *Injector) Wheel(dx, dy int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	for n := 0; n < absInt(dy); n++ {
		b := uint8(4) // up
		if dy > 0 {
			b = 5 // down
		}
		i.notch(b)
	}
	for n := 0; n < absInt(dx); n++ {
		b := uint8(6) // left
		if dx > 0 {
			b = 7 // right
		}
		i.notch(b)
	}
}

func (i *Injector) notch(b uint8) {
	if err := i.fake(evButtonPress, b, 0, 0); err != nil {
		i.log.Warn("FakeInput: wheel press", "button", b, "err", err)
	}
	if err := i.fake(evButtonRelease, b, 0, 0); err != nil {
		i.log.Warn("FakeInput: wheel release", "button", b, "err", err)
	}
}

// Key presses or releases a keysym. Shifted keysyms (uppercase, symbols)
// get a synthetic Shift press before and a release after — VNC-style
// choreography that never corrupts the local user's real modifier state,
// because we only release what we pressed.
func (i *Injector) Key(ks xproto.Keysym, down bool) {
	if ks == 0 { // NoSymbol: never inject
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()

	if modifierKeysyms[ks] {
		kc, _, ok := i.km.FindKeysym(ks)
		if !ok {
			return
		}
		typ := byte(evKeyPress)
		if !down {
			typ = evKeyRelease
		}
		if err := i.fake(typ, byte(kc), 0, 0); err != nil {
			i.log.Warn("FakeInput: modifier", "keysym", ks, "err", err)
		}
		if ks == KeyShiftL || ks == KeyShiftR {
			if down {
				i.shiftHeld++
			} else if i.shiftHeld > 0 {
				i.shiftHeld--
			}
		}
		return
	}

	if down {
		kc, needShift, ok := i.km.FindKeysym(ks)
		if !ok {
			i.log.Debug("keysym has no keycode on this server; dropped", "keysym", ks)
			return
		}
		// Only synthesize Shift when none is already held — the user's own
		// Shift (pressed via the modifier path above) serves just as well.
		if needShift && i.shiftHeld == 0 {
			if skc, _, ok := i.km.FindKeysym(KeyShiftL); ok {
				if i.fake(evKeyPress, byte(skc), 0, 0) == nil {
					i.autoShift = true
					i.shiftHeld = 1
				}
			}
		}
		if i.fake(evKeyPress, byte(kc), 0, 0) == nil {
			i.held[ks] = kc
		}
		return
	}

	kc, ok := i.held[ks]
	if !ok {
		return // unknown or already-up key: nothing to release
	}
	delete(i.held, ks)
	if err := i.fake(evKeyRelease, byte(kc), 0, 0); err != nil {
		i.log.Warn("FakeInput: key release", "keysym", ks, "err", err)
	}
	// Release only the Shift we pressed ourselves, once nothing else we
	// shifted for remains held.
	if i.autoShift && len(i.held) == 0 {
		if skc, _, ok := i.km.FindKeysym(KeyShiftL); ok {
			if err := i.fake(evKeyRelease, byte(skc), 0, 0); err != nil {
				i.log.Warn("FakeInput: shift release", "err", err)
			}
		}
		i.autoShift = false
		if i.shiftHeld > 0 {
			i.shiftHeld--
		}
	}
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
