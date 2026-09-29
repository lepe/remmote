package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jezek/xgb/xproto"
)

// slowSink stands in for a busy X server: every XTEST call costs
// perCall, which is what the pump's ordering rules have to survive.
type slowSink struct {
	perCall time.Duration

	mu        sync.Mutex
	calls     []string
	positions []movePos
}

func (s *slowSink) MovePointer(x, y int) {
	s.mu.Lock()
	s.positions = append(s.positions, movePos{x: x, y: y})
	s.mu.Unlock()
	s.record("move")
}
func (s *slowSink) Button(b uint8, down bool) { s.record("button") }
func (s *slowSink) Wheel(dx, dy int)          { s.record("wheel") }
func (s *slowSink) Key(ks xproto.Keysym, down bool) {
	s.record("key")
}

func (s *slowSink) record(kind string) {
	time.Sleep(s.perCall)
	s.mu.Lock()
	s.calls = append(s.calls, kind)
	s.mu.Unlock()
}

func (s *slowSink) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// A click must never queue behind a backlog of pointer positions. With
// the old inline-injection reader, N moves each cost one XTEST round
// trip, so a click arriving after 1000 moves on a 5 ms X server waited
// five seconds — the "input takes seconds to respond" symptom.
//
// The bound is asserted as a maximum over repeated trials, not an
// average: the discrete lane must be taken *as soon as it is pending*. A
// plain select over both lanes picks at random, which passes an
// average-based assertion while still leaving a long tail.
func TestInputPumpClickNotDelayedByMotionFlood(t *testing.T) {
	const (
		perCall = 5 * time.Millisecond // a busy X server
		trials  = 30
		flood   = 1000
	)
	worstMovesBefore := 0
	var worstWait time.Duration

	for trial := 0; trial < trials; trial++ {
		sink := &slowSink{perCall: perCall}
		p := newInputPump(sink)
		ctx, cancel := context.WithCancel(context.Background())

		// A fast mouse: far more positions than can be injected at 5 ms
		// each, queued before the click arrives.
		for i := 0; i < flood; i++ {
			p.moveTo(i, i)
		}
		t0 := time.Now()
		p.enqueue(ctx, inEvent{kind: kindButton, b: 1, down: true})
		go p.run(ctx)

		deadline := time.Now().Add(2 * time.Second)
		done := false
		for !done {
			for i, c := range sink.all() {
				if c == "button" {
					if i > worstMovesBefore {
						worstMovesBefore = i
					}
					if d := time.Since(t0); d > worstWait {
						worstWait = d
					}
					done = true
					break
				}
			}
			if !done {
				if time.Now().After(deadline) {
					cancel()
					t.Fatalf("trial %d: click never injected", trial)
				}
				time.Sleep(200 * time.Microsecond)
			}
		}
		cancel()
	}

	// At most: the position update already in flight, plus the one that
	// positions the click. Injected in order (the old design) the flood
	// alone would have cost flood × perCall.
	const maxBefore = 2
	if worstMovesBefore > maxBefore {
		t.Errorf("click waited behind up to %d positions over %d trials, want <= %d",
			worstMovesBefore, trials, maxBefore)
	}
	floodCost := time.Duration(flood) * perCall
	if worstWait > floodCost/100 {
		t.Errorf("worst click wait %v, want a small constant (the flood in order costs %v)",
			worstWait.Round(time.Millisecond), floodCost.Round(time.Millisecond))
	}
	t.Logf("worst of %d trials: %d position updates, %v (a %d-position flood in order costs %v)",
		trials, worstMovesBefore, worstWait.Round(time.Millisecond), flood,
		floodCost.Round(time.Millisecond))
}

