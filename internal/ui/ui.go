// Package ui is a small X11 widget layer: what a form needs and no more.
//
// Everything is drawn with core protocol calls — filled rectangles and
// the server's fixed font — into a back pixmap that is copied to the
// window when a screen is done painting. No toolkit, no fonts of our
// own, no CGo: the same plain-xgb world the viewer lives in, at the
// speed a form needs.
package ui

import (
	"fmt"
	"image"
	"log/slog"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"

	"github.com/lepe/remmote/internal/input"
	"github.com/lepe/remmote/internal/xconn"
)

// Color is a 24-bit RGB colour, as the screen's TrueColor visual wants.
type Color uint32

// A palette kept small on purpose: a form is read, not admired.
const (
	Bg       Color = 0x202428
	Panel    Color = 0x2a2f35
	PanelHi  Color = 0x343b42
	Fg       Color = 0xe8eaed
	Dim      Color = 0x9aa0a6
	Faint    Color = 0x6b7280
	Accent   Color = 0x4c8dff
	AccentFg Color = 0xffffff
	Danger   Color = 0xd9534f
	Ok       Color = 0x3fb950
	Border   Color = 0x444c56
	Input    Color = 0x1a1d21
)

const (
	// RowHeight is the line height of the fixed font, and the height of
	// the widgets that hold text.
	RowHeight = 22
	// FontWidth is how wide one character is in the fixed font.
	FontWidth = 6
	// FontHeight is the font's cell height (its baseline sits at 11).
	FontHeight = 13
	// TextBaseline is the distance from a row's top to the font baseline.
	TextBaseline = 15
)

// Painter draws into a window's back buffer. All coordinates are pixels
// from the window's top-left.
type Painter struct {
	conn  *xgb.Conn
	draw  xproto.Drawable
	gc    xproto.Gcontext
	w, h  int
	white xproto.Gcontext // for text, which wants its own colour
}

// Size is what is being painted on.
func (p *Painter) Size() (int, int) { return p.w, p.h }

// Fill paints a filled rectangle.
func (p *Painter) Fill(r image.Rectangle, c Color) {
	r = r.Intersect(image.Rect(0, 0, p.w, p.h))
	if r.Empty() {
		return
	}
	xproto.ChangeGC(p.conn, p.gc, xproto.GcForeground, []uint32{uint32(c)})
	xproto.PolyFillRectangle(p.conn, p.draw, p.gc,
		[]xproto.Rectangle{{X: int16(r.Min.X), Y: int16(r.Min.Y),
			Width: uint16(r.Dx()), Height: uint16(r.Dy())}})
}

// Rect paints a one-pixel outline.
func (p *Painter) Rect(r image.Rectangle, c Color) {
	xproto.ChangeGC(p.conn, p.gc, xproto.GcForeground, []uint32{uint32(c)})
	xproto.PolyRectangle(p.conn, p.draw, p.gc,
		[]xproto.Rectangle{{X: int16(r.Min.X), Y: int16(r.Min.Y),
			Width: uint16(max(r.Dx()-1, 0)), Height: uint16(max(r.Dy()-1, 0))}})
}

// Text draws a string from x, with y as the font's baseline. Tabs and
// newlines have no place on a form and are drawn as spaces.
func (p *Painter) Text(x, y int, s string, c Color) {
	if s == "" {
		return
	}
	xproto.ChangeGC(p.conn, p.white, xproto.GcForeground, []uint32{uint32(c)})
	if len(s) > 255 { // ImageText8's length is one byte
		s = s[:255]
	}
	xproto.ImageText8(p.conn, byte(len(s)), p.draw, p.white, int16(x), int16(y), s)
}

// TextWidth is how wide a string will be drawn.
func (p *Painter) TextWidth(s string) int { return len(s) * FontWidth }

// Truncate cuts a string to fit width, marking the cut with an ellipsis.
func Truncate(s string, width int) string {
	max := width / FontWidth
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}

