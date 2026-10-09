package stream

// Window-mode input integration tests: a real X server, a real shared
// window, and the router the session drives — a click and a keystroke
// must arrive at that window. Unit tests script the router against a
// fake WindowMap; these answer the question the unit tests cannot: does
// a window-mode stream's input actually reach the window being shared,
// including when another window is stacked above it?
//
// Like input's X tests, they need a disposable X server
// (REMMOTE_TEST_DISPLAY) and skip without one.

import (
	"bytes"
	"context"
	"image"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"

	"github.com/lepe/remmote/internal/proto"
	"github.com/lepe/remmote/internal/xconn"
)

func testDisplay(t *testing.T) string {
	t.Helper()
	display := os.Getenv("REMMOTE_TEST_DISPLAY")
	if display == "" {
		t.Skip("set REMMOTE_TEST_DISPLAY to a disposable Xvfb display")
	}
	return display
}

// observer owns the windows these tests share and reports the input
// events that reach them — on its own connection, so the stream's capture
// and input connections stay free to do their own work.
type observer struct {
	xc   *xconn.Conn
	evs  chan xgb.Event
	wins map[xproto.Window]bool
}

func newObserver(t *testing.T, display string) *observer {
	t.Helper()
	xc, err := xconn.Dial(display)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(xc.Close)
	o := &observer{xc: xc, evs: make(chan xgb.Event, 64), wins: map[xproto.Window]bool{}}
	go func() {
		defer close(o.evs)
		for {
			ev, err := xc.X.WaitForEvent()
			if ev == nil || err != nil {
				return
			}
			o.evs <- ev
		}
	}()
	return o
}

// window creates and maps one window selecting the input events these
// tests look for. The id is kept so events can be attributed to it.
func (o *observer) window(t *testing.T, x, y int16, w, h uint16) xproto.Window {
	t.Helper()
	id, err := o.xc.X.NewId()
	if err != nil {
		t.Fatal(err)
	}
	win := xproto.Window(id)
	if err := xproto.CreateWindowChecked(o.xc.X, o.xc.Depth(), win, o.xc.Root(),
		x, y, w, h, 0, xproto.WindowClassInputOutput, 0, xproto.CwEventMask,
		[]uint32{xproto.EventMaskButtonPress | xproto.EventMaskButtonRelease |
			xproto.EventMaskKeyPress | xproto.EventMaskKeyRelease}).Check(); err != nil {
		t.Fatal(err)
	}
	if err := xproto.MapWindowChecked(o.xc.X, win).Check(); err != nil {
		t.Fatal(err)
	}
	o.wins[win] = true
	return win
}

// rect reports a window's interior rectangle in root coordinates.
func (o *observer) rect(t *testing.T, win xproto.Window) image.Rectangle {
	t.Helper()
	tr, err := xproto.TranslateCoordinates(o.xc.X, win, o.xc.Root(), 0, 0).Reply()
	if err != nil {
		t.Fatal(err)
	}
	g, err := xproto.GetGeometry(o.xc.X, xproto.Drawable(win)).Reply()
	if err != nil {
		t.Fatal(err)
	}
	return image.Rect(int(tr.DstX), int(tr.DstY), int(tr.DstX)+int(g.Width), int(tr.DstY)+int(g.Height))
}

// inputEvents reports which of the tracked windows got a press or a
// keystroke within the deadline.
func (o *observer) inputEvents(t *testing.T, wait time.Duration) map[xproto.Window][]string {
	t.Helper()
	got := map[xproto.Window][]string{}
	deadline := time.After(wait)
	for {
		select {
		case ev := <-o.evs:
			var win xproto.Window
			var what string
			switch e := ev.(type) {
			case xproto.ButtonPressEvent:
				win, what = e.Event, "press"
			case xproto.KeyPressEvent:
				win, what = e.Event, "key"
			default:
				continue
			}
			if !o.wins[win] {
				continue
			}
			got[win] = append(got[win], what)
			if len(got[win]) >= 2 { // a press and a key: enough to judge
				return got
			}
		case <-deadline:
			return got
		}
	}
}

