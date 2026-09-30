package capture

import "testing"

// The window size is solved backwards through the canvas quantization:
// whatever comes out must make the canvas land on the 32 px grid within
// 16 px of what the viewer asked for, without moving the window and
// without pushing it off the screen.
func TestWindowSizeForCanvas(t *testing.T) {
	cases := []struct {
		name              string
		px, py            int // window origin, root coordinates
		wantW, wantH      int
		rootW, rootH      int
		expW, expH        int
		expCanvasW, expCH int // canvas the window would produce
	}{
		{
			name: "aligned origin",
			px:   64, py: 32, wantW: 1600, wantH: 900,
			rootW: 1920, rootH: 1080,
			expW: 1600, expH: 896,
			// canvas = ceilQ(origin+size) - floorQ(origin)
			expCanvasW: 1600, expCH: 896,
		},
		{
			name: "origin off the grid: the window absorbs the offset",
			px:   40, py: 30, wantW: 1600, wantH: 900,
			rootW: 1920, rootH: 1080,
			// floorQ(40)=32 → 1600-8 wide; floorQ(30)=0 → 896-30 tall
			expW: 1592, expH: 866,
			expCanvasW: 1600, expCH: 896,
		},
		{
			name: "request rounded to the nearest grid step",
			px:   0, py: 0, wantW: 1610, wantH: 905,
			rootW: 1920, rootH: 1080,
			expW: 1600, expH: 896, // nearest multiples of 32
			expCanvasW: 1600, expCH: 896,
		},
		{
			name: "clamped at the screen edge",
			px:   40, py: 0, wantW: 4000, wantH: 4000,
			rootW: 1632, rootH: 1080,
			// floorQ(40)=32, so 1632-32 = 1600 is the ceiling
			expW: 1592, expH: 1080,
			expCanvasW: 1600, expCH: 1080,
		},
		{
			name: "tiny request never produces a non-positive size",
			px:   40, py: 30, wantW: 10, wantH: 10,
			rootW: 1920, rootH: 1080,
			expW: 24, expH: 2, // one grid step past an origin 8/30 px in
			expCanvasW: 32, expCH: 32,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, h, cw0, ch0 := windowSizeForCanvas(tc.px, tc.py, tc.wantW, tc.wantH, tc.rootW, tc.rootH)
			if w != tc.expW || h != tc.expH {
				t.Fatalf("window = %dx%d, want %dx%d", w, h, tc.expW, tc.expH)
			}
			// The canvas this window would produce: its right/bottom edge
			// quantized outward minus the quantized origin, clipped to root.
			cw := min(ceilQ(tc.px+w), tc.rootW) - min(floorQ(tc.px), tc.rootW)
			ch := min(ceilQ(tc.py+h), tc.rootH) - min(floorQ(tc.py), tc.rootH)
			if cw != tc.expCanvasW || ch != tc.expCH {
				t.Fatalf("canvas would be %dx%d, want %dx%d", cw, ch, tc.expCanvasW, tc.expCH)
			}
			if cw0 != cw || ch0 != ch {
				t.Fatalf("reported canvas target %dx%d, want %dx%d", cw0, ch0, cw, ch)
			}
			// Precision claim, where the request is representable at all:
			// at least one grid step, and within the screen.
			if tc.wantW >= 64 && tc.wantW <= tc.rootW {
				if d := abs(cw - tc.wantW); d > 16 {
					t.Errorf("canvas width %d is %d px from the requested %d (>16)", cw, d, tc.wantW)
				}
			}
			if tc.wantH >= 64 && tc.wantH <= tc.rootH {
				if d := abs(ch - tc.wantH); d > 16 {
					t.Errorf("canvas height %d is %d px from the requested %d (>16)", ch, d, tc.wantH)
				}
			}
		})
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
