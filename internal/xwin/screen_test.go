package xwin

import (
	"image"
	"testing"

	"github.com/jezek/xgb/randr"
)

func foot(crtc randr.Crtc, x, y, w, h int, mode randr.Mode, modes ...modeRect) crtcFoot {
	if len(modes) == 0 {
		modes = []modeRect{{id: mode, w: w, h: h}}
	}
	return crtcFoot{
		crtc: crtc, x: x, y: y, w: w, h: h, mode: mode,
		rotation: randr.RotationRotate0, modes: modes,
	}
}

// The framebuffer alone can grow: an output already inside the target
// must be left completely alone.
func TestFitFootprintsGrowKeepsOutputs(t *testing.T) {
	foots := []crtcFoot{foot(1, 0, 0, 1280, 800, 10)}
	changes, err := fitFootprints(foots, 1600, 1000)
	if err != nil {
		t.Fatalf("fit: %v", err)
	}
	if changes[0].changed {
		t.Fatalf("grow changed the CRTC: %+v", changes[0])
	}
}

// A shrink picks the largest mode that fits the target.
func TestFitFootprintsShrinkPicksLargestFittingMode(t *testing.T) {
	foots := []crtcFoot{foot(1, 0, 0, 1280, 800, 10,
		modeRect{id: 10, w: 1280, h: 800},
		modeRect{id: 11, w: 1024, h: 768},
		modeRect{id: 12, w: 800, h: 600},
	)}
	changes, err := fitFootprints(foots, 1024, 600)
	if err != nil {
		t.Fatalf("fit: %v", err)
	}
	ch := changes[0]
	if !ch.changed || ch.mode != 12 {
		t.Fatalf("plan = %+v, want mode 12 (800x600): 1024x768 is taller than the target", ch)
	}
	if ch.x != 0 || ch.y != 0 {
		t.Errorf("position = %d,%d, want 0,0", ch.x, ch.y)
	}
}

// Xvfb's screen has exactly one mode, its initial size: no smaller mode
// exists, so the plan must refuse and touch nothing.
func TestFitFootprintsRefusesWhenNoModeFits(t *testing.T) {
	foots := []crtcFoot{foot(1, 0, 0, 1280, 800, 10, modeRect{id: 10, w: 1280, h: 800})}
	if _, err := fitFootprints(foots, 1024, 600); err == nil {
		t.Fatal("1280x800 is the only mode and does not fit 1024x600: want an error")
	}
}

// Outputs that already fit are repositioned rather than re-moded, so the
// shrink packs them inside the target.
func TestFitFootprintsRepositionsSideBySideOutputs(t *testing.T) {
	foots := []crtcFoot{
		foot(1, 0, 0, 1280, 720, 10),
		foot(2, 1280, 0, 1280, 720, 20),
	}
	changes, err := fitFootprints(foots, 1600, 900)
	if err != nil {
		t.Fatalf("fit: %v", err)
	}
	if changes[0].changed {
		t.Errorf("first output changed: %+v", changes[0])
	}
	if !changes[1].changed || changes[1].x != 320 || changes[1].y != 0 || changes[1].mode != 20 {
		t.Fatalf("second output = %+v, want the same mode moved to x=320", changes[1])
	}
}

// Two outputs whose modes cannot be packed that tightly must be refused
// as a whole rather than applied partially.
func TestFitFootprintsRefusesWhenUnionOverflows(t *testing.T) {
	foots := []crtcFoot{
		foot(1, 0, 0, 1024, 768, 10, modeRect{id: 10, w: 1024, h: 768}),
		foot(2, 1024, 0, 1024, 768, 20, modeRect{id: 20, w: 1024, h: 768}),
	}
	if _, err := fitFootprints(foots, 1500, 700); err == nil {
		t.Fatal("768-tall modes do not fit a 700-tall target: want an error")
	}
}

// A rotated CRTC's footprint is its mode swapped: 800x1280 turned a
// quarter is 1280x800 and fits a 1280x800 target.
func TestFitFootprintsAccountsForRotation(t *testing.T) {
	f := foot(1, 0, 0, 1280, 800, 10, modeRect{id: 10, w: 800, h: 1280})
	f.rotation = randr.RotationRotate90
	changes, err := fitFootprints([]crtcFoot{f}, 1280, 800)
	if err != nil {
		t.Fatalf("fit: %v", err)
	}
	if changes[0].changed {
		t.Fatalf("rotated output already fits, plan = %+v", changes[0])
	}
}

