// Package client connects to a remmote server, decodes rect updates into
// the viewer canvas, and forwards local input. It reconnects with
// backoff while the viewer window stays open.
package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/image/webp"

	"github.com/lepe/remmote/internal/api"
	"github.com/lepe/remmote/internal/clipboard"
	"github.com/lepe/remmote/internal/proto"
	"github.com/lepe/remmote/internal/tlsutil"
	"github.com/lepe/remmote/internal/viewer"
)

// Options configures the client.
type Options struct {
	Display       string
	ServerAddr    string
	FastScale     bool          // use nearest-neighbor viewer scaling
	Upscale       int           // 1, 2 or 4: magnify a downscaled stream back to host resolution (match the server's -downscale)
	Quality       int           // 0 = keep server default; 1-100 sends SetQuality
	Once          bool          // exit after the first keyframe (CI mode)
	Snapshot      string        // with Once: write the first full frame as PNG
	SnapshotAfter time.Duration // >0: run live for D, then write the composited canvas as PNG and exit (CI mode; exercises keyframe + delta updates)
	NoClipboard   bool          // disable clipboard synchronization
	TLS           bool          // encrypt the stream
	TLSValue      string        // the -tls argument: default mode, a fingerprint, or a shared secret
	// TLSConfig is the TLS configuration to use as it stands (a paired
	// device's identity). When set, TLS and TLSValue say nothing.
	TLSConfig *tls.Config
}

