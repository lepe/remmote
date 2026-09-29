package input

import (
	"os"
	"testing"
	"time"

	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"
	"github.com/lepe/remmote/internal/xconn"
)

// These tests must use a disposable X server, never the user's desktop.
func testX11(t testing.TB) *xconn.Conn {
	t.Helper()
	display := os.Getenv("REMMOTE_TEST_DISPLAY")
	if display == "" {
		t.Skip("set REMMOTE_TEST_DISPLAY to a disposable Xvfb display")
	}
	xc, err := xconn.Dial(display)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(xc.Close)
	return xc
}

func TestX11OrderedKeyboardDelivery(t *testing.T) {
	host := testX11(t)
	injConn := testX11(t)
	id, err := host.X.NewId()
	if err != nil {
		t.Fatal(err)
	}
	win := xproto.Window(id)
	if err := xproto.CreateWindowChecked(host.X, host.Depth(), win, host.Root(), 0, 0, 100, 100, 0, xproto.WindowClassInputOutput, 0, xproto.CwEventMask, []uint32{xproto.EventMaskKeyPress | xproto.EventMaskKeyRelease}).Check(); err != nil {
		t.Fatal(err)
	}
	if err := xproto.MapWindowChecked(host.X, win).Check(); err != nil {
		t.Fatal(err)
	}
	if err := xproto.SetInputFocusChecked(host.X, 2, win, xproto.TimeCurrentTime).Check(); err != nil {
		t.Fatal(err)
	}
	km, err := LoadKeymap(injConn.X)
	if err != nil {
		t.Fatal(err)
	}
	inj, err := NewInjector(injConn.X, km, 100, 100, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan byte, 128)
	go func() {
		for {
			ev, err := host.X.WaitForEvent()
			if err != nil || ev == nil {
				return
			}
			switch ev.(type) {
			case xproto.KeyPressEvent:
				got <- evKeyPress
			case xproto.KeyReleaseEvent:
				got <- evKeyRelease
			}
		}
	}()
	start := time.Now()
	for n := 0; n < 32; n++ {
		inj.Key(0x61, true)
		inj.Key(0x61, false)
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for n := 0; n < 64; n++ {
		select {
		case typ := <-got:
			want := byte(evKeyPress)
			if n%2 == 1 {
				want = evKeyRelease
			}
			if typ != want {
				t.Fatalf("event %d: got %d want %d", n, typ, want)
			}
		case <-timer.C:
			t.Fatalf("only received %d/64 keyboard events", n)
		}
	}
	t.Logf("32 key press/release pairs delivered in %v", time.Since(start))
}

// Both cases include a final X sync: the measured batch has actually been
// processed, rather than just added to a local request queue.
func BenchmarkX11Input(b *testing.B) {
	for _, checked := range []bool{true, false} {
		name := "ordered_async"
		if checked {
			name = "roundtrip_per_event"
		}
		b.Run(name, func(b *testing.B) {
			xc := testX11(b)
			if _, err := xtest.GetVersion(xc.X, 2, 2).Reply(); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				if checked {
					if err := xtest.FakeInputChecked(xc.X, evMotionNotify, 0, 0, xc.Root(), int16(n%100), 10, 0).Check(); err != nil {
						b.Fatal(err)
					}
				} else {
					xtest.FakeInput(xc.X, evMotionNotify, 0, 0, xc.Root(), int16(n%100), 10, 0)
				}
			}
			xc.X.Sync()
		})
	}
}
