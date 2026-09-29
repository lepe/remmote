package client

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/lepe/remmote/internal/proto"
)

// A motion flood must never cost the user a click or a keystroke. The
// old single-channel design dropped *any* message when the channel was
// full, so under a fast mouse (which fills the queue as soon as the
// socket write blocks) button presses and keys were silently discarded —
// which reads to the user as "input takes forever / never responds".
func TestDiscreteInputSurvivesMotionFlood(t *testing.T) {
	q := newOutQueue(64, nil)

	// Fill the queue completely with motion, the way a 1 kHz mouse does
	// while the writer is blocked on a full socket buffer.
	for i := 0; i < 4096; i++ {
		q.mouseMove(i%900, i%700)
	}
	if got := q.eventsLen(); got != 0 {
		t.Fatalf("motion flood queued %d discrete events, want 0", got)
	}

	// Now a click and a keystroke. These must be delivered, in order,
	// regardless of how much motion is pending.
	q.event(proto.MsgMouseButton, (&proto.MouseButton{Button: 1, Down: true}).Encode())
	q.event(proto.MsgKey, (&proto.Key{Down: true, Keysym: 0x0061}).Encode()) // 'a'
	q.event(proto.MsgMouseButton, (&proto.MouseButton{Button: 1, Down: false}).Encode())
	q.event(proto.MsgKey, (&proto.Key{Down: false, Keysym: 0x0061}).Encode())

	if got := q.eventsLen(); got != 4 {
		t.Fatalf("queued %d discrete events, want 4", got)
	}
	// The newest motion must still be there too.
	if _, ok := q.peekMove(); !ok {
		t.Fatal("newest motion position lost")
	}

	// And they must come out of the writer promptly, motion first.
	server, cli := net.Pipe()
	defer server.Close()
	defer cli.Close()
	in := newReadInput()
	go in.run(server)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go writeLoop(ctx, cli, q)

	want := []proto.MsgType{proto.MsgMouseButton, proto.MsgKey, proto.MsgMouseButton, proto.MsgKey}
	deadline := time.Now().Add(2 * time.Second)
	var types []proto.MsgType
	for {
		types = types[:0]
		leadingMove := false
		for _, m := range in.all() {
			if m.t == proto.MsgMouseMove && !leadingMove {
				leadingMove = true
				continue
			}
			types = append(types, m.t)
		}
		if len(types) >= len(want) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d discrete events written: %v", len(types), len(want), types)
		}
		time.Sleep(5 * time.Millisecond)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("event order = %v, want %v", types, want)
		}
	}
}