// Handler is a screen: it paints itself and takes the events.
type Handler interface {
	Paint(p *Painter)
	OnKey(KeyEvent)
	OnMouse(MouseEvent)
	OnResize(width, height int)
}

// KeyEvent is a key press. A printable character arrives in Char (and
// its keysym is the one the X server mapped); navigation and editing
// keys get their own field.
type KeyEvent struct {
	Char   rune
	Keysym uint32
	Ctrl   bool
	Shift  bool
	Tab    bool
	Enter  bool
	Escape bool
	Bksp   bool
	Delete bool
	Left   bool
	Right  bool
	Up     bool
	Down   bool
	Home   bool
	End    bool
	PageUp bool
	PageDn bool
	Space  bool
}

// MouseKind says what the pointer did.
type MouseKind int

// The pointer events a form cares about.
const (
	MouseMove MouseKind = iota
	MousePress
	MouseRelease
	MouseWheelUp
	MouseWheelDown
)

// MouseEvent is a pointer event in window coordinates.
type MouseEvent struct {
	X, Y   int
	Kind   MouseKind
	Button int
}

// Window is a top-level window with a back buffer and an event loop.
type Window struct {
	xc         *xconn.Conn
	km         *input.Keymap
	win        xproto.Window
	gc         xproto.Gcontext
	white      xproto.Gcontext // text colour, changed per string
	pixmap     xproto.Pixmap
	deleteAtom xproto.Atom
	w, h       int
	log        *slog.Logger
}

// Open makes a window of w×h on display ("" means $DISPLAY).
func Open(display, title string, w, h int, log *slog.Logger) (*Window, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(nil, nil))
	}
	xc, err := xconn.Dial(display)
	if err != nil {
		return nil, err
	}
	km, err := input.LoadKeymap(xc.X)
	if err != nil {
		xc.Close()
		return nil, fmt.Errorf("ui: keymap: %w", err)
	}
	u := &Window{xc: xc, km: km, w: w, h: h, log: log}

	id, err := xc.X.NewId()
	if err != nil {
		u.Close()
		return nil, err
	}
	u.win = xproto.Window(id)
	back := uint32(Bg)
	if err := xproto.CreateWindowChecked(xc.X, xc.Depth(), u.win, xc.Root(),
		0, 0, uint16(w), uint16(h), 0,
		xproto.WindowClassInputOutput, 0,
		xproto.CwBackPixel|xproto.CwEventMask,
		[]uint32{back, eventMask}).Check(); err != nil {
		u.Close()
		return nil, fmt.Errorf("ui: CreateWindow: %w", err)
	}

	gcID, err := xc.X.NewId()
	if err != nil {
		u.Close()
		return nil, err
	}
	u.gc = xproto.Gcontext(gcID)
	if err := xproto.CreateGCChecked(xc.X, u.gc, xproto.Drawable(u.win), 0, nil).Check(); err != nil {
		u.Close()
		return nil, fmt.Errorf("ui: CreateGC: %w", err)
	}
	// The fixed font: a server font means no font of ours to ship, and
	// every X server has this one (or its 6x13 spelling). Text draws
	// through a second GC, so changing its colour never disturbs the
	// rectangles.
	font := xproto.Font(0)
	for _, name := range []string{"fixed", "6x13"} {
		id, ferr := xc.X.NewId()
		if ferr != nil {
			continue
		}
		fid := xproto.Font(id)
		if err := xproto.OpenFontChecked(xc.X, fid, uint16(len(name)), name).Check(); err == nil {
			font = fid
			break
		}
	}
	if font == 0 {
		u.Close()
		return nil, fmt.Errorf("ui: this X server has no fixed font")
	}
	whiteID, err := xc.X.NewId()
	if err != nil {
		u.Close()
		return nil, err
	}
	u.white = xproto.Gcontext(whiteID)
	if err := xproto.CreateGCChecked(xc.X, u.white, xproto.Drawable(u.win), 0, nil).Check(); err != nil {
		u.Close()
		return nil, fmt.Errorf("ui: CreateGC: %w", err)
	}
	xproto.ChangeGC(xc.X, u.gc, xproto.GcFont, []uint32{uint32(font)})
	xproto.ChangeGC(xc.X, u.white, xproto.GcFont, []uint32{uint32(font)})

	if err := u.makeBack(); err != nil {
		u.Close()
		return nil, err
	}
	if err := u.setWMProperties(title); err != nil {
		log.Warn("window manager properties failed (continuing)", "err", err)
	}
	if err := xproto.MapWindowChecked(xc.X, u.win).Check(); err != nil {
		u.Close()
		return nil, fmt.Errorf("ui: MapWindow: %w", err)
	}
	return u, nil
}

