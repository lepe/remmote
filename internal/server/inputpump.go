package server

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/jezek/xgb/xproto"
)

// inputPump serialises input injection for one client and keeps input
// latency independent of how fast the mouse is moving.
//
// Pointer motion is a *stream*: only the newest position still matters, so
// it gets a one-slot latest-wins mailbox. Buttons, wheel and keys are
// *discrete*: they queue in arrival order and are never dropped or
// merged. The two lanes are separate precisely so that a fast mouse
// cannot build a backlog of stale positions that delays the next click —
// a click is injected after at most one position update, however many
// moves arrived before it.
//
// Without this, every move cost one synchronous XTEST round trip, so a
// client streaming hundreds of positions per second pushed the click
// behind the whole queue: seconds of apparent input lag on a fast mouse.
// inputSink is the injection surface the pump drives (input.Router in
// production). An interface so the pump's ordering and latency bounds can
// be tested against a slow sink — which is what a busy X server looks
// like from here.
type inputSink interface {
	MovePointer(x, y int)
	Button(b uint8, down bool)
	Wheel(dx, dy int)
	Key(ks xproto.Keysym, down bool)
}

type inputPump struct {
	rt      inputSink
	events  chan inEvent
	move    chan movePos
	keys    map[xproto.Keysym]bool
	buttons map[uint8]bool

	// Latency instrumentation: how long the oldest queued input waited
	// before being injected. Reported in the server stats line so a
	// latency problem is visible without a profiler.
	eventsN  atomic.Int64
	movesN   atomic.Int64
	droppedN atomic.Int64
	waitMax  atomic.Int64 // nanoseconds
}

type movePos struct {
	x, y int
	at   time.Time
}

type inKind uint8

const (
	kindButton inKind = iota
	kindWheel
	kindKey
)

type inEvent struct {
	kind inKind
	b    uint8
	down bool
	x, y int
	ks   xproto.Keysym
	at   time.Time
}

func newInputPump(rt inputSink) *inputPump {
	return &inputPump{
		rt:     rt,
		events: make(chan inEvent, 256),
		move:   make(chan movePos, 1),
		keys:   make(map[xproto.Keysym]bool), buttons: make(map[uint8]bool),
	}
}

// moveTo queues the newest pointer position, replacing any position that
// has not been injected yet. Never blocks: the mailbox is a single slot.
func (p *inputPump) moveTo(x, y int) {
	mv := movePos{x: x, y: y, at: time.Now()}
	select {
	case p.move <- mv:
		return
	default:
	}
	select { // drop the stale position, then install the new one
	case <-p.move:
	default:
	}
	select {
	case p.move <- mv:
	default:
	}
}

// enqueue queues a discrete event. It waits for room rather than
// dropping: a lost click or keystroke is far worse than a small delay.
func (p *inputPump) enqueue(ctx context.Context, e inEvent) {
	e.at = time.Now()
	select {
	case p.events <- e:
	case <-ctx.Done():
		p.droppedN.Add(1)
	}
}

func (p *inputPump) takeMove() (movePos, bool) {
	select {
	case mv := <-p.move:
		return mv, true
	default:
		return movePos{}, false
	}
}

func (p *inputPump) note(at time.Time) {
	if d := time.Since(at); d > 0 {
		for {
			cur := p.waitMax.Load()
			if int64(d) <= cur || p.waitMax.CompareAndSwap(cur, int64(d)) {
				return
			}
		}
	}
}

func (p *inputPump) apply(e inEvent) {
	switch e.kind {
	case kindButton:
		p.rt.Button(e.b, e.down)
		if e.down {
			p.buttons[e.b] = true
		} else {
			delete(p.buttons, e.b)
		}
	case kindWheel:
		p.rt.Wheel(e.x, e.y)
	case kindKey:
		p.rt.Key(e.ks, e.down)
		if e.down {
			p.keys[e.ks] = true
		} else {
			delete(p.keys, e.ks)
		}
	}
	// Measured after injection, so the reported figure is the full
	// submission latency: queue wait plus sending XTEST requests. This
	// does not measure X processing or the time until pixels reach the viewer.
	p.note(e.at)
}

func (p *inputPump) injectMove(mv movePos) {
	p.rt.MovePointer(mv.x, mv.y)
	p.note(mv.at)
	p.movesN.Add(1)
}

// run is the only goroutine that injects for this client, so XTEST calls
// stay ordered and the mailbox is drained at a predictable rate.
func (p *inputPump) run(ctx context.Context) {
	defer func() {
		for ks := range p.keys {
			p.rt.Key(ks, false)
		}
		for b := range p.buttons {
			p.rt.Button(b, false)
		}
	}()
	for {
		if ctx.Err() != nil {
			return
		}
		// Discrete events win over pointer motion. A plain select would
		// pick either lane at random, so a click could still queue behind
		// an extra position update; this makes the bound exact — at most
		// the one position update the click itself needs.
		select {
		case <-ctx.Done():
			return
		case e := <-p.events:
			p.event(e)
			continue
		default:
		}
		select {
		case <-ctx.Done():
			return
		case e := <-p.events:
			p.event(e)
		case mv := <-p.move:
			p.injectMove(mv)
		}
	}
}

func (p *inputPump) event(e inEvent) {
	// XTEST applies an event at the current pointer position, so the
	// newest queued position goes first: the click must land where the
	// user actually clicked.
	if mv, ok := p.takeMove(); ok {
		p.injectMove(mv)
	}
	p.apply(e)
	p.eventsN.Add(1)
}

// inputStats is a snapshot for the stats line.
type inputStats struct {
	events, moves, dropped int64
	waitMax                time.Duration
}

// takeInputStats returns the counters and resets the latency high-water
// mark, so the stats line reports the worst wait in each interval.
func (p *inputPump) takeInputStats() inputStats {
	return inputStats{
		events:  p.eventsN.Swap(0),
		moves:   p.movesN.Swap(0),
		dropped: p.droppedN.Swap(0),
		waitMax: time.Duration(p.waitMax.Swap(0)),
	}
}
