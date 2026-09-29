package proto

import "testing"

func TestRectUpdateViewOwnership(t *testing.T) {
	p := (&RectUpdate{W: 1, H: 1, Codec: CodecJPEG, Data: []byte{1, 2, 3}}).Encode()
	owned, err := DecodeRectUpdate(p)
	if err != nil {
		t.Fatal(err)
	}
	view, err := DecodeRectUpdateView(p)
	if err != nil {
		t.Fatal(err)
	}
	p[rectUpdateHeader] = 9
	if owned.Data[0] != 1 {
		t.Fatal("owning decoder aliases input")
	}
	if view.Data[0] != 9 {
		t.Fatal("view copied input")
	}
	if cap(view.Data) != len(view.Data) {
		t.Fatal("view permits appending into input")
	}
	for n := 0; n < rectUpdateHeader; n++ {
		if _, err := DecodeRectUpdateView(p[:n]); err == nil {
			t.Fatalf("accepted %d-byte header", n)
		}
	}
}

func BenchmarkRectUpdateDecode(b *testing.B) {
	p := (&RectUpdate{W: 1920, H: 1080, Codec: CodecJPEG, Data: make([]byte, 1<<20)}).Encode()
	for _, tc := range []struct {
		name   string
		decode func([]byte) (*RectUpdate, error)
	}{{"copy", DecodeRectUpdate}, {"view", DecodeRectUpdateView}} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				m, err := tc.decode(p)
				if err != nil || len(m.Data) != 1<<20 {
					b.Fatal("decode failed")
				}
			}
		})
	}
}
