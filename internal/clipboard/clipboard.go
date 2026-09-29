// Package clipboard synchronizes UTF-8 text between the X11 CLIPBOARD
// selection of one display and the remote peer. It opens its own X
// connection with a hidden 1×1 window: a ticker requests the current
// selection (UTF8_STRING) every interval — unless we own the selection
// ourselves, which SelectionClear tells us — and the event loop both
// reads the answers and serves selection requests with remote text.
//
// Loop prevention: everything we pushed or pulled last is remembered; a
// local change is only forwarded when it differs. Text is the only
// format; transfers needing INCR (larger than proto.MaxClipboard) are
// skipped.
package clipboard

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"

	"github.com/lepe/remmote/internal/proto"
	"github.com/lepe/remmote/internal/xconn"
)

const eventSelectionNotify = 31 // SelectionNotify event code

// Watcher watches and serves one display's CLIPBOARD.
type Watcher struct {
	x    *xgb.Conn
	root xproto.Window
	win  xproto.Window
	log  *slog.Logger

	atomClipboard xproto.Atom
	atomUTF8      xproto.Atom
	atomTEXT      xproto.Atom
	atomSTRING    xproto.Atom
	atomTargets   xproto.Atom
	atomIncr      xproto.Atom
	propAtom      xproto.Atom

	mu      sync.Mutex
	served  []byte // text we currently serve as selection owner
	synced  []byte // last content seen going either direction
	amOwner bool   // we hold the selection (until SelectionClear)

	interval  time.Duration
	closeOnce sync.Once
}

// New connects to the display and prepares the hidden window.
func New(display string, interval time.Duration, log *slog.Logger) (*Watcher, error) {
	if interval <= 0 {
		interval = 700 * time.Millisecond
	}
	x, err := xgb.NewConnDisplay(display)
	if err != nil {
		return nil, fmt.Errorf("clipboard: connect %q: %w", xconn.DisplayString(display), err)
	}
	setup := xproto.Setup(x)
	screen := setup.DefaultScreen(x)

	w := &Watcher{
		x:        x,
		root:     screen.Root,
		log:      log,
		interval: interval,
	}

	w.atomClipboard = w.intern("CLIPBOARD")
	w.atomUTF8 = w.intern("UTF8_STRING")
	w.atomTEXT = w.intern("TEXT")
	w.atomSTRING = w.intern("STRING")
	w.atomTargets = w.intern("TARGETS")
	w.atomIncr = w.intern("INCR")
	w.propAtom = w.intern("REMMOTE_CLIP")
	if w.atomClipboard == 0 || w.atomUTF8 == 0 {
		x.Close()
		return nil, fmt.Errorf("clipboard: required atoms unavailable")
	}

	wid, err := x.NewId()
	if err != nil {
		x.Close()
		return nil, err
	}
	w.win = xproto.Window(wid)
	// A 1×1 unmapped InputOutput window can own selections.
	if err := xproto.CreateWindowChecked(x, screen.RootDepth, w.win, w.root,
		0, 0, 1, 1, 0,
		xproto.WindowClassInputOutput, 0, 0, nil).Check(); err != nil {
		x.Close()
		return nil, fmt.Errorf("clipboard: CreateWindow: %w", err)
	}
	return w, nil
}

func (w *Watcher) intern(name string) xproto.Atom {
	r, err := xproto.InternAtom(w.x, false, uint16(len(name)), name).Reply()
	if err != nil {
		return 0
	}
	return r.Atom
}

// Close shuts the X connection down.
func (w *Watcher) Close() { w.closeOnce.Do(w.x.Close) }

// Run drives the watcher until ctx is done or the connection closes: it
// polls the local clipboard and calls onLocal whenever locally-copied
// text differs from what was last synchronized. Run blocks; run it in a
// goroutine. xgb delivers events as values.
func (w *Watcher) Run(ctx context.Context, onLocal func(text string)) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopClose := context.AfterFunc(ctx, w.Close)
	defer stopClose()
	pollDone := make(chan struct{})
	defer func() { cancel(); <-pollDone }()
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	go func() {
		defer close(pollDone)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				w.mu.Lock()
				owner := w.amOwner
				w.mu.Unlock()
				if !owner {
					xproto.ConvertSelection(w.x, w.win, w.atomClipboard, w.atomUTF8, w.propAtom, xproto.TimeCurrentTime)
				}
			}
		}
	}()

	for {
		if ctx.Err() != nil {
			return
		}
		ev, err := w.x.WaitForEvent()
		if err != nil {
			return // connection closed
		}
		if ev == nil && err == nil {
			return // both nil: connection closed (xgb contract)
		}
		switch e := ev.(type) {
		case xproto.SelectionNotifyEvent:
			if e.Selection != w.atomClipboard || e.Property != w.propAtom || e.Target != w.atomUTF8 {
				continue
			}
			text, ok := w.readProperty()
			if !ok {
				continue
			}
			w.mu.Lock()
			changed := !bytes.Equal(text, w.synced)
			if changed {
				w.synced = text
			}
			w.mu.Unlock()
			if changed {
				onLocal(string(text))
			}

		case xproto.SelectionRequestEvent:
			w.serveRequest(e)

		case xproto.SelectionClearEvent:
			// We lost ownership (someone copied locally) — resume polling.
			if e.Selection == w.atomClipboard {
				w.mu.Lock()
				w.amOwner = false
				w.mu.Unlock()
			}
		}
	}
}

