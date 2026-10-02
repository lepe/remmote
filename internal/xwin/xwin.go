// Package xwin provides X round-trip helpers for window management
// queries (properties, tree walks, geometry, event-mask selection) plus
// pure membership predicates used to decide which windows belong to a
// shared application. Everything here issues requests-with-replies —
// never call it from the event pump goroutine.
package xwin

import (
	"encoding/binary"
	"fmt"
	"image"
	"sync"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/randr"
	"github.com/jezek/xgb/xproto"
)

// Client caches atoms and answers window queries on one connection.
type Client struct {
	x    *xgb.Conn
	root xproto.Window

	mu    sync.Mutex
	atoms map[string]xproto.Atom

	// madeModes caches the exact-size modes created for virtual outputs
	// (see createMode), keyed by output and size.
	madeModes map[string]randr.Mode
}

// NewClient wraps a connection and its root window.
func NewClient(x *xgb.Conn, root xproto.Window) *Client {
	return &Client{x: x, root: root,
		atoms:     make(map[string]xproto.Atom),
		madeModes: make(map[string]randr.Mode)}
}

// Root returns the root window queries are anchored to.
func (c *Client) Root() xproto.Window { return c.root }

// Atom interns (and caches) a property/selection atom name.
func (c *Client) Atom(name string) xproto.Atom {
	c.mu.Lock()
	if a, ok := c.atoms[name]; ok {
		c.mu.Unlock()
		return a
	}
	c.mu.Unlock()
	r, err := xproto.InternAtom(c.x, false, uint16(len(name)), name).Reply()
	if err != nil {
		return 0
	}
	c.mu.Lock()
	c.atoms[name] = r.Atom
	c.mu.Unlock()
	return r.Atom
}

// prop reads up to 64 bytes of a property with any type.
func (c *Client) prop(w xproto.Window, name string) (data []byte, typ xproto.Atom, err error) {
	a := c.Atom(name)
	if a == 0 {
		return nil, 0, fmt.Errorf("xwin: atom %q unavailable", name)
	}
	// LongLength is in 4-byte units; 16 longs = 64 bytes is plenty for
	// every property we read.
	r, err := xproto.GetProperty(c.x, false, w, a, 0, 0, 16).Reply()
	if err != nil {
		return nil, 0, err
	}
	return r.Value, r.Type, nil
}

// PropUint32 reads a CARDINAL/WINDOW (format 32) property. Values set by
// x86 Xlib clients are little-endian; xgb returns raw bytes.
func (c *Client) PropUint32(w xproto.Window, name string) (uint32, bool) {
	data, typ, err := c.prop(w, name)
	if err != nil || typ == 0 || len(data) < 4 {
		return 0, false
	}
	return binary.LittleEndian.Uint32(data), true
}

// WMClass returns the res_instance and res_class strings of WM_CLASS.
func (c *Client) WMClass(w xproto.Window) (instance, class string, ok bool) {
	data, typ, err := c.prop(w, "WM_CLASS")
	if err != nil || typ == 0 || len(data) == 0 {
		return "", "", false
	}
	instance, class = ParseWMClass(data)
	return instance, class, instance != "" || class != ""
}

// WMName returns the window's WM_NAME — the title a window list shows.
// The property is STRING or UTF8_STRING; both read as bytes here, which
// is exact for the common case and close enough for a picker on the rest.
func (c *Client) WMName(w xproto.Window) (string, bool) {
	data, typ, err := c.prop(w, "WM_NAME")
	if err != nil || typ == 0 || len(data) == 0 {
		return "", false
	}
	return string(data), true
}

// HasWMState reports whether the window carries WM_STATE — the EWMH mark
// of a WM-managed client window (as opposed to a frame or decoration).
func (c *Client) HasWMState(w xproto.Window) bool {
	_, typ, err := c.prop(w, "WM_STATE")
	return err == nil && typ != 0
}

// Parent returns the window's parent via QueryTree.
func (c *Client) Parent(w xproto.Window) (xproto.Window, error) {
	r, err := xproto.QueryTree(c.x, w).Reply()
	if err != nil {
		return 0, err
	}
	return r.Parent, nil
}

