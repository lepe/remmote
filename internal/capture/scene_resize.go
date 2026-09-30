package capture

import (
	"fmt"

	"github.com/jezek/xgb/xproto"
)

// ResizeTo records the size a viewer asked for and sends it to the shared
// application's main window as soon as that window is known and mapped.
// The canvas follows on its own — the scene recomputes its bounding box
// from ConfigureNotify and signals Resized — so no keyframe work happens
// here. Capture-loop only: it issues X requests.
func (s *Scene) ResizeTo(w, h int) error {
	s.mu.Lock()
	s.wantW, s.wantH = w, h
	s.mu.Unlock()
	s.applyPendingResize()
	return nil
}

// applyPendingResize sends the recorded size to the main window, once,
// and only when it differs from what was sent last — it runs on every
// ApplyResize, i.e. every frame. The main window is the seed window when
// it is known (the same one -maximize targets), otherwise the first
// tracked window that is mapped; the choice sticks, and is re-made when
// that window is destroyed. An unmapped main window is waited for rather
// than resized, since a window manager is entitled to discard a request
// for a window that is not viewable. Capture-loop only.
func (s *Scene) applyPendingResize() {
	s.mu.Lock()
	if s.wantW == 0 || s.wantH == 0 {
		s.mu.Unlock()
		return
	}
	target, px, py := s.resizeTarget, 0, 0
	if target == 0 {
		target = s.maximizeTarget
	}
	picked := false
	if target != 0 {
		for _, t := range s.windows {
			if t.id == target {
				picked = true
				if !t.mapped {
					s.mu.Unlock()
					return // retried on the next ApplyResize
				}
				px, py = t.rect.Min.X, t.rect.Min.Y
				break
			}
		}
	}
	if !picked {
		target = 0
		for _, t := range s.windows {
			if t.mapped {
				target, px, py = t.id, t.rect.Min.X, t.rect.Min.Y
				break
			}
		}
	}
	if target == 0 {
		s.mu.Unlock()
		return // no window yet (the application may still be starting)
	}
	if s.appliedTo == target && s.appliedW == s.wantW && s.appliedH == s.wantH {
		s.mu.Unlock()
		return
	}
	wantW, wantH := s.wantW, s.wantH
	s.mu.Unlock()

	rootW, rootH := s.xc.ScreenSize()
	nw, nh, cw, ch := windowSizeForCanvas(px, py, wantW, wantH, int(rootW), int(rootH))
	if cw != floorQ(wantW+16) || ch != floorQ(wantH+16) {
		// The window sits too close to the right/bottom screen edge to
		// grow any further without being cut off: say so, because the
		// viewer's letterbox now has a cause worth naming.
		s.log.Info("viewer size limited by the screen edge",
			"asked", fmt.Sprintf("%dx%d", wantW, wantH),
			"canvas", fmt.Sprintf("%dx%d", cw, ch))
	}

	s.mu.Lock()
	s.resizeTarget = target
	s.appliedTo, s.appliedW, s.appliedH = target, wantW, wantH
	s.mu.Unlock()

	mode, err := s.xq.Resize(target, nw, nh)
	if err != nil {
		s.log.Warn("viewer resize failed", "window", uint32(target),
			"width", nw, "height", nh, "err", err)
		return
	}
	s.log.Info("resized window for viewer", "window", uint32(target),
		"width", nw, "height", nh, "mode", mode)
}

// forgetResizeTarget drops the resize bookkeeping for a destroyed window
// so the next mapped window of the set is sized instead. Caller holds
// s.mu.
func (s *Scene) forgetResizeTarget(id xproto.Window) {
	if s.resizeTarget == id {
		s.resizeTarget = 0
	}
	if s.appliedTo == id {
		s.appliedTo = 0
	}
}

// windowSizeForCanvas solves the main window's size for the canvas the
// viewer asked for. The canvas is the window's bounding box quantized
// outward to a 32 px grid (quantizeOutward), so a 32-aligned target C
// comes out exact when the window's right and bottom edges sit on
// floorQ(origin)+C: the window keeps its position, its size absorbs the
// sub-32 px origin offset, and the canvas lands within 16 px of the
// request — the closest a single window on that grid can get. C is also
// clamped so the aligned edge stays on screen, because the canvas is
// clipped to the root window; the returned cw/ch are the canvas the
// window will actually produce. Pure: unit-tested without X.
func windowSizeForCanvas(px, py, wantW, wantH, rootW, rootH int) (nw, nh, cw, ch int) {
	fx, fy := floorQ(px), floorQ(py)
	// Nearest 32 to the request (floorQ(v+16)), lower-bounded by the
	// grid step and upper-bounded by what fits between the origin's grid
	// position and the screen edge.
	cw = min(max(floorQ(wantW+16), 32), max(32, rootW-fx))
	ch = min(max(floorQ(wantH+16), 32), max(32, rootH-fy))
	nw, nh = fx+cw-px, fy+ch-py
	if nw < 1 || nh < 1 {
		return max(1, wantW), max(1, wantH), cw, ch
	}
	return nw, nh, cw, ch
}
