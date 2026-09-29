package client

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"testing"

	xdraw "golang.org/x/image/draw"

	"github.com/lepe/remmote/internal/encode"
	"github.com/lepe/remmote/internal/proto"
)

// The whole point of -upscale is to undo the server's point sampling:
// the server takes host pixel (x*N, y*N) for wire pixel (x, y), so each
// wire pixel must expand back into exactly the N×N block of host pixels
// it came from. Nearest-neighbour block replication is the only
// transform that satisfies this — any smoothing filter would place the
// host's colors at coordinates the host never drew them at.
func TestUpscaleInvertsServerSamplingGrid(t *testing.T) {
	for _, factor := range []int{2, 4} {
		const w, h = 7, 5 // deliberately not multiples of the factor
		host := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				host.SetRGBA(x, y, color.RGBA{byte(x * 30), byte(y * 50), byte(x + y), 0xFF})
			}
		}
		// The server's scaledSource: keep every factor'th pixel.
		wire := image.NewRGBA(image.Rect(0, 0, (w+factor-1)/factor, (h+factor-1)/factor))
		for y := 0; y < wire.Rect.Dy(); y++ {
			for x := 0; x < wire.Rect.Dx(); x++ {
				wire.Set(x, y, host.At(x*factor, y*factor))
			}
		}

		up := newUpscaler(factor)
		r, img := up.apply(image.Rect(0, 0, wire.Rect.Dx(), wire.Rect.Dy()), wire, wire.Rect.Dx(), wire.Rect.Dy())
		if want := image.Rect(0, 0, wire.Rect.Dx()*factor, wire.Rect.Dy()*factor); r != want {
			t.Fatalf("factor %d: rect = %v, want %v", factor, r, want)
		}
		if got, want := img.Bounds(), image.Rect(0, 0, r.Dx(), r.Dy()); got != want {
			t.Fatalf("factor %d: image bounds = %v, want %v (size must match the rect)", factor, got, want)
		}
		for y := 0; y < r.Dy(); y++ {
			for x := 0; x < r.Dx(); x++ {
				if got, want := img.At(x, y), wire.At(x/factor, y/factor); got != want {
					t.Fatalf("factor %d: pixel (%d,%d) = %v, want %v", factor, x, y, got, want)
				}
			}
		}
	}
}

// A non-zero origin must survive magnification: the wire rect of a delta
// is in stream coordinates, and the canvas rect it maps to is that origin
// scaled. Getting this wrong silently displaces every delta update.
func TestUpscaleScalesDeltaOrigin(t *testing.T) {
	up := newUpscaler(2)
	wire := image.NewRGBA(image.Rect(0, 0, 4, 3))
	for i := range wire.Pix {
		wire.Pix[i] = byte(i)
	}
	r, img := up.apply(image.Rect(10, 20, 14, 23), wire, 4, 3)
	if want := image.Rect(20, 40, 28, 46); r != want {
		t.Fatalf("rect = %v, want %v", r, want)
	}
	// The magnified payload stays 0-based (Canvas.Composite reads its
	// source point from img.Bounds().Min); r carries the canvas position.
	if got, want := img.Bounds(), image.Rect(0, 0, 8, 6); got != want {
		t.Fatalf("image bounds = %v, want %v", got, want)
	}
	// Nearest-neighbour samples the source block, not a 1:1 copy of the
	// top-left pixel across the whole rect.
	if got, want := img.At(0, 0), wire.At(0, 0); got != want {
		t.Fatalf("top-left = %v, want %v", got, want)
	}
	if got, want := img.At(7, 5), wire.At(3, 2); got != want {
		t.Fatalf("bottom-right = %v, want %v", got, want)
	}
}

// Factor 1 is the default and must be a pass-through: same image, same
// rect, no copy — the common path cannot afford a per-rect allocation.
func TestUpscaleIdentityIsPassThrough(t *testing.T) {
	up := newUpscaler(1)
	wire := image.NewRGBA(image.Rect(0, 0, 3, 2))
	for i := range wire.Pix {
		wire.Pix[i] = byte(i)
	}
	r := image.Rect(5, 6, 8, 8)
	gotR, gotImg := up.apply(r, wire, 3, 2)
	if gotR != r {
		t.Fatalf("rect = %v, want %v", gotR, r)
	}
	if gotImg != image.Image(wire) {
		t.Fatal("factor 1 copied the image instead of returning it")
	}
	// newUpscaler(0) is the zero-value Options path and must behave as 1.
	if got := newUpscaler(0).factor; got != 1 {
		t.Fatalf("newUpscaler(0).factor = %d, want 1", got)
	}
}

