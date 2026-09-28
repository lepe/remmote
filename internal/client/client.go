// Package client connects to a remmote server, decodes rect updates into
// the viewer canvas, and forwards local input. It reconnects with
// backoff while the viewer window stays open.
package client

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"time"

	"golang.org/x/image/webp"

	"github.com/lepe/remmote/internal/clipboard"
	"github.com/lepe/remmote/internal/proto"
	"github.com/lepe/remmote/internal/viewer"
)

// Options configures the client.
type Options struct {
	Display     string
	ServerAddr  string
	Quality     int    // 0 = keep server default; 1-100 sends SetQuality
	Once        bool   // exit after the first keyframe (CI mode)
	Snapshot    string // with Once: write the first full frame as PNG
	NoClipboard bool   // disable clipboard synchronization
}

// Run drives the client until the window closes or ctx is canceled.
func Run(ctx context.Context, opts Options, log *slog.Logger) error {
	if opts.Once {
		return runOnce(ctx, opts, log)
	}

	// Interactive mode: the window lives across reconnects.
	var win *viewer.Window
	outbound := make(chan outMsg, 64)
	winClosed := make(chan error, 1)
	var haveWindow bool

	// Clipboard watcher on the viewer's display: local copies go to the
	// server, remote text becomes the local clipboard.
	var clip *clipboard.Watcher
	if !opts.NoClipboard {
		if w, err := clipboard.New(opts.Display, 0, log); err != nil {
			log.Warn("clipboard sync unavailable", "err", err)
		} else {
			clip = w
			defer w.Close()
			log.Info("clipboard sync enabled")
			go w.Run(ctx, func(text string) {
				select {
				case outbound <- outMsg{t: proto.MsgClipboard, payload: (&proto.ClipboardData{Text: text}).Encode()}:
				default:
				}
			})
		}
	}

	attempt := 0
	for {
		// Connect (with backoff on repeat attempts).
		conn, hello, err := dial(opts, log, attempt)
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			case <-winClosed:
				return nil
			case <-time.After(backoff(attempt)):
			}
			attempt++
			continue
		}

		if !haveWindow {
			win, err = viewer.Open(opts.Display, int(hello.Width), int(hello.Height),
				"remmote — "+hello.Name, log)
			if err != nil {
				return fmt.Errorf("client: open viewer: %w", err)
			}
			defer win.Close()
			haveWindow = true
			go func() { winClosed <- win.Pump(&sender{out: outbound}) }()
		}

		if opts.Quality >= 1 && opts.Quality <= 100 {
			outbound <- outMsg{t: proto.MsgSetQuality, payload: (&proto.SetQuality{Quality: uint8(opts.Quality)}).Encode()}
		}

		log.Info("connected", "server", opts.ServerAddr, "screen",
			fmt.Sprintf("%dx%d", hello.Width, hello.Height), "name", hello.Name)

		netErr := session(ctx, conn, win, clip, outbound, log)

		select {
		case <-ctx.Done():
			return nil
		case <-winClosed:
			return nil // user closed the window: done for real
		default:
		}
		if netErr != nil {
			log.Warn("disconnected; reconnecting", "err", netErr)
		}
		attempt++
	}
}

// session runs one connection to completion: network reader + writer.
// Returns the error that ended the session (nil on clean Close).
func session(ctx context.Context, conn net.Conn, win *viewer.Window, clip *clipboard.Watcher, outbound chan outMsg, log *slog.Logger) error {
	defer conn.Close()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		bw := bufio.NewWriter(conn)
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-outbound:
				_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if err := proto.WriteMsg(bw, m.t, 0, m.payload); err != nil {
					return
				}
				if err := bw.Flush(); err != nil {
					return
				}
			}
		}
	}()

	br := bufio.NewReader(conn)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		t, flags, payload, err := proto.ReadMsg(br)
		if err != nil {
			return err
		}
		switch t {
		case proto.MsgRectUpdate:
			m, err := proto.DecodeRectUpdate(payload)
			if err != nil {
				return fmt.Errorf("bad RectUpdate: %w", err)
			}
			img, err := decode(m.Codec, m.Data)
			if err != nil {
				log.Warn("decode rect", "codec", m.Codec, "err", err)
				continue
			}
			r := image.Rect(int(m.X), int(m.Y), int(m.X)+int(m.W), int(m.Y)+int(m.H))
			if flags&proto.FlagKeyframe != 0 {
				win.UpdateServerSize(int(m.W), int(m.H)) // keyframe = full screen
			}
			win.Canvas().Composite(r, img)
			win.Dirty()

		case proto.MsgScreenResize:
			m, err := proto.DecodeScreenResize(payload)
			if err != nil {
				return err
			}
			win.UpdateServerSize(int(m.Width), int(m.Height))

		case proto.MsgPing:
			m, err := proto.DecodePingPong(payload)
			if err != nil {
				return err
			}
			select {
			case outbound <- outMsg{t: proto.MsgPong, payload: m.Encode()}:
			default:
			}

		case proto.MsgClipboard:
			if m, err := proto.DecodeClipboard(payload); err == nil && clip != nil {
				clip.SetRemote(m.Text)
			}

		case proto.MsgClose:
			m, _ := proto.DecodeClose(payload)
			log.Info("server closed connection", "reason", m.Reason)
			return nil

		default:
			return fmt.Errorf("unexpected message %v", t)
		}
	}
}

