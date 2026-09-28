package capture

import (
	"image"

	"github.com/jezek/xgb/xproto"
)

// xprotoRect converts an X rectangle (int16 origin, uint16 size) into an
// image.Rectangle. Damage areas can arrive slightly outside the screen
// (events racing a resize); callers must still clamp before use.
func xprotoRect(r xproto.Rectangle) image.Rectangle {
	return image.Rect(int(r.X), int(r.Y), int(r.X)+int(r.Width), int(r.Y)+int(r.Height))
}

// clampRect intersects r with bounds, yielding a rect safe to feed to
// GetImage. Out-of-range coordinates collapse to an empty rect.
func clampRect(r, bounds image.Rectangle) image.Rectangle {
	return r.Intersect(bounds)
}

// unionRect merges two rects; empty operands vanish.
func unionRect(a, b image.Rectangle) image.Rectangle {
	if a.Empty() {
		return b
	}
	if b.Empty() {
		return a
	}
	return a.Union(b)
}

// shouldFullRefresh reports whether a dirty bounding box covers enough of
// the screen that encoding one full frame beats encoding the bbox: >60%.
func shouldFullRefresh(r, screen image.Rectangle) bool {
	if r.Empty() || screen.Empty() {
		return false
	}
	return r.Dx()*r.Dy()*5 > screen.Dx()*screen.Dy()*3
}