// The scratch buffer is reused across rects, so a reused buffer must be
// fully overwritten: a stale pixel from a larger previous rect leaking
// into a smaller one would show as garbage on screen.
func TestUpscaleReusedBufferHasNoStalePixels(t *testing.T) {
	up := newUpscaler(2)
	big := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := range big.Pix {
		big.Pix[i] = 0xFF
	}
	_, first := up.apply(image.Rect(0, 0, 8, 8), big, 8, 8)
	if first.Bounds() != image.Rect(0, 0, 16, 16) {
		t.Fatalf("first bounds = %v", first.Bounds())
	}

	small := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for i := range small.Pix {
		small.Pix[i] = 0x40
	}
	_, second := up.apply(image.Rect(0, 0, 2, 2), small, 2, 2)
	if second.Bounds() != image.Rect(0, 0, 4, 4) {
		t.Fatalf("second bounds = %v, want 4x4", second.Bounds())
	}
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			if got := second.At(x, y); got != small.At(x/2, y/2) {
				t.Fatalf("(%d,%d) = %v, want %v — stale pixel from the reused buffer", x, y, got, small.At(x/2, y/2))
			}
		}
	}
}

// The whole path through the real codec: a ZRAW frame the server
// downscaled, magnified back by the client, must place exactly the
// samples the server took at their host coordinates. Detail is not
// recovered — that is what -downscale discarded — but nothing may shift,
// which an interpolating scaler would break.
func TestUpscaleRoundTripsZRAWThroughServerGrid(t *testing.T) {
	const factor = 2
	const w, h = 16, 12
	host := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// A 1-px checkerboard on the sampling grid plus a gradient:
			// block replication reproduces the samples exactly, any
			// interpolation blends them and shifts the edges.
			v := uint8(0x20 + x*12)
			if (x/factor+y/factor)%2 == 0 {
				v = 0xF0
			}
			host.SetRGBA(x, y, color.RGBA{v, byte(255 - y*15), 0x40, 0xFF})
		}
	}
	// Server side: encode the point-sampled wire image.
	wire := image.NewRGBA(image.Rect(0, 0, w/factor, h/factor))
	for y := 0; y < wire.Rect.Dy(); y++ {
		for x := 0; x < wire.Rect.Dx(); x++ {
			wire.Set(x, y, host.At(x*factor, y*factor))
		}
	}
	enc, err := encode.New(proto.CodecZRAW)
	if err != nil {
		t.Fatal(err)
	}
	_, data, err := enc.Encode(wire, 75)
	if err != nil {
		t.Fatal(err)
	}
	dec := newDecoder()
	defer dec.close()
	got, err := dec.decode(proto.CodecZRAW, data, wire.Rect.Dx(), wire.Rect.Dy())
	if err != nil {
		t.Fatal(err)
	}

	up := newUpscaler(factor)
	r, img := up.apply(image.Rect(0, 0, wire.Rect.Dx(), wire.Rect.Dy()), got, wire.Rect.Dx(), wire.Rect.Dy())
	if r != image.Rect(0, 0, w, h) {
		t.Fatalf("rect = %v, want the full host screen", r)
	}
	// Every host pixel lands the color of the host sample it came from.
	// The pixels the server never sent cannot be recovered — that is
	// exactly the detail -downscale threw away — but nothing may shift:
	// each block must be uniform and equal to host(k*factor, l*factor).
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x%factor != 0 || y%factor != 0 {
				continue // not a sample point: its value is unknowable
			}
			want := host.At(x, y)
			for dy := 0; dy < factor; dy++ {
				for dx := 0; dx < factor; dx++ {
					if got := img.At(x+dx, y+dy); got != want {
						t.Fatalf("(%d,%d) = %v, want the %d,%d host sample %v",
							x+dx, y+dy, got, x, y, want)
					}
				}
			}
		}
	}
}

// The wire carries stream coordinates and the server multiplies by its
// -downscale before injecting. A client that magnified the canvas must
// divide on the way out, or every click lands N times too far away.
func TestSenderDividesHostCoordinatesForUpscaledCanvas(t *testing.T) {
	for _, div := range []int{1, 2, 4} {
		q := newOutQueue(4, nil)
		s := &sender{q: q, div: div}
		s.MouseMove(1234, 567)
		m, ok := q.takeMove()
		if !ok {
			t.Fatalf("div %d: no move queued", div)
		}
		got, err := proto.DecodeMouseMove(m.payload)
		if err != nil {
			t.Fatal(err)
		}
		if wantX, wantY := uint16(1234/div), uint16(567/div); got.X != wantX || got.Y != wantY {
			t.Errorf("div %d: sent (%d,%d), want (%d,%d)", div, got.X, got.Y, wantX, wantY)
		}
	}
	// A zero div (zero-value sender) must not divide by zero.
	q := newOutQueue(1, nil)
	(&sender{q: q}).MouseMove(10, 20)
	if m, ok := q.takeMove(); !ok {
		t.Fatal("zero div dropped the move")
	} else if got, _ := proto.DecodeMouseMove(m.payload); got.X != 10 || got.Y != 20 {
		t.Errorf("zero div sent (%d,%d), want (10,20)", got.X, got.Y)
	}
}