// Run drives the client until the window closes or ctx is canceled.
func Run(ctx context.Context, opts Options, log *slog.Logger) (err error) {
	callerCtx := ctx
	defer func() {
		if callerCtx.Err() != nil {
			err = nil
		}
	}()
	// A zero Upscale means the default (1), so a bare Options{} is valid —
	// the same normalization server.Options.Downscale gets.
	if opts.Upscale == 0 {
		opts.Upscale = 1
	}
	if opts.Upscale != 1 && opts.Upscale != 2 && opts.Upscale != 4 {
		return fmt.Errorf("client: -upscale must be 1, 2 or 4")
	}
	if opts.Once {
		return runOnce(ctx, opts, log)
	}

	// Cancelable so the snapshot-after timer can stop the session.
	ctx, cancel := context.WithCancel(ctx)

	// One upscaler for the whole session: it owns the scratch buffer, so
	// a reconnect must not throw the allocation away.
	up := newUpscaler(opts.Upscale)

	// Interactive mode: the window lives across reconnects.
	var win *viewer.Window
	var winMu sync.Mutex // win read by the snapshot goroutine
	q := newOutQueue(64, log)
	q.ctx = ctx
	winClosed := make(chan error, 1)
	var haveWindow bool

	// Clipboard watcher on the viewer's display: local copies go to the
	// server, remote text becomes the local clipboard.
	var clip *clipboard.Watcher
	var workers sync.WaitGroup
	defer func() {
		cancel() // unblock queue producers before closing the event connection
		if clip != nil {
			clip.Close()
		}
		if win != nil {
			win.Close()
		}
		workers.Wait()
	}()
	if !opts.NoClipboard {
		if w, err := clipboard.New(opts.Display, 0, log); err != nil {
			log.Warn("clipboard sync unavailable", "err", err)
		} else {
			clip = w
			log.Info("clipboard sync enabled")
			workers.Add(1)
			go func() {
				defer workers.Done()
				w.Run(ctx, func(text string) {
					q.tryEvent(proto.MsgClipboard, (&proto.ClipboardData{Text: text}).Encode())
				})
			}()
		}
	}

	// CI mode: after the delay, snapshot the composited canvas — what the
	// live session decoded (keyframe plus every delta update) — then stop.
	if opts.SnapshotAfter > 0 && opts.Snapshot != "" {
		workers.Add(1)
		go func() {
			defer workers.Done()
			t := time.NewTimer(opts.SnapshotAfter)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			winMu.Lock()
			w := win
			winMu.Unlock()
			switch {
			case w == nil:
				log.Error("snapshot-after: never connected; nothing written", "path", opts.Snapshot)
			default:
				if err := w.Canvas().Snapshot(opts.Snapshot); err != nil {
					log.Error("snapshot-after failed", "err", err)
				} else {
					log.Info("snapshot written", "path", opts.Snapshot)
				}
			}
			cancel()
		}()
	}

	attempt := 0
	for {
		// Connect (with backoff on repeat attempts).
		conn, hello, err := dial(ctx, opts, log, attempt)
		if err != nil {
			// Never fail silently: a dial error swallowed by the backoff
			// loop leaves the user with a client that says nothing while
			// the server logs the symptom.
			if ctx.Err() == nil {
				log.Warn("connect failed; retrying", "server", opts.ServerAddr, "err", err)
			}
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
			// The canvas is host resolution even when the stream is
			// downscaled: the viewer window and its pointer mapping then
			// match the host screen, not the reduced wire size.
			hostW, hostH := up.size(int(hello.Width), int(hello.Height))
			w, err := viewer.Open(opts.Display, hostW, hostH,
				"remmote — "+hello.Name, log, opts.FastScale)
			if err != nil {
				conn.Close()
				return fmt.Errorf("client: open viewer: %w", err)
			}
			winMu.Lock()
			win = w
			winMu.Unlock()
			haveWindow = true
			workers.Add(1)
			go func() { defer workers.Done(); winClosed <- w.Pump(&sender{q: q, div: up.factor}); cancel() }()
		}

		if opts.Quality >= 1 && opts.Quality <= 100 {
			q.event(proto.MsgSetQuality, (&proto.SetQuality{Quality: uint8(opts.Quality)}).Encode())
		}

		log.Info("connected", "server", opts.ServerAddr, "screen",
			fmt.Sprintf("%dx%d", hello.Width, hello.Height), "name", hello.Name, "tls", opts.TLS)

		netErr := session(ctx, conn, win, clip, q, up, log)

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
func session(ctx context.Context, conn net.Conn, win *viewer.Window, clip *clipboard.Watcher, q *outQueue, up *upscaler, log *slog.Logger) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer conn.Close()
	// Cancellation (snapshot timer, Ctrl-C) must not wait for a read
	// timeout: closing the conn unblocks the reader immediately.
	sessDone := make(chan struct{})
	defer close(sessDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-sessDone:
		}
	}()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		writeLoop(ctx, conn, q)
	}()

	defer func() { cancel(); conn.Close(); <-writerDone }()
	dec := newDecoder()
	defer dec.close()
	br := bufio.NewReader(conn)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		t, flags, payload, err := proto.ReadMsg(br)
		if err != nil {
			return err
		}
		switch t {
		case proto.MsgRectUpdate:
			m, err := proto.DecodeRectUpdateView(payload)
			if err != nil {
				return fmt.Errorf("bad RectUpdate: %w", err)
			}
			img, err := dec.decode(m.Codec, m.Data, int(m.W), int(m.H))
			if err != nil {
				log.Warn("decode rect", "codec", m.Codec, "err", err)
				continue
			}
			// Magnify before compositing so the canvas, and with it the
			// viewer's letterbox fit and pointer mapping, stay in host
			// coordinates whatever the server's -downscale is.
			r, img := up.apply(image.Rect(int(m.X), int(m.Y), int(m.X)+int(m.W), int(m.Y)+int(m.H)),
				img, int(m.W), int(m.H))
			if flags&proto.FlagKeyframe != 0 {
				w, h := up.size(int(m.W), int(m.H))
				win.UpdateServerSize(w, h) // keyframe = full screen
			}
			win.Canvas().Composite(r, img)
			win.Dirty()

		case proto.MsgScreenResize:
			m, err := proto.DecodeScreenResize(payload)
			if err != nil {
				return err
			}
			w, h := up.size(int(m.Width), int(m.Height))
			win.UpdateServerSize(w, h)

		case proto.MsgPing:
			m, err := proto.DecodePingPong(payload)
			if err != nil {
				return err
			}
			q.tryEvent(proto.MsgPong, m.Encode())

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

// outQueue carries local input from the viewer pump to the writer
// goroutine.
//
// Two lanes, because they have opposite failure modes: pointer motion
// is a *stream* where only the newest position matters, while buttons and
// keys are discrete events that must never be lost. Motion gets a
// one-slot latest-wins mailbox, so a 1 kHz mouse cannot grow a backlog;
// discrete events get a queue that is never dropped from, so a click can
// never be discarded just because the mouse is busy.
type outQueue struct {
	events chan outMsg // buttons, keys — queued, never dropped
	move   chan outMsg // cap 1, holds only the newest position
	log    *slog.Logger
	ctx    context.Context
}

// inputLagWarn is the threshold at which queued input is reported as
// lagging. Input that waits longer than this is a bug the user feels, so
// it is worth a line in the log on the machine where it happens.
const inputLagWarn = 250 * time.Millisecond

func newOutQueue(events int, log *slog.Logger) *outQueue {
	return &outQueue{
		events: make(chan outMsg, events),
		move:   make(chan outMsg, 1),
		log:    log,
		ctx:    context.Background(),
	}
}

// noteLag reports how long an input event waited between being queued
// here and being handed to the socket.
func (q *outQueue) noteLag(m outMsg) {
	if m.at.IsZero() || q.log == nil {
		return
	}
	if d := time.Since(m.at); d > inputLagWarn {
		q.log.Warn("input lagged in client before reaching the wire",
			"type", m.t.String(), "waited_ms", d.Milliseconds())
	}
}

// mouseMove replaces any position still waiting: the host only needs the
// latest one, and keeping stale ones would delay a later click.
func (q *outQueue) mouseMove(x, y int) {
	m := outMsg{t: proto.MsgMouseMove, at: time.Now(),
		payload: (&proto.MouseMove{X: uint16(x), Y: uint16(y)}).Encode()}
	select {
	case q.move <- m:
		return
	default:
	}
	select { // drop the stale position, then install the new one
	case <-q.move:
	default:
	}
	select {
	case q.move <- m:
	default:
	}
}

// event queues a discrete input event. It waits for room rather than
// dropping: a lost click is a bug the user feels as lag.
func (q *outQueue) event(t proto.MsgType, payload []byte) {
	select {
	case q.events <- outMsg{t: t, at: time.Now(), payload: payload}:
	case <-q.ctx.Done():
	}
}

// tryEvent queues a discrete event only if there is room. For traffic
// that is regenerated on the next tick anyway (pong, clipboard).
func (q *outQueue) tryEvent(t proto.MsgType, payload []byte) {
	select {
	case q.events <- outMsg{t: t, at: time.Now(), payload: payload}:
	default:
	}
}

// takeMove removes the pending position, if any.
func (q *outQueue) takeMove() (outMsg, bool) {
	select {
	case m := <-q.move:
		return m, true
	default:
		return outMsg{}, false
	}
}

// peekMove reports whether a position is pending, without consuming it.
func (q *outQueue) peekMove() (outMsg, bool) {
	m, ok := q.takeMove()
	if ok {
		q.move <- m
	}
	return m, ok
}

func (q *outQueue) eventsLen() int { return len(q.events) }

// writeLoop sends queued input to the server. A pending pointer position
// is always written *before* the button or key it precedes, because XTEST
// applies an event at the current pointer position.
func writeLoop(ctx context.Context, conn net.Conn, q *outQueue) {
	bw := bufio.NewWriter(conn)
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-q.events:
			q.noteLag(m)
			if mv, ok := q.takeMove(); ok {
				if !writeOne(conn, bw, mv) {
					return
				}
			}
			if !writeOne(conn, bw, m) {
				return
			}
		case mv := <-q.move:
			if !writeOne(conn, bw, mv) {
				return
			}
		}
	}
}

