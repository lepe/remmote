package client

import (
	"bufio"
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/lepe/remmote/internal/proto"
)

// readInput reads framed input messages from r in a background goroutine.
type readInput struct {
	mu      sync.Mutex
	msgs    []inMsg
	changed chan struct{}
}

type inMsg struct {
	t proto.MsgType
	p []byte
}

func newReadInput() *readInput {
	r := &readInput{changed: make(chan struct{}, 1)}
	return r
}

func (r *readInput) run(conn net.Conn) {
	br := bufio.NewReader(conn)
	for {
		t, _, payload, err := proto.ReadMsg(br)
		if err != nil {
			return
		}
		r.mu.Lock()
		r.msgs = append(r.msgs, inMsg{t: t, p: payload})
		select {
		case r.changed <- struct{}{}:
		default:
		}
		r.mu.Unlock()
	}
}

// waitForType waits for the actual event, not earlier motion traffic.
func (r *readInput) waitForType(typ proto.MsgType, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		for _, m := range r.all() {
			if m.t == typ {
				return true
			}
		}
		select {
		case <-r.changed:
		case <-timer.C:
			return false
		}
	}
}

func (r *readInput) all() []inMsg {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]inMsg(nil), r.msgs...)
}

// Motion coalescing must never drop, delay, or reorder a click: XTEST
// applies a button at the current pointer position, so the position has
// to reach the wire first.
func TestWriteLoopOrdersMoveBeforeButton(t *testing.T) {
	server, cli := net.Pipe()
	defer server.Close()
	defer cli.Close()
	in := newReadInput()
	go in.run(server)

	q := newOutQueue(64, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go writeLoop(ctx, cli, q)

	for i := 0; i < 32; i++ {
		q.mouseMove(i, i)
	}
	q.event(proto.MsgMouseButton, (&proto.MouseButton{Button: 1, Down: true}).Encode())

	deadline := time.Now().Add(2 * time.Second)
	for {
		msgs := in.all()
		for i, m := range msgs {
			if m.t != proto.MsgMouseButton {
				continue
			}
			if i == 0 || msgs[i-1].t != proto.MsgMouseMove {
				t.Fatalf("button written without a preceding position: %v", msgs)
			}
			// Earlier motion may already have been sent. The position
			// immediately before the click must be the newest one.
			d, err := proto.DecodeMouseMove(msgs[i-1].p)
			if err != nil {
				t.Fatal(err)
			}
			if int(d.X) != 31 {
				t.Fatalf("stale position written: x=%d, want newest 31", d.X)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("button press never written; got %d messages", len(msgs))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Under a sustained motion flood the newest position must keep flowing
// and a click must not be starved.
func TestWriteLoopUnderMotionFlood(t *testing.T) {
	server, cli := net.Pipe()
	defer server.Close()
	defer cli.Close()
	in := newReadInput()
	go in.run(server)

	q := newOutQueue(1024, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go writeLoop(ctx, cli, q)

	stop := make(chan struct{})
	var pushed int
	var mu sync.Mutex
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			q.mouseMove(i%900, i%700)
			mu.Lock()
			pushed++
			mu.Unlock()
		}
	}()

	time.Sleep(200 * time.Millisecond) // let the flood build up

	t0 := time.Now()
	q.event(proto.MsgMouseButton, (&proto.MouseButton{Button: 1, Down: true}).Encode())
	if !in.waitForType(proto.MsgMouseButton, 2*time.Second) {
		close(stop)
		t.Fatal("click starved behind the motion flood")
	}
	clickDelay := time.Since(t0)
	t.Logf("click delay under flood: %v", clickDelay.Round(time.Microsecond))
	if clickDelay > 250*time.Millisecond {
		t.Errorf("click took %v under a motion flood, want <250ms", clickDelay)
	}
	close(stop)

	mu.Lock()
	p := pushed
	mu.Unlock()
	// The flood pushes far more positions than the writer can send: the
	// mailbox must collapse them, or the queue is not really latest-wins.
	if len(in.all()) >= p/2 {
		t.Errorf("no coalescing: %d positions pushed, %d written", p, len(in.all()))
	}
}