// The fast path reimplements what x/image/draw's nearest-neighbour
// scaler computes, because for an integer factor the two are the same
// transform and the general one is far slower. If the fast path ever
// drifts from the reference — a rounding difference, a stride or origin
// bug — pixels would land at coordinates the host never drew, which no
// other test can see. So compare them byte for byte.
//
// Byte comparison alone is not enough: for an exact integer factor the
// two transforms produce identical output, so a silent fallback would
// pass the comparison while quietly costing four times the CPU. The
// scratch row is allocated only by the fast path, which is how the
// selection is pinned here.
func TestUpscaleMatchesReferenceScaler(t *testing.T) {
	for _, factor := range []int{2, 4} {
		for _, dims := range [][2]int{{7, 5}, {1, 1}, {16, 9}, {63, 33}} {
			w, h := dims[0], dims[1]
			// A non-zero source origin too: the ZRAW decoder reuses one
			// buffer, so a rect can arrive as a view into it.
			for _, origin := range [][2]int{{0, 0}, {5, 3}} {
				src := image.NewRGBA(image.Rect(origin[0], origin[1], origin[0]+w, origin[1]+h))
				for y := 0; y < h; y++ {
					for x := 0; x < w; x++ {
						src.SetRGBA(origin[0]+x, origin[1]+y,
							color.RGBA{byte(x * 7), byte(y * 11), byte(x ^ y), 0xFF})
					}
				}

				up := newUpscaler(factor)
				_, got := up.apply(image.Rect(0, 0, w, h), src, w, h)
				if up.row == nil {
					t.Fatalf("factor %d %dx%d origin %v: an RGBA source did not take the block-copy fast path",
						factor, w, h, origin)
				}

				want := image.NewRGBA(image.Rect(0, 0, w*factor, h*factor))
				xdraw.NearestNeighbor.Scale(want, want.Rect, src, src.Bounds(), xdraw.Src, nil)

				if !bytes.Equal(got.(*image.RGBA).Pix, want.Pix) {
					t.Fatalf("factor %d %dx%d origin %v: fast path differs from the reference scaler",
						factor, w, h, origin)
				}
			}
		}
	}
}

// replicate must decline anything it cannot block-copy safely, so a
// malformed or unusual frame reaches the general scaler — which clips —
// instead of panicking or reading past the source mid-session.
func TestUpscaleFastPathDeclinesBadSources(t *testing.T) {
	up := newUpscaler(2)
	dst := image.NewRGBA(image.Rect(0, 0, 8, 8))

	// A Rect claiming more rows than Pix holds: Stride and the claimed
	// width agree, so only the length check stands between this and a
	// slice panic.
	liar := &image.RGBA{Pix: make([]byte, 64), Stride: 16, Rect: image.Rect(0, 0, 4, 8)}
	if up.replicate(dst, liar, 4, 8) {
		t.Fatal("accepted an image whose Pix cannot hold its Rect")
	}
	// A width beyond the source's own bounds.
	src := image.NewRGBA(image.Rect(0, 0, 4, 4))
	if up.replicate(dst, src, 8, 4) {
		t.Fatal("accepted a width beyond the source bounds")
	}
	// A source whose rows are narrower than the claimed width.
	if up.replicate(dst, &image.RGBA{Pix: make([]byte, 128), Stride: 8, Rect: image.Rect(0, 0, 4, 8)}, 4, 8) {
		t.Fatal("accepted a stride narrower than the claimed width")
	}
	// Non-RGBA sources belong to the general scaler.
	if up.replicate(dst, image.NewYCbCr(image.Rect(0, 0, 4, 4), image.YCbCrSubsampleRatio444), 4, 4) {
		t.Fatal("block-copied a non-RGBA source")
	}
	// Degenerate sizes.
	if up.replicate(dst, src, 0, 0) {
		t.Fatal("accepted a zero-sized rect")
	}
}

