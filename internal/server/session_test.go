package server

import (
	"context"
	"github.com/lepe/remmote/internal/proto"
	"github.com/lepe/remmote/internal/xconn"
	"image"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

func TestOutboxRecoversWithFreshKeyframe(t *testing.T) {
	sess := &session{out: make(chan *frame, outboxCap)}
	for n := 0; n < outboxCap; n++ {
		sess.deliver(newFrame(proto.MsgRectUpdate, 0, []byte{byte(n)}, false))
	}
	sess.deliver(newFrame(proto.MsgRectUpdate, 0, []byte{99}, false))
	if !sess.needKey.Load() || sess.dropped.Load() != 1 {
		t.Fatal("dropped delta did not request recovery")
	}
	key := newFrame(proto.MsgRectUpdate, proto.FlagKeyframe, []byte{100}, true)
	sess.deliver(key)
	if len(sess.out) != 1 || <-sess.out != key {
		t.Fatal("keyframe did not replace stale backlog")
	}
}

func TestShutdownInterruptsStalledHandshake(t *testing.T) {
	srvConn, peer := net.Pipe()
	defer peer.Close()
	srv := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.handleConn(ctx, srvConn); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stalled handshake prevented shutdown")
	}
}

func TestShutdownInterruptsBlockedFrameWrite(t *testing.T) {
	srvConn, peer := net.Pipe()
	defer peer.Close()
	srv := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil)), sessions: make(map[uint64]*session), xc: &xconn.Conn{}, src: &imageSource{img: image.NewRGBA(image.Rect(0, 0, 2, 2))}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { srv.handleConn(ctx, srvConn); close(done) }()
	if err := proto.WriteMsg(peer, proto.MsgClientHello, 0, (&proto.ClientHello{Version: proto.ProtoVersion}).Encode()); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := proto.ReadMsg(peer); err != nil {
		t.Fatal(err)
	}
	// Wait until registration completes, then stop consuming the peer socket.
	deadline := time.Now().Add(time.Second)
	for {
		srv.mu.Lock()
		registered := len(srv.sessions) > 0
		srv.mu.Unlock()
		if registered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session never registered")
		}
		time.Sleep(time.Millisecond)
	}
	srv.broadcast(newFrame(proto.MsgRectUpdate, 0, make([]byte, 1<<20), false))
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("blocked writer prevented shutdown")
	}
}
