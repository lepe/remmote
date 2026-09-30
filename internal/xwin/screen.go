package xwin

import (
	"fmt"
	"image"

	"github.com/jezek/xgb/randr"
	"github.com/jezek/xgb/xproto"
)

// ResizeScreen resizes the X screen — the root window every client on
// that display sees — to w×h and reports what the server actually
// accepted. Best effort by design, because RANDR is the gatekeeper:
//
//   - The cheap path is a framebuffer resize alone (RRSetScreenSize),
//     which is all a grow needs and all any server that tolerates a
//     framebuffer smaller than its active outputs ever needs.
//   - A shrink normally fails while the outputs' current modes are
//     larger than the target, so the outputs are moved to the largest
//     modes that fit first — each one independently, positions clamped
//     into the target — and the framebuffer is retried.
//   - Outputs without modes of their own are virtual (Xephyr: its mode
//     list is fixed, and emptied when its window is resized). Those get
//     an exact-size mode created on demand instead: nested windows take
//     any size. A real monitor is never given a mode it did not
//     advertise — that can drive it out of range.
//
// Nothing is left changed when the plan cannot be applied: the CRTCs
// are restored and the caller keeps letterboxing. The advertised
// screen-size range is deliberately not clamped to — nested servers
// report a range that only ever shrinks.
func (c *Client) ResizeScreen(w, h int) (int, int, error) {
	if err := randr.Init(c.x); err != nil {
		return 0, 0, fmt.Errorf("xwin: RANDR unavailable: %w", err)
	}
	if w < 1 || h < 1 || w > 0xffff || h > 0xffff {
		return 0, 0, fmt.Errorf("xwin: screen resize %dx%d out of range", w, h)
	}
	cur, err := xproto.GetGeometry(c.x, xproto.Drawable(c.root)).Reply()
	if err != nil {
		return 0, 0, fmt.Errorf("xwin: root geometry: %w", err)
	}
	if w == int(cur.Width) && h == int(cur.Height) {
		return w, h, nil
	}
	first := c.setScreenSize(w, h)
	if first == nil {
		return w, h, nil
	}
	// The framebuffer alone would not do: the active outputs have to
	// shrink first (or the server has to be one that refuses outright).
	res, rerr := randr.GetScreenResourcesCurrent(c.x, c.root).Reply()
	if rerr != nil {
		return int(cur.Width), int(cur.Height), first
	}
	foots, ferr := c.queryCrtcs(res)
	if ferr != nil {
		return int(cur.Width), int(cur.Height), first
	}
	for i := range foots {
		if foots[i].virtual {
			// Exact size, created on demand: a nested server (Xephyr)
			// resizes its window and framebuffer to whatever it is given.
			foots[i].modes = []modeRect{{id: 0, w: w, h: h}}
		}
	}
	changes, perr := fitFootprints(foots, w, h)
	if perr != nil {
		// Nothing could be made to fit: report why, chained with the
		// original refusal.
		return int(cur.Width), int(cur.Height),
			fmt.Errorf("xwin: screen resize %dx%d: %w (%v)", w, h, first, perr)
	}
	anyChanged := false
	for _, ch := range changes {
		anyChanged = anyChanged || ch.changed
	}
	if !anyChanged {
		return int(cur.Width), int(cur.Height), first
	}
	rollback, aerr := c.applyCrtcChanges(foots, changes, res.ConfigTimestamp)
	if aerr != nil {
		return int(cur.Width), int(cur.Height), aerr
	}
	if err := c.setScreenSize(w, h); err != nil {
		rollback()
		return int(cur.Width), int(cur.Height),
			fmt.Errorf("xwin: screen resize %dx%d after mode change: %w", w, h, err)
	}
	return w, h, nil
}

// setScreenSize issues RRSetScreenSize, keeping the screen's physical
// size proportional so the DPI stays put.
func (c *Client) setScreenSize(w, h int) error {
	scr := xproto.Setup(c.x).DefaultScreen(c.x)
	mmW := scaleMM(w, int(scr.WidthInPixels), int(scr.WidthInMillimeters))
	mmH := scaleMM(h, int(scr.HeightInPixels), int(scr.HeightInMillimeters))
	return randr.SetScreenSizeChecked(c.x, c.root,
		uint16(w), uint16(h), uint32(mmW), uint32(mmH)).Check()
}

// scaleMM keeps millimetres proportional to pixels across a resize.
func scaleMM(target, pixels, millimetres int) int {
	if pixels <= 0 || millimetres <= 0 || target <= 0 {
		return 0
	}
	mm := target * millimetres / pixels
	if mm < 1 {
		mm = 1
	}
	return mm
}

// crtcFoot is one active CRTC: where its output sits now, and which modes
// the driving output offers (in the server's preference order) should it
// have to change. virtual marks an output without modes of its own — a
// nested server (Xephyr) — which can be given an exact-size mode (id 0,
// created on demand) instead of being limited to what it advertised.
type crtcFoot struct {
	crtc     randr.Crtc
	out      randr.Output
	x, y     int
	mode     randr.Mode
	w, h     int // footprint, rotation applied
	rotation uint16
	outputs  []randr.Output
	modes    []modeRect
	virtual  bool
}