// readProperty fetches (and deletes) our read property. ok=false for
// unsupported transfers (INCR / empty).
func (w *Watcher) readProperty() ([]byte, bool) {
	// LongLength is in 4-byte units.
	reply, err := xproto.GetProperty(w.x, false, w.win, w.propAtom, 0, 0, proto.MaxClipboard/4).Reply()
	if err != nil {
		return nil, false
	}
	xproto.DeleteProperty(w.x, w.win, w.propAtom)
	if reply.Type == w.atomIncr {
		w.log.Debug("clipboard: INCR transfer skipped")
		return nil, false
	}
	if len(reply.Value) == 0 {
		return nil, false
	}
	return reply.Value, true
}

// serveRequest answers a selection request when we own the clipboard.
func (w *Watcher) serveRequest(e xproto.SelectionRequestEvent) {
	w.mu.Lock()
	served := w.served
	w.mu.Unlock()

	var ok bool
	switch e.Target {
	case w.atomTargets:
		// Format-32 property values follow the readers' byte order —
		// little-endian, same reasoning as sendSelectionNotify.
		targets := make([]byte, 12)
		binary.LittleEndian.PutUint32(targets[0:4], uint32(w.atomUTF8))
		binary.LittleEndian.PutUint32(targets[4:8], uint32(w.atomTEXT))
		binary.LittleEndian.PutUint32(targets[8:12], uint32(w.atomSTRING))
		ok = w.setProp(e.Requestor, e.Property, w.atomTargets, 32, targets)
	case w.atomUTF8, w.atomTEXT, w.atomSTRING:
		if len(served) == 0 {
			ok = w.setProp(e.Requestor, e.Property, e.Target, 8, nil)
		} else {
			ok = w.setProp(e.Requestor, e.Property, w.atomUTF8, 8, served)
		}
	}
	if !ok {
		e.Property = 0 // refuse
	}
	w.sendSelectionNotify(e)
}

func (w *Watcher) setProp(win xproto.Window, prop, typ xproto.Atom, format byte, data []byte) bool {
	return xproto.ChangePropertyChecked(w.x, xproto.PropModeReplace, win, prop, typ, format,
		uint32(len(data)/(int(format)/8)), data).Check() == nil
}

func (w *Watcher) sendSelectionNotify(e xproto.SelectionRequestEvent) {
	// SelectionNotify wire layout: code(1) pad(1) seq(2) time(4)
	// requestor(4) selection(4) target(4) property(4) pad(8).
	//
	// xgb negotiates an LSB-first connection and decodes with Get32
	// (little-endian); x86 Xlib clients are little-endian too, so the
	// synthetic event is encoded little-endian for every likely reader.
	b := make([]byte, 32)
	b[0] = eventSelectionNotify
	binary.LittleEndian.PutUint32(b[4:8], uint32(e.Time))
	binary.LittleEndian.PutUint32(b[8:12], uint32(e.Requestor))
	binary.LittleEndian.PutUint32(b[12:16], uint32(e.Selection))
	binary.LittleEndian.PutUint32(b[16:20], uint32(e.Target))
	binary.LittleEndian.PutUint32(b[20:24], uint32(e.Property))
	xproto.SendEvent(w.x, false, e.Requestor, 0, string(b))
}

// SetRemote installs text from the remote peer as the local clipboard:
// we take selection ownership and serve it. Marked as synced so our own
// poll does not bounce it back.
func (w *Watcher) SetRemote(text string) {
	data := []byte(text)
	w.mu.Lock()
	w.served = data
	w.synced = data
	w.amOwner = true
	w.mu.Unlock()
	xproto.SetSelectionOwner(w.x, w.win, w.atomClipboard, xproto.TimeCurrentTime)
}
