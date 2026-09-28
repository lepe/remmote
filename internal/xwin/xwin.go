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
	"github.com/jezek/xgb/xproto"
)

// Client caches atoms and answers window queries on one connection.
type Client struct {
	x    *xgb.Conn
	root xproto.Window

	mu    sync.Mutex
	atoms map[string]xproto.Atom
}

// NewClient wraps a connection and its root window.
func NewClient(x *xgb.Conn, root xproto.Window) *Client {
	return &Client{x: x, root: root, atoms: make(map[string]xproto.Atom)}
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