// modeRect is a RANDR mode with its size.
type modeRect struct {
	id   randr.Mode
	w, h int
}

// crtcChange is a planned CRTC move: same footprint unless changed. A
// mode of 0 means "create one of createW×createH when applying" (only
// ever planned for virtual outputs).
type crtcChange struct {
	foot    int
	x, y    int
	mode    randr.Mode
	createW int
	createH int
	changed bool
}

// queryCrtcs reads every active CRTC and the mode list of its output.
// Outputs with no modes of their own (Xephyr: its list is fixed, and
// emptied when its window resizes) are marked virtual and fall back to
// the screen's mode list — the sizes the server registered overall.
func (c *Client) queryCrtcs(res *randr.GetScreenResourcesCurrentReply) ([]crtcFoot, error) {
	sizes := make(map[randr.Mode]image.Point, len(res.Modes))
	var screenModes []modeRect
	for _, m := range res.Modes {
		sizes[randr.Mode(m.Id)] = image.Pt(int(m.Width), int(m.Height))
		screenModes = append(screenModes, modeRect{id: randr.Mode(m.Id), w: int(m.Width), h: int(m.Height)})
	}
	type outFacts struct {
		crtc  randr.Crtc
		modes []modeRect
	}
	outputs := make(map[randr.Output]outFacts, len(res.Outputs))
	for _, o := range res.Outputs {
		oi, err := randr.GetOutputInfo(c.x, o, res.ConfigTimestamp).Reply()
		if err != nil || oi.Connection != randr.ConnectionConnected {
			continue
		}
		var modes []modeRect
		for _, mid := range oi.Modes {
			if sz, ok := sizes[mid]; ok {
				modes = append(modes, modeRect{id: mid, w: sz.X, h: sz.Y})
			}
		}
		outputs[o] = outFacts{crtc: oi.Crtc, modes: modes}
	}

	var foots []crtcFoot
	for _, cr := range res.Crtcs {
		ci, err := randr.GetCrtcInfo(c.x, cr, res.ConfigTimestamp).Reply()
		if err != nil || ci.Mode == 0 || len(ci.Outputs) == 0 {
			continue // disabled CRTC: contributes nothing to the framebuffer
		}
		f := crtcFoot{
			crtc:     cr,
			x:        int(ci.X),
			y:        int(ci.Y),
			mode:     ci.Mode,
			rotation: ci.Rotation,
			outputs:  ci.Outputs,
		}
		if sz, ok := sizes[ci.Mode]; ok {
			f.w, f.h = rotatedSize(sz.X, sz.Y, ci.Rotation)
		}
		for o, facts := range outputs {
			if facts.crtc == cr {
				f.out, f.modes = o, facts.modes
				break
			}
		}
		if len(f.modes) == 0 {
			f.virtual, f.modes = true, screenModes
		}
		foots = append(foots, f)
	}
	return foots, nil
}

// rotatedSize swaps width and height for the quarter turns.
func rotatedSize(w, h int, rotation uint16) (int, int) {
	if rotation&randr.RotationRotate90 != 0 || rotation&randr.RotationRotate270 != 0 {
		return h, w
	}
	return w, h
}

// fitFootprints plans the CRTC configuration for a target screen of
// tw×th: every CRTC that already fits inside the target keeps its mode
// (its position is clamped only if it would hang outside), the others
// move to the largest mode that fits. Virtual outputs — nested servers,
// see crtcFoot — are always handed the exact target instead (mode 0 =
// create one of createW×createH when applying). The plan is refused —
// and nothing applied — when the result cannot be packed into the
// target. Pure: the X round trips happen in applyCrtcChanges.
func fitFootprints(foots []crtcFoot, tw, th int) ([]crtcChange, error) {
	changes := make([]crtcChange, len(foots))
	union := image.Rectangle{}
	for i, f := range foots {
		ch := crtcChange{foot: i, x: f.x, y: f.y, mode: f.mode}
		cw, chh := f.w, f.h
		force := f.virtual && (f.w != tw || f.h != th)
		if force || f.x < 0 || f.y < 0 || f.x+cw > tw || f.y+chh > th {
			// Hangs outside the target: keep the mode if its size alone
			// fits (repositioning is enough), else pick a smaller one —
			// or, for virtual outputs, exactly the one asked for.
			mode, mw, mh := f.mode, cw, chh
			if force || mw > tw || mh > th {
				var ok bool
				mode, mw, mh, ok = bestFit(f.modes, tw, th, f.rotation)
				if !ok {
					return nil, fmt.Errorf("no mode of the output on CRTC %#x fits in %dx%d",
						uint32(f.crtc), tw, th)
				}
				for _, m := range f.modes {
					if m.id == mode {
						ch.createW, ch.createH = m.w, m.h
						break
					}
				}
			}
			cw, chh = mw, mh
			ch.mode = mode
			ch.x = min(max(f.x, 0), max(0, tw-cw))
			ch.y = min(max(f.y, 0), max(0, th-chh))
			ch.changed = mode != f.mode || ch.x != f.x || ch.y != f.y
		}
		union = union.Union(image.Rect(ch.x, ch.y, ch.x+cw, ch.y+chh))
		changes[i] = ch
	}
	if union.Min.X < 0 || union.Min.Y < 0 || union.Max.X > tw || union.Max.Y > th {
		return nil, fmt.Errorf("outputs cover %v, which does not fit in %dx%d",
			union, tw, th)
	}
	return changes, nil
}

