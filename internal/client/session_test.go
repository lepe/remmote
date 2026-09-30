package client

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/lepe/remmote/internal/proto"
)

// serveUpgrade answers the client's attach request with the 101 a real
// daemon sends, so a fake server speaks to the stream exactly where the
// daemon hands the connection over.
func serveUpgrade(conn net.Conn) error {
	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	_, err := conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: remmote\r\nConnection: Upgrade\r\n\r\n"))
	return err
}

func TestHandshakePreservesFirstFrame(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		if err := serveUpgrade(conn); err != nil {
			done <- err
			return
		}
		if _, _, _, err := proto.ReadMsg(conn); err != nil {
			done <- err
			return
		}
		var wire bytes.Buffer
		_ = proto.WriteMsg(&wire, proto.MsgServerHello, 0, (&proto.ServerHello{Version: proto.ProtoVersion, Width: 2, Height: 2}).Encode())
		_ = proto.WriteMsg(&wire, proto.MsgRectUpdate, proto.FlagKeyframe, []byte("first-frame"))
		_, err = conn.Write(wire.Bytes())
		done <- err
	}()
	conn, _, err := dial(context.Background(), Options{ServerAddr: ln.Addr().String()}, slog.Default(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	typ, _, data, err := proto.ReadMsg(conn)
	if err != nil || typ != proto.MsgRectUpdate || string(data) != "first-frame" {
		t.Fatalf("first frame lost: %v %v %q", err, typ, data)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSessionStopsIdleWriterBeforeReconnect(t *testing.T) {
	srv, cli := net.Pipe()
	defer srv.Close()
	q := newOutQueue(64, nil)
	done := make(chan error, 1)
	go func() {
		done <- session(context.Background(), cli, nil, nil, q, newUpscaler(1), slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	if err := proto.WriteMsg(srv, proto.MsgClose, 0, (&proto.Close{Reason: "test"}).Encode()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("session waited for idle writer")
	}
	// session only returns after its writer exits, so the reconnect owns
	// all subsequent input. No stale goroutine may drain this event.
	q.event(proto.MsgKey, (&proto.Key{Keysym: 0x61, Down: true}).Encode())
	time.Sleep(10 * time.Millisecond)
	if len(q.events) != 1 {
		t.Fatal("disconnected writer consumed new input")
	}
}

func TestCancellationInterruptsHandshakeAndFirstFrame(t *testing.T) {
	for _, hello := range []bool{false, true} {
		name := "handshake"
		if hello {
			name = "first-frame"
		}
		t.Run(name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			ready := make(chan struct{})
			peerDone := make(chan struct{})
			go func() {
				defer close(peerDone)
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				if err := serveUpgrade(conn); err != nil {
					return
				}
				if _, _, _, err := proto.ReadMsg(conn); err != nil {
					return
				}
				if hello {
					_ = proto.WriteMsg(conn, proto.MsgServerHello, 0, (&proto.ServerHello{Version: proto.ProtoVersion, Width: 2, Height: 2}).Encode())
				}
				close(ready)
				_, _ = io.Copy(io.Discard, conn)
			}()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- Run(ctx, Options{ServerAddr: ln.Addr().String(), Once: true}, slog.Default()) }()
			select {
			case <-ready:
			case <-time.After(time.Second):
				t.Fatal("client never connected")
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Ctrl+C should be successful: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation ignored")
			}
			<-peerDone
		})
	}
}

func TestCancellationUnblocksFullInputQueue(t *testing.T) {
	q := newOutQueue(1, nil)
	ctx, cancel := context.WithCancel(context.Background())
	q.ctx = ctx
	q.event(proto.MsgKey, nil)
	done := make(chan struct{})
	go func() { q.event(proto.MsgKey, nil); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("input producer stuck during shutdown")
	}
}
