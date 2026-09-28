package capture

import (
	"image"
	"testing"

	"github.com/jezek/xgb/xproto"
)

func TestClampRect(t *testing.T) {
	screen := image.Rect(0, 0, 1920, 1080)
	cases := []struct {
		in   image.Rectangle
		want image.Rectangle
	}{
		{image.Rect(10, 10, 100, 100), image.Rect(10, 10, 100, 100)},
		{image.Rect(-50, -50, 100, 100), image.Rect(0, 0, 100, 100)},
		{image.Rect(1900, 1000, 2200, 1200), image.Rect(1900, 1000, 1920, 1080)}, // post-resize straggler
		{image.Rect(2000, 0, 2100, 100), image.Rectangle{}},                      // fully outside
		{image.Rect(-100, -100, -10, -10), image.Rectangle{}},                    // negative-only
	}
	for i, tc := range cases {
		if got := clampRect(tc.in, screen); got != tc.want {
			t.Errorf("case %d: clamp(%v) = %v, want %v", i, tc.in, got, tc.want)
		}
	}
}

func TestUnionRect(t *testing.T) {
	empty := image.Rectangle{}
	a := image.Rect(0, 0, 100, 100)
	b := image.Rect(50, 50, 150, 150)
	if got := unionRect(a, b); got != image.Rect(0, 0, 150, 150) {
		t.Errorf("union(a,b) = %v", got)
	}
	if got := unionRect(empty, b); got != b {
		t.Errorf("union(empty,b) = %v", got)
	}
	if got := unionRect(a, empty); got != a {
		t.Errorf("union(a,empty) = %v", got)
	}
	if got := unionRect(empty, empty); !got.Empty() {
		t.Errorf("union(empty,empty) = %v", got)
	}
}

func TestShouldFullRefresh(t *testing.T) {
	screen := image.Rect(0, 0, 1000, 1000)
	cases := []struct {
		r    image.Rectangle
		want bool
	}{
		{image.Rect(0, 0, 700, 700), false},    // 49%
		{image.Rect(0, 0, 800, 800), true},     // 64%
		{image.Rect(0, 0, 770, 780), true},     // ~60.1%
		{image.Rect(0, 0, 770, 770), false},    // 59.3%
		{image.Rect(0, 0, 100, 100), false},    // 1%
		{image.Rectangle{}, false},             // empty
		{image.Rect(0, 0, 10000, 10000), true}, // exceeds screen
	}
	for i, tc := range cases {
		if got := shouldFullRefresh(tc.r, screen); got != tc.want {
			t.Errorf("case %d: shouldFullRefresh(%v) = %v, want %v", i, tc.r, got, tc.want)
		}
	}
}

func TestXprotoRect(t *testing.T) {
	got := xprotoRect(xproto.Rectangle{X: -5, Y: 10, Width: 100, Height: 200})
	if got != image.Rect(-5, 10, 95, 210) {
		t.Errorf("xprotoRect = %v", got)
	}
}