// writeOne frames and flushes one outbound message; false = connection
// dead, the writer goroutine must exit.
func writeOne(conn net.Conn, bw *bufio.Writer, m outMsg) bool {
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := proto.WriteMsg(bw, m.t, 0, m.payload); err != nil {
		return false
	}
	return bw.Flush() == nil
}

type outMsg struct {
	t       proto.MsgType
	payload []byte
	at      time.Time // when the event entered the queue
}

// sender adapts the viewer's EventListener onto the outbound queue.
//
// The viewer reports host-screen coordinates, but the wire carries
// stream coordinates: the server multiplies what it receives by its own
// -downscale before injecting (internal/server/session.go). So a client
// that magnified the stream back to host resolution divides on the way
// out, and the round trip lands on the same host pixel either way.
type sender struct {
	q   *outQueue
	div int

	// Window sizes are debounced: a resize drag emits ConfigureNotify
	// per step, and every request that reaches the host costs an X round
	// trip (a RANDR mode change in desktop mode), so only the size the
	// user settles on goes out. gen makes a superseded timer a no-op.
	mu    sync.Mutex
	rw    int
	rh    int
	gen   uint64
	timer *time.Timer
}

func (s *sender) MouseMove(x, y int) { s.q.mouseMove(x/max(s.div, 1), y/max(s.div, 1)) }