// sender adapts the viewer's EventListener onto the outbound channel.
type sender struct {
	out chan outMsg
}

type outMsg struct {
	t       proto.MsgType
	payload []byte
}

func (s *sender) MouseMove(x, y int) {
	s.send(proto.MsgMouseMove, (&proto.MouseMove{X: uint16(x), Y: uint16(y)}).Encode())
}

func (s *sender) Button(b uint8, down bool) {
	s.send(proto.MsgMouseButton, (&proto.MouseButton{Button: b, Down: down}).Encode())
}

func (s *sender) Key(ks uint32, down bool) {
	s.send(proto.MsgKey, (&proto.Key{Down: down, Keysym: ks}).Encode())
}

func (s *sender) send(t proto.MsgType, payload []byte) {
	select {
	case s.out <- outMsg{t: t, payload: payload}:
	default: // input flood: drop rather than block the UI thread
	}
}

// runOnce connects, waits for the first keyframe, snapshots it, exits.
func runOnce(ctx context.Context, opts Options, log *slog.Logger) error {
	conn, hello, err := dial(opts, log, 0)
	if err != nil {
		return err
	}
	defer conn.Close()
	log.Info("connected", "server", opts.ServerAddr, "name", hello.Name,
		"screen", fmt.Sprintf("%dx%d", hello.Width, hello.Height))

	br := bufio.NewReader(conn)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		t, flags, payload, err := proto.ReadMsg(br)
		if err != nil {
			return fmt.Errorf("waiting for keyframe: %w", err)
		}
		if t != proto.MsgRectUpdate || flags&proto.FlagKeyframe == 0 {
			continue
		}
		m, err := proto.DecodeRectUpdate(payload)
		if err != nil {
			return err
		}
		img, err := decode(m.Codec, m.Data)
		if err != nil {
			return fmt.Errorf("decode keyframe: %w", err)
		}
		if opts.Snapshot != "" {
			if err := savePNG(img, opts.Snapshot); err != nil {
				return err
			}
		}
		log.Info("keyframe received", "width", m.W, "height", m.H, "codec", proto.CodecName(m.Codec))
		return nil
	}
	return fmt.Errorf("no keyframe within 10 s")
}

// dial connects and performs the version handshake.
func dial(opts Options, log *slog.Logger, attempt int) (net.Conn, *proto.ServerHello, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(context.Background(), "tcp", opts.ServerAddr)
	if err != nil {
		return nil, nil, err
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	hello := &proto.ClientHello{Version: proto.ProtoVersion}
	if err := proto.WriteMsg(conn, proto.MsgClientHello, 0, hello.Encode()); err != nil {
		conn.Close()
		return nil, nil, err
	}
	br := bufio.NewReader(conn)
	t, _, payload, err := proto.ReadMsg(br)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	if t != proto.MsgServerHello {
		if t == proto.MsgClose {
			if c, err := proto.DecodeClose(payload); err == nil {
				return nil, nil, fmt.Errorf("server refused: %s", c.Reason)
			}
		}
		conn.Close()
		return nil, nil, fmt.Errorf("expected ServerHello, got %v", t)
	}
	srvHello, err := proto.DecodeServerHello(payload)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, srvHello, nil
}

// decode decompresses a rect payload by codec byte (both pure Go).
func decode(codec uint8, data []byte) (image.Image, error) {
	switch codec {
	case proto.CodecJPEG:
		return jpeg.Decode(bytes.NewReader(data))
	case proto.CodecWebP:
		return webp.Decode(bytes.NewReader(data))
	default:
		return nil, fmt.Errorf("unknown codec %d", codec)
	}
}

func savePNG(img image.Image, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

// backoff: 500ms << attempt, capped at 8s, ±20% jitter.
func backoff(attempt int) time.Duration {
	d := 500 * time.Millisecond << min(attempt, 4)
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	jitter := time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
	return jitter
}