// bestFit returns the largest mode (by area, ties broken by the server's
// preference order) whose *footprint* — rotated as the CRTC is — fits
// inside w×h. A mode id of 0 is the "create one" placeholder and wins
// ties like any other. ok=false when no mode fits.
func bestFit(modes []modeRect, w, h int, rotation uint16) (randr.Mode, int, int, bool) {
	var (
		bestID randr.Mode
		bw, bh int
		found  bool
	)
	for _, m := range modes {
		mw, mh := rotatedSize(m.w, m.h, rotation)
		if mw > w || mh > h {
			continue
		}
		if !found || mw*mh > bw*bh {
			bestID, bw, bh, found = m.id, mw, mh, true
		}
	}
	return bestID, bw, bh, found
}

// realModes drops the "create one" placeholder (id 0) from a mode list.
func realModes(modes []modeRect) []modeRect {
	out := modes[:0:0]
	for _, m := range modes {
		if m.id != 0 {
			out = append(out, m)
		}
	}
	return out
}

// applyCrtcChanges applies every changed CRTC in plan order and returns a
// rollback that puts the ones it moved back. Requests are checked: a
// refused SetCrtcConfig stops the sequence and unwinds it. A planned mode
// of 0 is created on demand (virtual outputs only); when the server
// refuses made-up modes, the largest mode it does offer within the same
// target is used instead.
func (c *Client) applyCrtcChanges(foots []crtcFoot, plan []crtcChange, configTs xproto.Timestamp) (func(), error) {
	var done []crtcChange
	rollback := func() {
		for i := len(done) - 1; i >= 0; i-- {
			f := foots[done[i].foot]
			_ = c.setCrtc(configTs, f.crtc, f.x, f.y, f.mode, f.rotation, f.outputs)
		}
	}
	for _, ch := range plan {
		if !ch.changed {
			continue
		}
		f := foots[ch.foot]
		mode := ch.mode
		if mode == 0 {
			var err error
			mode, err = c.createMode(f.out, ch.createW, ch.createH)
			if err != nil {
				var ok bool
				mode, _, _, ok = bestFit(realModes(f.modes), ch.createW, ch.createH, f.rotation)
				if !ok {
					rollback()
					return nil, err
				}
			}
		}
		if err := c.setCrtc(configTs, f.crtc, ch.x, ch.y, mode, f.rotation, f.outputs); err != nil {
			rollback()
			return nil, err
		}
		done = append(done, ch)
	}
	return rollback, nil
}

// createMode creates (or reuses) a mode of exactly w×h and attaches it
// to out. Only ever called for virtual outputs — a monitor is never
// given a mode it did not advertise, since that can drive it out of
// range. Modes are cached per output and size, so a resize drag does not
// leave a pile of one-off modes behind.
func (c *Client) createMode(out randr.Output, w, h int) (randr.Mode, error) {
	key := fmt.Sprintf("%d:%dx%d", out, w, h)
	if m, ok := c.madeModes[key]; ok {
		return m, nil
	}
	r, err := randr.CreateMode(c.x, c.root,
		randr.ModeInfo{Width: uint16(w), Height: uint16(h)},
		fmt.Sprintf("remmote-%dx%d", w, h)).Reply()
	if err != nil {
		return 0, fmt.Errorf("xwin: CreateMode %dx%d: %w", w, h, err)
	}
	if err := randr.AddOutputModeChecked(c.x, out, r.Mode).Check(); err != nil {
		return 0, fmt.Errorf("xwin: AddOutputMode %dx%d: %w", w, h, err)
	}
	if c.madeModes == nil {
		c.madeModes = make(map[string]randr.Mode)
	}
	c.madeModes[key] = r.Mode
	return r.Mode, nil
}

// setCrtc moves one CRTC to mode/position. The config timestamp is the
// one from the resources the plan was read against; CurrentTime for the
// change timestamp, which is what clients are expected to pass.
func (c *Client) setCrtc(configTs xproto.Timestamp, crtc randr.Crtc, x, y int,
	mode randr.Mode, rotation uint16, outputs []randr.Output) error {
	r, err := randr.SetCrtcConfig(c.x, crtc, xproto.TimeCurrentTime, configTs,
		int16(x), int16(y), mode, rotation, outputs).Reply()
	if err != nil {
		return fmt.Errorf("xwin: SetCrtcConfig %#x: %w", uint32(crtc), err)
	}
	if r.Status != 0 {
		return fmt.Errorf("xwin: SetCrtcConfig %#x: status %d", uint32(crtc), r.Status)
	}
	return nil
}