// A non-RGBA source (JPEG decodes to YCbCr) cannot take the block-copy
// fast path; it must still be magnified correctly by the fallback.
func TestUpscaleHandlesNonRGBSources(t *testing.T) {
	src := image.NewYCbCr(image.Rect(0, 0, 8, 4), image.YCbCrSubsampleRatio420)
	for i := range src.Y {
		src.Y[i] = byte(i * 3)
	}
	up := newUpscaler(2)
	r, img := up.apply(image.Rect(0, 0, 8, 4), src, 8, 4)
	if r != image.Rect(0, 0, 16, 8) {
		t.Fatalf("rect = %v, want (0,0)-(16,8)", r)
	}
	if img.Bounds() != r {
		t.Fatalf("bounds = %v, want %v", img.Bounds(), r)
	}
	// Block replication still holds: every 2x2 block holds one source
	// pixel, so the block is uniform. That is the property the fast path
	// provides and an interpolating scaler would break. The block's exact
	// color is allowed a 1/255 step: the YCbCr → RGB conversion is not
	// exactly reversible, so even the reference scaler lands a rounding
	// step off src.At().
	const tol = 0x101 // 1/255 of full scale, in 16-bit units
	for y := 0; y < 8; y++ {
		for x := 0; x < 16; x++ {
			got := img.At(x, y)
			gr, gg, gb, _ := got.RGBA()
			wr, wg, wb, _ := src.At(x/2, y/2).RGBA()
			if d := abs16(gr, wr); d > tol {
				t.Fatalf("(%d,%d) red off by %d", x, y, d)
			}
			if d := abs16(gg, wg); d > tol {
				t.Fatalf("(%d,%d) green off by %d", x, y, d)
			}
			if d := abs16(gb, wb); d > tol {
				t.Fatalf("(%d,%d) blue off by %d", x, y, d)
			}
			// Uniform within the block.
			br, bg, bb, _ := img.At(x&^1, y&^1).RGBA()
			if gr != br || gg != bg || gb != bb {
				t.Fatalf("(%d,%d) is not uniform inside its 2x2 block: %v vs %v",
					x, y, got, img.At(x&^1, y&^1))
			}
		}
	}
}

func abs16(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}

// BenchmarkUpscale measures the per-rect magnification cost, including
// the composite that consumes it, at the sizes a keyframe and a typical
// delta produce. Factor 1 is the baseline every other row is measured
// against.
func BenchmarkUpscale(b *testing.B) {
	cases := []struct {
		name   string
		w, h   int
		factor int
	}{
		{"keyframe-native", 1920, 1080, 1},
		{"keyframe-2x", 960, 540, 2},
		{"keyframe-4x", 480, 270, 4},
		{"delta-2x", 320, 200, 2},
		{"delta-4x", 160, 100, 4},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			wire := image.NewRGBA(image.Rect(0, 0, tc.w, tc.h))
			for i := range wire.Pix {
				wire.Pix[i] = byte(i)
			}
			up := newUpscaler(tc.factor)
			canvas := image.NewRGBA(image.Rect(0, 0, tc.w*tc.factor, tc.h*tc.factor))
			r := image.Rect(0, 0, tc.w, tc.h)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				cr, img := up.apply(r, wire, tc.w, tc.h)
				draw.Draw(canvas, cr, img, img.Bounds().Min, draw.Src)
			}
		})
	}
}

// The pointer round trip through the real sender: the viewer reports host
// coordinates, sender.MouseMove divides to stream coordinates, and the
// server multiplies back by its -downscale. Magnifying the canvas must
// not add error of its own — the landing pixel may differ from the
// requested one only by the server's own sampling quantisation.
func TestPointerRoundTripThroughSender(t *testing.T) {
	const hostW, hostH = 1920, 1080
	winW, winH := hostW*8/10, hostH*8/10 // the viewer's 80%-capped window
	for _, f := range []int{1, 2, 4} {
		for _, p := range [][2]int{{0, 0}, {1, 1}, {winW / 2, winH / 2}, {winW - 1, winH - 1}} {
			// Viewer: window → host canvas coordinates.
			hostX, hostY := p[0]*hostW/winW, p[1]*hostH/winH

			q := newOutQueue(1, nil)
			(&sender{q: q, div: f}).MouseMove(hostX, hostY)
			m, ok := q.takeMove()
			if !ok {
				t.Fatalf("factor %d: no move queued", f)
			}
			wire, err := proto.DecodeMouseMove(m.payload)
			if err != nil {
				t.Fatal(err)
			}

			// Server: stream → host, multiplying by its -downscale.
			landedX, landedY := int(wire.X)*f, int(wire.Y)*f
			if d := hostX - landedX; d < 0 || d >= f {
				t.Errorf("factor %d: x %d landed at %d, %d px away (want < %d)",
					f, hostX, landedX, d, f)
			}
			if d := hostY - landedY; d < 0 || d >= f {
				t.Errorf("factor %d: y %d landed at %d, %d px away (want < %d)",
					f, hostY, landedY, d, f)
			}
		}
	}
}
