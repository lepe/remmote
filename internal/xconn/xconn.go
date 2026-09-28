// Package xconn wraps a raw xgb connection with the facts both binaries
// need: the root window, screen geometry, pixel layout, and extension
// availability.
package xconn

import (
	"fmt"
	"os"
	"sync"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/damage"
	"github.com/jezek/xgb/randr"
	"github.com/jezek/xgb/shm"
	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"
)

// Conn is a ready-to-use X connection with cached screen facts.
type Conn struct {
	X *xgb.Conn

	// Ext reports which extensions the server provides. Populated by Dial.
	Ext Ext

	root     xproto.Window
	depth    byte
	visual   xproto.Visualid
	lsbFirst bool

	mu            sync.Mutex
	width, height uint16
}

// Ext lists the extensions remmote uses.
type Ext struct {
	DAMAGE bool // dirty-rect tracking
	SHM    bool // fast GetImage/PutImage through SysV shared memory
	RANDR  bool // screen resize notifications
	XTEST  bool // input injection
}

// Dial connects to display ("" means $DISPLAY) and initializes the
// extensions remmote needs. Only TrueColor roots at 24/32 depth with 32
// bits per pixel are supported; anything else fails fast.
func Dial(display string) (*Conn, error) {
	x, err := xgb.NewConnDisplay(display)
	if err != nil {
		return nil, fmt.Errorf("xconn: connect %q: %w", DisplayString(display), err)
	}
	setup := xproto.Setup(x)
	screen := setup.DefaultScreen(x)

	if screen.RootDepth != 24 && screen.RootDepth != 32 {
		x.Close()
		return nil, fmt.Errorf("xconn: unsupported root depth %d (need 24 or 32 TrueColor)", screen.RootDepth)
	}
	bpp := byte(0)
	for _, f := range setup.PixmapFormats {
		if f.Depth == screen.RootDepth {
			bpp = f.BitsPerPixel
		}
	}
	if bpp != 32 {
		x.Close()
		return nil, fmt.Errorf("xconn: depth %d has %d bits/pixel, need 32", screen.RootDepth, bpp)
	}

	c := &Conn{
		X:        x,
		root:     screen.Root,
		depth:    screen.RootDepth,
		visual:   screen.RootVisual,
		lsbFirst: setup.ImageByteOrder == xproto.ImageOrderLSBFirst,
		width:    screen.WidthInPixels,
		height:   screen.HeightInPixels,
	}
	c.Ext = Ext{
		DAMAGE: damage.Init(x) == nil,
		SHM:    shm.Init(x) == nil,
		RANDR:  randr.Init(x) == nil,
		XTEST:  xtest.Init(x) == nil,
	}
	return c, nil
}

// Root returns the root window id.
func (c *Conn) Root() xproto.Window { return c.root }

// Depth returns the root depth (24 or 32, both 4 bytes/pixel).
func (c *Conn) Depth() byte { return c.depth }

// Visual returns the root visual id (for CreateWindow with CopyFromParent
// fallbacks and GC creation).
func (c *Conn) Visual() xproto.Visualid { return c.visual }

// LSBFirst reports the server's ZPixmap byte order.
func (c *Conn) LSBFirst() bool { return c.lsbFirst }

// ScreenSize returns the current root window size.
func (c *Conn) ScreenSize() (w, h uint16) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.width, c.height
}

// SetScreenSize updates the cached root size (after a RANDR change).
func (c *Conn) SetScreenSize(w, h uint16) {
	c.mu.Lock()
	c.width, c.height = w, h
	c.mu.Unlock()
}

// Close closes the underlying connection.
func (c *Conn) Close() { c.X.Close() }

// DisplayString resolves "" to the effective $DISPLAY for messages.
func DisplayString(display string) string {
	if display == "" {
		return os.Getenv("DISPLAY")
	}
	return display
}