// Tree lists all windows reachable from the root within depth 3 (root →
// frame → client → client-child covers every layout we support).
func (c *Client) Tree() []xproto.Window {
	var out []xproto.Window
	seen := make(map[xproto.Window]bool)
	var walk func(w xproto.Window, depth int)
	walk = func(w xproto.Window, depth int) {
		if depth > 3 || seen[w] {
			return
		}
		seen[w] = true
		out = append(out, w)
		r, err := xproto.QueryTree(c.x, w).Reply()
		if err != nil {
			return
		}
		for _, child := range r.Children {
			walk(child, depth+1)
		}
	}
	walk(c.root, 0)
	return out
}

// Stack returns the root's direct children in bottom-to-top stacking
// order.
func (c *Client) Stack() []xproto.Window {
	r, err := xproto.QueryTree(c.x, c.root).Reply()
	if err != nil {
		return nil
	}
	return r.Children
}

// RootRect computes a window's interior rect in root coordinates via
// TranslateCoordinates + GetGeometry. GetGeometry's X/Y are
// parent-relative and therefore unusable for reparented clients.
func (c *Client) RootRect(w xproto.Window) (image.Rectangle, error) {
	tr, err := xproto.TranslateCoordinates(c.x, w, c.root, 0, 0).Reply()
	if err != nil {
		return image.Rectangle{}, fmt.Errorf("xwin: translate %#x: %w", uint32(w), err)
	}
	if !tr.SameScreen {
		return image.Rectangle{}, fmt.Errorf("xwin: window %#x is on another screen", uint32(w))
	}
	g, err := xproto.GetGeometry(c.x, xproto.Drawable(w)).Reply()
	if err != nil {
		return image.Rectangle{}, fmt.Errorf("xwin: geometry %#x: %w", uint32(w), err)
	}
	return image.Rect(int(tr.DstX), int(tr.DstY), int(tr.DstX)+int(g.Width), int(tr.DstY)+int(g.Height)), nil
}

// IsViewable reports whether the window is mapped and viewable.
func (c *Client) IsViewable(w xproto.Window) bool {
	a, err := xproto.GetWindowAttributes(c.x, w).Reply()
	if err != nil {
		return false
	}
	return a.MapState == xproto.MapStateViewable
}

// HasWM reports whether a window manager is running. EWMH defines
// _NET_SUPPORTING_WM_CHECK on the root as its presence marker, and it is
// the difference between requests a WM honours and requests nobody reads.
func (c *Client) HasWM() bool {
	a := c.Atom("_NET_SUPPORTING_WM_CHECK")
	if a == 0 {
		return false
	}
	r, err := xproto.GetProperty(c.x, false, c.root, a, 0, 0, 1).Reply()
	if err != nil {
		return false
	}
	return len(r.Value) >= 4
}

// Maximize makes w fill the screen. With a window manager this is an EWMH
// request, which is what keeps the window in a real maximized state and
// accounts for its decorations and panels; without one (bare Xvfb, and
// therefore the integration tests) nobody reads that request, so the
// window is resized to the screen outright instead. Both are one-shot:
// the geometry follows asynchronously via ConfigureNotify.
// Maximize makes w fill the screen, and reports how the request was made
// ("ewmh" or "resize") so a caller can log which path its display took.
// With a window manager this is an EWMH request, which is what keeps the
// window in a real maximized state and accounts for its decorations and
// panels; without one (bare Xvfb, and therefore the integration tests)
// nobody reads that request, so the window is resized to the screen
// outright instead. Both are one-shot requests: the geometry follows
// asynchronously via ConfigureNotify.
func (c *Client) Maximize(w xproto.Window) (string, error) {
	if !c.HasWM() {
		g, err := xproto.GetGeometry(c.x, xproto.Drawable(c.root)).Reply()
		if err != nil {
			return "resize", fmt.Errorf("xwin: root geometry: %w", err)
		}
		return "resize", xproto.ConfigureWindowChecked(c.x, w,
			xproto.ConfigWindowX|xproto.ConfigWindowY|
				xproto.ConfigWindowWidth|xproto.ConfigWindowHeight,
			[]uint32{0, 0, uint32(g.Width), uint32(g.Height)}).Check()
	}
	state := c.Atom("_NET_WM_STATE")
	vert := c.Atom("_NET_WM_STATE_MAXIMIZED_VERT")
	horz := c.Atom("_NET_WM_STATE_MAXIMIZED_HORZ")
	if state == 0 || vert == 0 || horz == 0 {
		return "ewmh", fmt.Errorf("xwin: EWMH _NET_WM_STATE unavailable")
	}
	// Addressed to the root's substructure, where a WM is listening.
	return "ewmh", xproto.SendEventChecked(c.x, false, c.root,
		xproto.EventMaskSubstructureRedirect|xproto.EventMaskSubstructureNotify,
		string(maximizeClientMessage(w, state, vert, horz))).Check()
}