func (s *sender) Button(b uint8, down bool) {
	s.q.event(proto.MsgMouseButton, (&proto.MouseButton{Button: b, Down: down}).Encode())
}

func (s *sender) Key(ks uint32, down bool) {
	s.q.event(proto.MsgKey, (&proto.Key{Down: down, Keysym: ks}).Encode())
}

// resizeDebounce is how long a resize must pause before the newest size
// goes on the wire (trailing edge: the last size of a drag wins).
const resizeDebounce = 200 * time.Millisecond

// Resize records the new window size and (re)arms the debounce timer.
func (s *sender) Resize(w, h int) {
	s.mu.Lock()
	s.rw, s.rh = w, h
	s.gen++
	gen := s.gen
	if s.timer != nil {
		s.timer.Stop()
	}
	s.timer = time.AfterFunc(resizeDebounce, func() { s.flushResize(gen) })
	s.mu.Unlock()
}

// flushResize sends the settled size, unless a newer one superseded it.
// Coordinates follow MouseMove's rule: divided by -upscale on the way
// out, multiplied back by -downscale on the server.
func (s *sender) flushResize(gen uint64) {
	s.mu.Lock()
	if gen != s.gen {
		s.mu.Unlock()
		return
	}
	w, h := s.rw, s.rh
	s.mu.Unlock()
	div := max(s.div, 1)
	w, h = max(1, min(w/div, 65535)), max(1, min(h/div, 65535))
	s.q.event(proto.MsgResize, (&proto.Resize{Width: uint16(w), Height: uint16(h)}).Encode())
}

// runOnce connects, waits for the first keyframe, snapshots it, exits.
func runOnce(ctx context.Context, opts Options, log *slog.Logger) error {
	conn, hello, err := dial(ctx, opts, log, 0)
	if err != nil {
		return err
	}
	defer conn.Close()
	stopCancel := context.AfterFunc(ctx, func() { conn.Close() })
	defer stopCancel()
	log.Info("connected", "server", opts.ServerAddr, "name", hello.Name,
		"screen", fmt.Sprintf("%dx%d", hello.Width, hello.Height), "tls", opts.TLS)

	br := bufio.NewReader(conn)
	dec := newDecoder()
	defer dec.close()
	up := newUpscaler(opts.Upscale)
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
		m, err := proto.DecodeRectUpdateView(payload)
		if err != nil {
			return err
		}
		img, err := dec.decode(m.Codec, m.Data, int(m.W), int(m.H))
		if err != nil {
			return fmt.Errorf("decode keyframe: %w", err)
		}
		// Same magnification the interactive path applies, so a -once
		// snapshot is directly comparable with a -snapshot-after one.
		_, img = up.apply(image.Rect(0, 0, int(m.W), int(m.H)), img, int(m.W), int(m.H))
		if opts.Snapshot != "" {
			if err := savePNG(img, opts.Snapshot); err != nil {
				return err
			}
		}
		w, h := up.size(int(m.W), int(m.H))
		log.Info("keyframe received", "width", w, "height", h, "codec", proto.CodecName(m.Codec))
		return nil
	}
	return fmt.Errorf("no keyframe within 10 s")
}

// certHint names the fix for the certificate failure a client is most
// likely to cause: a server started with a shared secret, and a client that
// brought none. What the protocol says on its own ("certificate required",
// "bad certificate") describes the refusal without suggesting the remedy,
// and a refusal with no remedy reads like a broken server. Clients that
// supplied a value already get a message naming the mismatch, so they are
// left alone.
func certHint(unverified bool, err error) error {
	if err == nil || !unverified || !strings.Contains(err.Error(), "certificate") {
		return err
	}
	return fmt.Errorf("%w — the server may have been started with -tls <shared secret>; pass the same value to the client", err)
}