// eventMask is what a form wants to hear about.
const eventMask = xproto.EventMaskKeyPress | xproto.EventMaskKeyRelease |
	xproto.EventMaskButtonPress | xproto.EventMaskButtonRelease |
	xproto.EventMaskPointerMotion | xproto.EventMaskExposure |
	xproto.EventMaskStructureNotify

// makeBack (re)creates the back pixmap at the current size.
func (u *Window) makeBack() error {
	if u.pixmap != 0 {
		xproto.FreePixmap(u.xc.X, u.pixmap)
	}
	id, err := u.xc.X.NewId()
	if err != nil {
		return err
	}
	u.pixmap = xproto.Pixmap(id)
	return xproto.CreatePixmapChecked(u.xc.X, u.xc.Depth(), u.pixmap,
		xproto.Drawable(u.win), uint16(u.w), uint16(u.h)).Check()
}

// Painter builds a painter for the back buffer.
func (u *Window) Painter() *Painter {
	return &Painter{conn: u.xc.X, draw: xproto.Drawable(u.pixmap), gc: u.gc,
		w: u.w, h: u.h, white: u.white}
}

// Draw paints the screen into the back buffer and shows it.
func (u *Window) Draw(paint func(*Painter)) {
	p := u.Painter()
	// Every screen paints its own background: a back buffer carries no
	// state between paints.
	p.Fill(image.Rect(0, 0, u.w, u.h), Bg)
	paint(p)
	xproto.CopyArea(u.xc.X, xproto.Drawable(u.pixmap), xproto.Drawable(u.win), u.gc,
		0, 0, 0, 0, uint16(u.w), uint16(u.h))
}

// Size is the window's current size.
func (u *Window) Size() (int, int) { return u.w, u.h }

// Pump runs the event loop until the window closes or the screen says to
// stop, whichever comes first.
func (u *Window) Pump(h Handler) error {
	u.Draw(h.Paint)
	for {
		ev, err := u.xc.X.WaitForEvent()
		if err != nil {
			return fmt.Errorf("ui: %w", err)
		}
		if ev == nil {
			return nil // the connection is gone
		}
		switch e := ev.(type) {
		case xproto.KeyPressEvent:
			h.OnKey(u.keyEvent(e.State, e.Detail))
			u.Draw(h.Paint)
		case xproto.ExposeEvent:
			if e.Count == 0 {
				u.Draw(h.Paint)
			}
		case xproto.ConfigureNotifyEvent:
			if int(e.Width) != u.w || int(e.Height) != u.h {
				u.w, u.h = int(e.Width), int(e.Height)
				if err := u.makeBack(); err != nil {
					return err
				}
				h.OnResize(u.w, u.h)
				u.Draw(h.Paint)
			}
		case xproto.ButtonPressEvent:
			h.OnMouse(mouseEvent(e.EventX, e.EventY, MousePress, byte(e.Detail)))
			u.Draw(h.Paint)
		case xproto.ButtonReleaseEvent:
			kind := MouseRelease
			switch e.Detail {
			case 4:
				kind = MouseWheelUp
			case 5:
				kind = MouseWheelDown
			}
			h.OnMouse(mouseEvent(e.EventX, e.EventY, kind, byte(e.Detail)))
			u.Draw(h.Paint)
		case xproto.MotionNotifyEvent:
			h.OnMouse(mouseEvent(e.EventX, e.EventY, MouseMove, 0))
			u.Draw(h.Paint)
		case xproto.ClientMessageEvent:
			if u.isDeleteMessage(e) {
				return nil // the window manager closed the window
			}
		}
	}
}