// Ordering guarantee: the position a click lands on is the newest one
// sent, not a stale one.
func TestInputPumpAppliesNewestPositionBeforeClick(t *testing.T) {
	sink := &slowSink{}
	p := newInputPump(sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.run(ctx)

	for i := 0; i < 50; i++ {
		p.moveTo(i, i)
	}
	p.moveTo(999, 888) // newest
	p.enqueue(ctx, inEvent{kind: kindButton, b: 1, down: true})

	deadline := time.Now().Add(2 * time.Second)
	for {
		calls := sink.all()
		for i, c := range calls {
			if c == "button" {
				if i == 0 || calls[i-1] != "move" {
					t.Fatalf("button missing preceding position: %v", calls)
				}
				sink.mu.Lock()
				last := sink.positions[len(sink.positions)-1]
				sink.mu.Unlock()
				if last.x != 999 || last.y != 888 {
					t.Fatalf("click at stale position: %v", last)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("click never injected; calls = %v", calls)
		}
		time.Sleep(time.Millisecond)
	}
}

// The motion mailbox must stay a single slot. Without it the queue grows
// with the mouse: positions pile up faster than they can be injected and
// the pointer keeps replaying stale coordinates long after the user has
// moved on (the click still jumps the queue, but the pointer is wrong).
func TestInputPumpCoalescesMotion(t *testing.T) {
	sink := &slowSink{perCall: 5 * time.Millisecond}
	p := newInputPump(sink)
	// Push far more positions than the sink can absorb.
	for i := 0; i < 2000; i++ {
		p.moveTo(i, i)
	}
	if backlog := len(p.move); backlog > 1 {
		t.Errorf("%d positions queued after a 2000-position flood, want at most 1", backlog)
	}
	// The newest position must be the one that survives.
	mv, ok := p.takeMove()
	if !ok {
		t.Fatal("no position pending")
	}
	if mv.x != 1999 || mv.y != 1999 {
		t.Errorf("pending position = %d,%d, want the newest 1999,1999", mv.x, mv.y)
	}
}

// Discrete events keep their order and are never merged away.
func TestInputPumpKeepsDiscreteOrder(t *testing.T) {
	sink := &slowSink{}
	p := newInputPump(sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.run(ctx)

	p.enqueue(ctx, inEvent{kind: kindButton, b: 1, down: true})
	p.enqueue(ctx, inEvent{kind: kindKey, ks: 'a', down: true})
	p.enqueue(ctx, inEvent{kind: kindKey, ks: 'a', down: false})
	p.enqueue(ctx, inEvent{kind: kindButton, b: 1, down: false})

	deadline := time.Now().Add(2 * time.Second)
	for {
		calls := sink.all()
		if len(calls) >= 4 {
			got := calls[:4]
			want := []string{"button", "key", "key", "button"}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("order = %v, want %v", got, want)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/4 events injected: %v", len(calls), calls)
		}
		time.Sleep(time.Millisecond)
	}
}

// The stats snapshot must report the latency it claims to measure.
func TestInputPumpReportsWait(t *testing.T) {
	sink := &slowSink{perCall: 30 * time.Millisecond}
	p := newInputPump(sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.run(ctx)

	p.enqueue(ctx, inEvent{kind: kindKey, ks: 'x', down: true})
	deadline := time.Now().Add(2 * time.Second)
	for {
		if st := p.takeInputStats(); st.events > 0 {
			if st.waitMax < 20*time.Millisecond {
				t.Errorf("waitMax = %v, want >= 20ms (the sink cost)", st.waitMax)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("event never counted")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type heldInputSink struct{ events chan inEvent }

func (s *heldInputSink) MovePointer(x, y int) {}
func (s *heldInputSink) Wheel(dx, dy int)     {}
func (s *heldInputSink) Button(b uint8, down bool) {
	s.events <- inEvent{kind: kindButton, b: b, down: down}
}
func (s *heldInputSink) Key(ks xproto.Keysym, down bool) {
	s.events <- inEvent{kind: kindKey, ks: ks, down: down}
}

func TestInputPumpReleasesHeldInputOnExit(t *testing.T) {
	sink := &heldInputSink{events: make(chan inEvent, 8)}
	p := newInputPump(sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); p.run(ctx) }()
	p.enqueue(ctx, inEvent{kind: kindKey, ks: 0x61, down: true})
	p.enqueue(ctx, inEvent{kind: kindButton, b: 1, down: true})
	for n := 0; n < 2; n++ {
		select {
		case e := <-sink.events:
			if !e.down {
				t.Fatal("unexpected release")
			}
		case <-time.After(time.Second):
			t.Fatal("input not applied")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("input worker did not exit")
	}
	key, button := false, false
	for len(sink.events) > 0 {
		e := <-sink.events
		if e.down {
			t.Fatal("shutdown injected a press")
		}
		key = key || e.kind == kindKey && e.ks == 0x61
		button = button || e.kind == kindButton && e.b == 1
	}
	if !key || !button {
		t.Fatal("shutdown left held input behind")
	}
}