// maximizeClientMessage builds the 32-byte ClientMessage that asks a
// window manager to add _NET_WM_STATE_MAXIMIZED_{VERT,HORZ} to w.
func maximizeClientMessage(w xproto.Window, state, vert, horz xproto.Atom) []byte {
	return wmStateClientMessage(w, state, wmStateAdd, vert, horz)
}

// wmStateClientMessage builds the 32-byte _NET_WM_STATE ClientMessage.
//
// Layout: code(1) format(1) seq(2) window(4) type(4) data(20), where the
// data is [action, arg1, arg2, source, pad]. The event is built by hand
// because xgb's ClientMessageData union only serialises its raw Data8
// bytes — its Bytes() panics on anything else — so the five 32-bit fields
// have to be packed explicitly. Little-endian, for the same reason as
// sendSelectionNotify in internal/clipboard: xgb negotiates an LSB-first
// connection, as do x86 Xlib clients.
func wmStateClientMessage(w xproto.Window, state xproto.Atom, action uint32, vert, horz xproto.Atom) []byte {
	b := make([]byte, 32)
	b[0] = 33 // ClientMessage
	b[1] = 32 // format: 32-bit
	binary.LittleEndian.PutUint32(b[4:8], uint32(w))
	binary.LittleEndian.PutUint32(b[8:12], uint32(state))
	binary.LittleEndian.PutUint32(b[12:16], action)
	binary.LittleEndian.PutUint32(b[16:20], uint32(vert))
	binary.LittleEndian.PutUint32(b[20:24], uint32(horz))
	binary.LittleEndian.PutUint32(b[24:28], 1) // source indication: application
	return b
}

// SelectStructureNotify ORs StructureNotify into the window's existing
// event mask (clobbering the mask would break the app's own selections).
func (c *Client) SelectStructureNotify(w xproto.Window) error {
	a, err := xproto.GetWindowAttributes(c.x, w).Reply()
	if err != nil {
		return err
	}
	mask := a.YourEventMask | xproto.EventMaskStructureNotify
	return xproto.ChangeWindowAttributesChecked(c.x, w, xproto.CwEventMask,
		[]uint32{mask}).Check()
}

// SelectSubstructureNotifyRoot ORs SubstructureNotify into the root's
// event mask so we hear about every new top-level window.
func (c *Client) SelectSubstructureNotifyRoot() error {
	a, err := xproto.GetWindowAttributes(c.x, c.root).Reply()
	if err != nil {
		return err
	}
	mask := a.YourEventMask | xproto.EventMaskSubstructureNotify
	return xproto.ChangeWindowAttributesChecked(c.x, c.root, xproto.CwEventMask,
		[]uint32{mask}).Check()
}

// FindClientWindows walks the tree and returns every window carrying
// WM_STATE (the WM-managed client windows).
func (c *Client) FindClientWindows() []xproto.Window {
	var out []xproto.Window
	for _, w := range c.Tree() {
		if w != c.root && c.HasWMState(w) {
			out = append(out, w)
		}
	}
	return out
}
