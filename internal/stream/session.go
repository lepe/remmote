package stream

import (
	"bufio"
	"context"
	"net"
	"sync/atomic"
	"time"

	"github.com/jezek/xgb/xproto"

	"github.com/lepe/remmote/internal/proto"
)

// frame is one fully-encoded outgoing message, shared (read-only) by all
// sessions so encoding happens exactly once per capture.
type frame struct {
	typ      proto.MsgType
	flags    uint8
	payload  []byte
	keyframe bool
}

func newFrame(typ proto.MsgType, flags uint8, payload []byte, keyframe bool) *frame {
	return &frame{typ: typ, flags: flags, payload: payload, keyframe: keyframe}
}

// outboxCap bounds the per-client frame queue. Frames share their encoded
// payload, but retaining many old frames increases visual latency under
// load. Keep only a short backlog, recovering dropped deltas with a keyframe.
const outboxCap = 2

// session is one connected client: a TCP conn with a reader (input
// events in), a writer (frames out), and a bounded frame queue between
// them and the capture loop.
type session struct {
	id   uint64
	st   *Stream
	conn net.Conn
	br   *bufio.Reader // carries bytes buffered during the handshake
	out  chan *frame
	pump *inputPump

	needKey   atomic.Bool
	dropped   atomic.Uint64
	framesOut atomic.Uint64
}

func newSession(st *Stream, id uint64, conn net.Conn, br *bufio.Reader) *session {
	return &session{
		id:   id,
		st:   st,
		conn: conn,
		br:   br,
		out:  make(chan *frame, outboxCap),
		pump: newInputPump(st.rt),
	}
}

// deliver queues f without blocking the capture loop. Queue full: drop
// the frame and request a keyframe — unless f is a keyframe itself, in
// which case it evicts the queued frames first.
func (sess *session) deliver(f *frame) {
	select {
	case sess.out <- f:
		sess.framesOut.Add(1)
	default:
		if !f.keyframe {
			sess.dropped.Add(1)
			sess.needKey.Store(true)
			return
		}
		for {
			select {
			case <-sess.out: // evict a stale non-key frame
			default:
				select {
				case sess.out <- f:
					sess.framesOut.Add(1)
				default:
					sess.dropped.Add(1)
					sess.needKey.Store(true)
				}
				return
			}
		}
	}
}

const shutdownGrace = 250 * time.Millisecond

// reader consumes client messages until the client goes away. Input is
// handed to the session's inputPump rather than injected inline, so a
// fast mouse cannot build a backlog ahead of a click or keystroke.
func (sess *session) reader(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		_ = sess.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		t, _, payload, err := proto.ReadMsg(sess.br)
		if err != nil {
			return
		}
		switch t {
		case proto.MsgMouseMove:
			if m, err := proto.DecodeMouseMove(payload); err == nil {
				sess.pump.moveTo(int(m.X)*max(1, sess.st.opts.Downscale), int(m.Y)*max(1, sess.st.opts.Downscale))
			}
		case proto.MsgMouseButton:
			if m, err := proto.DecodeMouseButton(payload); err == nil {
				sess.pump.enqueue(ctx, inEvent{kind: kindButton, b: m.Button, down: m.Down})
			}
		case proto.MsgWheel:
			if m, err := proto.DecodeWheel(payload); err == nil {
				sess.pump.enqueue(ctx, inEvent{kind: kindWheel, x: int(m.DX), y: int(m.DY)})
			}
		case proto.MsgKey:
			if m, err := proto.DecodeKey(payload); err == nil {
				sess.pump.enqueue(ctx, inEvent{kind: kindKey, ks: xproto.Keysym(m.Keysym), down: m.Down})
			}
		case proto.MsgSetQuality:
			if m, err := proto.DecodeSetQuality(payload); err == nil && m.Quality >= 1 && m.Quality <= 100 {
				sess.st.quality.Store(int32(m.Quality))
				sess.st.log.Info("quality changed", "quality", m.Quality, "client", sess.id)
			}
		case proto.MsgResize:
			// Stream coordinates, like MouseMove: the client divided by
			// its -upscale, so multiply back by -downscale to land in
			// host pixels.
			if m, err := proto.DecodeResize(payload); err == nil {
				f := max(1, sess.st.opts.Downscale)
				sess.st.requestResize(int(m.Width)*f, int(m.Height)*f)
			}
		case proto.MsgPong, proto.MsgPing:
			// keepalive traffic; the read deadline is fed
		case proto.MsgClipboard:
			if m, err := proto.DecodeClipboard(payload); err == nil && sess.st.clip != nil {
				sess.st.clip.SetRemote(m.Text)
			}
		case proto.MsgClose:
			return
		default:
			sess.st.log.Warn("unexpected message from client; dropping connection",
				"type", t.String(), "client", sess.id)
			return
		}
	}
}

// writer drains the outbox to the client.
func (sess *session) writer(ctx context.Context) {
	defer sess.conn.Close()
	bw := bufio.NewWriter(sess.conn)
	for {
		select {
		case <-ctx.Done():
			sess.sayBye(bw, proto.CloseShutdown, "server shutting down")
			sess.conn.Close()
			return
		case f := <-sess.out:
			_ = sess.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := proto.WriteMsg(bw, f.typ, f.flags, f.payload); err != nil {
				if ctx.Err() == nil {
					sess.st.log.Warn("client write failed (slow client?)", "client", sess.id, "err", err)
				}
				sess.conn.Close()
				return
			}
			if err := bw.Flush(); err != nil {
				if ctx.Err() == nil {
					sess.st.log.Warn("client flush failed", "client", sess.id, "err", err)
				}
				sess.conn.Close()
				return
			}
		}
	}
}

func (sess *session) sayBye(bw *bufio.Writer, code uint8, reason string) {
	_ = sess.conn.SetWriteDeadline(time.Now().Add(shutdownGrace))
	c := &proto.Close{Code: code, Reason: reason}
	if err := proto.WriteMsg(bw, proto.MsgClose, 0, c.Encode()); err == nil {
		_ = bw.Flush()
	}
}