// dial connects and performs the version handshake.
func dial(ctx context.Context, opts Options, log *slog.Logger, attempt int) (net.Conn, *proto.ServerHello, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", opts.ServerAddr)
	if err != nil {
		return nil, nil, err
	}
	stopCancel := context.AfterFunc(ctx, func() { conn.Close() })
	defer stopCancel()
	if tc := tlsutil.TCP(conn); tc != nil {
		_ = tc.SetNoDelay(true)
	}
	// Set before the attach request and the handshake below: a peer that
	// stalls mid-handshake is bounded by this deadline, not just the byte
	// transfer. It also covers a session that is still starting — the
	// daemon answers the attach as soon as the session is live.
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	unverified := false
	cfg := opts.TLSConfig
	if cfg == nil && opts.TLS {
		var err error
		cfg, err = tlsutil.ClientConfig(opts.TLSValue)
		if err != nil {
			conn.Close()
			return nil, nil, err
		}
	}
	if cfg != nil {
		// A configuration with no verification callback accepted whatever
		// certificate arrived; say so rather than let it look verified.
		unverified = cfg.VerifyPeerCertificate == nil && cfg.RootCAs == nil
		tconn := tls.Client(conn, cfg)
		if err := tconn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, nil, certHint(unverified,
				fmt.Errorf("tls handshake with %s: %w", opts.ServerAddr, err))
		}
		conn = tconn
		// Once per successful session: accepting an unverified certificate is
		// a decision the user should see stated, not discover later.
		if unverified {
			log.Warn("TLS: encrypting without verifying the server certificate; pass the server's -tls value (its fingerprint, or the shared secret) to have it checked",
				"server", opts.ServerAddr)
		}
	}

	// Control and stream share the daemon's one port: say which one this
	// connection came for, and it is handed to the stream.
	up, err := api.Upgrade(conn, opts.ServerAddr)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	conn = up

	hello := &proto.ClientHello{Version: proto.ProtoVersion}
	if err := proto.WriteMsg(conn, proto.MsgClientHello, 0, hello.Encode()); err != nil {
		conn.Close()
		return nil, nil, err
	}
	// Read exactly the hello; a temporary buffered reader could swallow
	// the beginning of the first frame when both arrive together. A server
	// that rejects us for a certificate usually surfaces that here rather
	// than during the handshake, since a TLS 1.3 client finishes its own
	// flight before the server inspects what it sent.
	t, _, payload, err := proto.ReadMsg(conn)
	if err != nil {
		conn.Close()
		return nil, nil, certHint(unverified, err)
	}
	if t != proto.MsgServerHello {
		if t == proto.MsgClose {
			if c, err := proto.DecodeClose(payload); err == nil {
				conn.Close()
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

// maxZRAWBytes bounds one decompressed ZRAW rect (8192×8192 px). The
// server never sends more than a full screen, so this only ever rejects
// a corrupt or hostile stream: w and h arrive from the wire as uint16,
// and a legal 32 MiB payload of highly compressible data expands by
// three orders of magnitude — so the size check must happen *before*
// DecodeAll, not after it.
const maxZRAWBytes = 64 << 20

// decoder decodes rect payloads, reusing the ZRAW decompression buffer
// across rects (safe: each decoded image is composited before the next
// decode runs).
type decoder struct {
	zr  *zstd.Decoder
	pix []byte
}

func newDecoder() *decoder {
	zr, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(maxZRAWBytes))
	if err != nil {
		// Only fails on bad options, which are compile-time constants.
		panic(err)
	}
	return &decoder{zr: zr}
}

func (d *decoder) close() { d.zr.Close() }

// decode decompresses a rect payload by codec byte (all pure Go). w, h
// are the rect dimensions from the wire; only ZRAW needs them (JPEG/WebP
// carry their own).
func (d *decoder) decode(codec uint8, data []byte, w, h int) (image.Image, error) {
	switch codec {
	case proto.CodecJPEG:
		return jpeg.Decode(bytes.NewReader(data))
	case proto.CodecWebP:
		return webp.Decode(bytes.NewReader(data))
	case proto.CodecZRAW:
		if w <= 0 || h <= 0 {
			return nil, fmt.Errorf("zraw: empty %dx%d rect", w, h)
		}
		// 64-bit math: w*h*4 overflows int on 32-bit hosts.
		want := int64(w) * int64(h) * 4
		if want > maxZRAWBytes {
			return nil, fmt.Errorf("zraw: %dx%d rect needs %d bytes, cap is %d",
				w, h, want, maxZRAWBytes)
		}
		pix, err := d.zr.DecodeAll(data, d.pix[:0])
		if err != nil {
			return nil, fmt.Errorf("zraw: %w", err)
		}
		if int64(len(pix)) != want {
			return nil, fmt.Errorf("zraw: %d bytes for %dx%d rect", len(pix), w, h)
		}
		d.pix = pix
		return &image.RGBA{Pix: pix, Stride: 4 * w, Rect: image.Rect(0, 0, w, h)}, nil
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