// share starts a window-mode stream on the display for win.
func share(t *testing.T, display string, win xproto.Window) (*Stream, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	st, err := New(Options{Display: display, Window: uint32(win),
		FPS: 30, Quality: 75, Codec: proto.CodecJPEG, FullRefresh: time.Second},
		slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("stream: %v\n%s", err, logs.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { defer close(runDone); _ = st.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-runDone; st.Close() })
	return st, &logs
}

// clickAndType drives the stream's input front-end the way a viewer
// would: position, click, type.
func clickAndType(st *Stream, x, y int) {
	st.rt.MovePointer(x, y)
	st.rt.Button(1, true)
	st.rt.Button(1, false)
	st.rt.Key(0x61, true) // 'a'
	st.rt.Key(0x61, false)
	st.ixc.X.Sync()
}

func TestWindowModeInputReachesTheSharedWindow(t *testing.T) {
	display := testDisplay(t)
	obs := newObserver(t, display)
	// The window the session shares, alone on the screen.
	shared := obs.window(t, 100, 80, 200, 150)
	st, logs := share(t, display, shared)

	// Click and type at the middle of the canvas: with one window that is
	// inside the shared window, wherever the quantized canvas puts it.
	r := st.ScreenRect()
	t.Logf("canvas %v", r)
	clickAndType(st, r.Dx()/2, r.Dy()/2)

	got := obs.inputEvents(t, 3*time.Second)
	if len(got[shared]) < 2 {
		t.Fatalf("input never reached the shared window: got %v\n%s", got, logs.String())
	}
}

func TestWindowModeClickReachesTheSharedWindowUnderAnOccluder(t *testing.T) {
	display := testDisplay(t)
	obs := newObserver(t, display)
	// The shared window, and something else stacked above it — on a real
	// desktop that is any other window, or the viewer itself. Both are
	// sized to most of the screen, so no placement policy can keep them
	// apart and the covered region is always real.
	sw, sh := obs.xc.ScreenSize()
	shared := obs.window(t, 0, 0, uint16(int(sw)*7/10), uint16(int(sh)*7/10))
	obs.window(t, int16(int(sw)/5), int16(int(sh)/5), uint16(int(sw)*7/10), uint16(int(sh)*7/10))
	st, logs := share(t, display, shared)

	// Where the canvas (0,0) is in root coordinates — the offset every
	// canvas coordinate gets when it becomes a host pointer position.
	r := st.ScreenRect()
	t.Logf("canvas %v", r)
	st.rt.MovePointer(0, 0)
	st.ixc.X.Sync()
	q, err := xproto.QueryPointer(obs.xc.X, obs.xc.Root()).Reply()
	if err != nil {
		t.Fatal(err)
	}
	origin := image.Pt(int(q.RootX), int(q.RootY))
	sharedRect := obs.rect(t, shared)
	var occRect image.Rectangle
	for w := range obs.wins {
		if w != shared {
			occRect = obs.rect(t, w)
		}
	}
	cover := sharedRect.Intersect(occRect)
	t.Logf("shared %v, occluder %v, cover %v", sharedRect, occRect, cover)
	if cover.Empty() {
		t.Skipf("the windows do not overlap on this setup: %v / %v", sharedRect, occRect)
	}
	// Click in the middle of the covered part: the point where a remote
	// pointer must reach the shared window even though another one is
	// stacked over it.
	at := cover.Min.Add(cover.Max).Div(2)
	click := at.Sub(origin)
	if !at.In(sharedRect) || !at.In(occRect) {
		t.Fatalf("test setup: covered point %v is not covered (%v / %v)", at, sharedRect, occRect)
	}

	clickAndType(st, click.X, click.Y)
	got := obs.inputEvents(t, 3*time.Second)
	t.Logf("input went to: %v", got)
	if len(got[shared]) < 2 {
		t.Fatalf("input did not reach the shared window under an occluder: got %v\n%s",
			got, logs.String())
	}
}