// bestFit compares footprints, not raw mode dimensions.
func TestBestFitUsesRotatedFootprint(t *testing.T) {
	modes := []modeRect{{id: 7, w: 800, h: 1280}}
	id, w, h, ok := bestFit(modes, 1280, 800, randr.RotationRotate90)
	if !ok || id != 7 || w != 1280 || h != 800 {
		t.Fatalf("bestFit = %d %dx%d ok=%v, want 7 1280x800 true", id, w, h, ok)
	}
	if _, _, _, ok := bestFit(modes, 1280, 800, randr.RotationRotate0); ok {
		t.Fatal("unrotated 800x1280 must not fit a 1280x800 target")
	}
}

// A mode id of 0 is the "create one" placeholder (virtual outputs): it
// must be usable, not read as "no mode found".
func TestBestFitAcceptsCreatedModePlaceholder(t *testing.T) {
	id, w, h, ok := bestFit([]modeRect{{id: 0, w: 700, h: 500}}, 700, 500, randr.RotationRotate0)
	if !ok || id != 0 || w != 700 || h != 500 {
		t.Fatalf("bestFit = %d %dx%d ok=%v, want the 700x500 placeholder", id, w, h, ok)
	}
}

// Virtual outputs (nested servers) get the exact target even when their
// current mode already fits — growing must work too — and the created
// mode's raw size is carried for CreateMode.
func TestFitFootprintsVirtualOutputGetsExactTarget(t *testing.T) {
	f := foot(1, 0, 0, 640, 480, 10,
		modeRect{id: 0, w: 800, h: 600}, // the exact target, to be created
		modeRect{id: 10, w: 640, h: 480},
		modeRect{id: 11, w: 320, h: 240})
	f.virtual = true
	changes, err := fitFootprints([]crtcFoot{f}, 800, 600)
	if err != nil {
		t.Fatalf("fit: %v", err)
	}
	ch := changes[0]
	if !ch.changed || ch.mode != 0 || ch.createW != 800 || ch.createH != 600 {
		t.Fatalf("plan = %+v, want mode 0 (create 800x600)", ch)
	}
}

// When several candidates fit, the created one is exact and wins; the
// advertised list stays as the fallback for servers that refuse it.
func TestFitFootprintsVirtualPrefersExactOverList(t *testing.T) {
	f := foot(1, 0, 0, 320, 240, 10,
		modeRect{id: 0, w: 700, h: 500}, // exact target
		modeRect{id: 10, w: 640, h: 480})
	f.virtual = true
	changes, err := fitFootprints([]crtcFoot{f}, 700, 500)
	if err != nil {
		t.Fatalf("fit: %v", err)
	}
	if ch := changes[0]; ch.mode != 0 || ch.createW != 700 || ch.createH != 500 {
		t.Fatalf("plan = %+v, want the exact 700x500 placeholder over 640x480", ch)
	}
}

func TestRealModesDropsPlaceholder(t *testing.T) {
	got := realModes([]modeRect{{id: 0, w: 1, h: 1}, {id: 9, w: 2, h: 2}})
	if len(got) != 1 || got[0].id != 9 {
		t.Fatalf("realModes = %+v, want only the id-9 mode", got)
	}
}

func TestFitFootprintsUnionIsInsideTarget(t *testing.T) {
	foots := []crtcFoot{
		foot(1, 0, 0, 1920, 1080, 10,
			modeRect{id: 10, w: 1920, h: 1080},
			modeRect{id: 11, w: 1600, h: 900},
			modeRect{id: 12, w: 1280, h: 720}),
	}
	changes, err := fitFootprints(foots, 1600, 900)
	if err != nil {
		t.Fatalf("fit: %v", err)
	}
	if changes[0].mode != 11 {
		t.Fatalf("mode = %d, want 11 (1600x900, the largest that fits)", changes[0].mode)
	}
	union := image.Rect(changes[0].x, changes[0].y,
		changes[0].x+1600, changes[0].y+900)
	if !union.In(image.Rect(0, 0, 1600, 900)) {
		t.Errorf("union %v escapes the target", union)
	}
}
