package client

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/lepe/remmote/internal/proto"
)

func discardQueue() *outQueue {
	return newOutQueue(8, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// A resize drag emits ConfigureNotify per step; only the settled size
// goes on the wire, exactly once, in stream coordinates (divided by the
// client's -upscale, as MouseMove is).
func TestSenderResizeDebouncesToOneMessage(t *testing.T) {
	q := discardQueue()
	q.ctx = context.Background()
	s := &sender{q: q, div: 1}

	s.Resize(1600, 900)
	s.Resize(1024, 768) // supersedes the first before the timer fires

	select {
	case m := <-q.events:
		if m.t != proto.MsgResize {
			t.Fatalf("message type = %v, want Resize", m.t)
		}
		r, err := proto.DecodeResize(m.payload)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if r.Width != 1024 || r.Height != 768 {
			t.Fatalf("sent %dx%d, want the last size 1024x768", r.Width, r.Height)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no resize message after the debounce window")
	}
	select {
	case m := <-q.events:
		t.Fatalf("second resize message %v: the debounce did not collapse the drag", m.t)
	case <-time.After(resizeDebounce / 2):
	}
}

// Sizes leave the client in stream coordinates: divided by -upscale,
// exactly like the pointer.
func TestSenderResizeDividesByUpscale(t *testing.T) {
	q := discardQueue()
	q.ctx = context.Background()
	s := &sender{q: q, div: 4}

	s.Resize(1600, 800)
	select {
	case m := <-q.events:
		r, err := proto.DecodeResize(m.payload)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if r.Width != 400 || r.Height != 200 {
			t.Fatalf("sent %dx%d, want 400x200 (host ÷ upscale)", r.Width, r.Height)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no resize message")
	}
}

// A sub-pixel window (smaller than the upscale factor) must not encode
// as zero — the server refuses empty sizes and would drop the session.
func TestSenderResizeClampsToAtLeastOne(t *testing.T) {
	q := discardQueue()
	q.ctx = context.Background()
	s := &sender{q: q, div: 4}

	s.Resize(3, 2)
	select {
	case m := <-q.events:
		r, err := proto.DecodeResize(m.payload)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if r.Width != 1 || r.Height != 1 {
			t.Fatalf("sent %dx%d, want 1x1", r.Width, r.Height)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no resize message")
	}
}
