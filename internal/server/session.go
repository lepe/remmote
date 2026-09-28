package server

import (
	"bufio"
	"context"
	"net"
	"sync"
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

const outboxCap = 4

// session is one connected client: a TCP conn with a reader (input
// events in), a writer (frames out), and a bounded frame queue between
// them and the capture loop.
type session struct {
	id   uint64
	srv  *Server
	conn net.Conn
	br   *bufio.Reader // carries bytes buffered during the handshake
	out  chan *frame

	stopOnce sync.Once
	done     chan struct{}

	needKey   atomic.Bool
	dropped   atomic.Uint64
	framesOut atomic.Uint64
}

func newSession(srv *Server, id uint64, conn net.Conn, br *bufio.Reader) *session {
	return &session{
		id:   id,
		srv:  srv,
		conn: conn,
		br:   br,
		out:  make(chan *frame, outboxCap),
		done: make(chan struct{}),
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

// stop closes the connection and signals the writer to say goodbye.
// Idempotent.
func (sess *session) stop(reason string) {
	sess.stopOnce.Do(func() {
		close(sess.done)
		// Give the writer a brief window to flush a goodbye, then yank.
		go func() {
			select {
			case <-time.After(2 * time.Second):
			}
			sess.conn.Close()
		}()
	})
}

// reader consumes client messages until the client goes away.
func (sess *session) reader(ctx context.Context) {
	for {
		_ = sess.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		t, _, payload, err := proto.ReadMsg(sess.br)
		if err != nil {
			return
		}
		switch t {
		case proto.MsgMouseMove:
			if m, err := proto.DecodeMouseMove(payload); err == nil {
				sess.srv.rt.MovePointer(int(m.X), int(m.Y))
			}
		case proto.MsgMouseButton:
			if m, err := proto.DecodeMouseButton(payload); err == nil {
				sess.srv.rt.Button(m.Button, m.Down)
			}
		case proto.MsgWheel:
			if m, err := proto.DecodeWheel(payload); err == nil {
				sess.srv.rt.Wheel(int(m.DX), int(m.DY))
			}
		case proto.MsgKey:
			if m, err := proto.DecodeKey(payload); err == nil {
				sess.srv.rt.Key(xproto.Keysym(m.Keysym), m.Down)
			}
		case proto.MsgSetQuality:
			if m, err := proto.DecodeSetQuality(payload); err == nil && m.Quality >= 1 {
				sess.srv.quality.Store(int32(m.Quality))
				sess.srv.log.Info("quality changed", "quality", m.Quality, "client", sess.id)
			}
		case proto.MsgPong, proto.MsgPing:
			// keepalive traffic; the read deadline is fed
		case proto.MsgClipboard:
			if m, err := proto.DecodeClipboard(payload); err == nil && sess.srv.clip != nil {
				sess.srv.clip.SetRemote(m.Text)
			}
		case proto.MsgClose:
			return
		default:
			sess.srv.log.Warn("unexpected message from client; dropping connection",
				"type", t.String(), "client", sess.id)
			return
		}
	}
}

// writer drains the outbox to the client.
func (sess *session) writer(ctx context.Context) {
	bw := bufio.NewWriter(sess.conn)
	for {
		select {
		case <-ctx.Done():
			sess.sayBye(bw, proto.CloseShutdown, "server shutting down")
			sess.conn.Close()
			return
		case <-sess.done:
			sess.sayBye(bw, proto.CloseShutdown, "session closed")
			sess.conn.Close()
			return
		case f := <-sess.out:
			_ = sess.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := proto.WriteMsg(bw, f.typ, f.flags, f.payload); err != nil {
				sess.srv.log.Warn("client write failed (slow client?)",
					"client", sess.id, "err", err)
				sess.conn.Close()
				return
			}
			if err := bw.Flush(); err != nil {
				sess.srv.log.Warn("client flush failed", "client", sess.id, "err", err)
				sess.conn.Close()
				return
			}
		}
	}
}

func (sess *session) sayBye(bw *bufio.Writer, code uint8, reason string) {
	_ = sess.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	c := &proto.Close{Code: code, Reason: reason}
	if err := proto.WriteMsg(bw, proto.MsgClose, 0, c.Encode()); err == nil {
		_ = bw.Flush()
	}
}