// Close tears the window down.
func (u *Window) Close() {
	if u.xc == nil {
		return
	}
	if u.pixmap != 0 {
		xproto.FreePixmap(u.xc.X, u.pixmap)
	}
	u.xc.Close()
	u.xc = nil
}

// setWMProperties names the window and asks to be told when it closes.
func (u *Window) setWMProperties(title string) error {
	conn := u.xc.X
	protocols, err := xproto.InternAtom(conn, false, uint16(len("WM_PROTOCOLS")), "WM_PROTOCOLS").Reply()
	if err != nil {
		return err
	}
	del, err := xproto.InternAtom(conn, false, uint16(len("WM_DELETE_WINDOW")), "WM_DELETE_WINDOW").Reply()
	if err != nil {
		return err
	}
	u.deleteAtom = del.Atom

	var buf [4]byte
	buf[0], buf[1], buf[2], buf[3] = byte(del.Atom), byte(del.Atom>>8), byte(del.Atom>>16), byte(del.Atom>>24)
	if err := xproto.ChangePropertyChecked(conn, xproto.PropModeReplace, u.win,
		protocols.Atom, xproto.AtomAtom, 32, 1, buf[:]).Check(); err != nil {
		return err
	}
	name := []byte(title)
	return xproto.ChangePropertyChecked(conn, xproto.PropModeReplace, u.win,
		xproto.AtomWmName, xproto.AtomString, 8, uint32(len(name)), name).Check()
}

// isDeleteMessage reports whether the window manager is asking to close
// the window.
func (u *Window) isDeleteMessage(e xproto.ClientMessageEvent) bool {
	return xproto.Atom(e.Data.Data32[0]) == u.deleteAtom
}

// keyEvent turns a raw key press into a KeyEvent, with the keysym the
// keyboard's current mapping gives it (so Shift+A is 'A').
func (u *Window) keyEvent(state uint16, detail xproto.Keycode) KeyEvent {
	col := 0
	if state&xproto.ModMaskShift != 0 {
		col = 1
	}
	ev := KeyEvent{
		Ctrl:   state&xproto.ModMaskControl != 0,
		Shift:  state&xproto.ModMaskShift != 0,
		Keysym: uint32(u.km.KeysymAt(detail, col)),
	}
	switch ev.Keysym {
	case 0xff09:
		ev.Tab = true
	case 0xff0d, 0xff8d:
		ev.Enter = true
	case 0xff1b:
		ev.Escape = true
	case 0xff08:
		ev.Bksp = true
	case 0xffff, 0xff9f:
		ev.Delete = true
	case 0xff51:
		ev.Left = true
	case 0xff53:
		ev.Right = true
	case 0xff52:
		ev.Up = true
	case 0xff54:
		ev.Down = true
	case 0xff50:
		ev.Home = true
	case 0xff57:
		ev.End = true
	case 0xff55:
		ev.PageUp = true
	case 0xff56:
		ev.PageDn = true
	case 0x20:
		ev.Space = true
		ev.Char = ' '
	default:
		if ev.Keysym >= 0x20 && ev.Keysym <= 0x7e {
			ev.Char = rune(ev.Keysym)
		}
	}
	return ev
}

func mouseEvent(x, y int16, kind MouseKind, button byte) MouseEvent {
	return MouseEvent{X: int(x), Y: int(y), Kind: kind, Button: int(button)}
}
