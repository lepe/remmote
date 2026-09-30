// Package server hosts the remmote screen stream: it owns the capture
// loop (paced by XDamage), the input injector, and one session per
// connected client, broadcasting frames without ever blocking the
// capture loop on a slow client.
package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"image"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jezek/xgb/xproto"

	"github.com/lepe/remmote/internal/capture"
	"github.com/lepe/remmote/internal/clipboard"
	"github.com/lepe/remmote/internal/encode"
	"github.com/lepe/remmote/internal/input"
	"github.com/lepe/remmote/internal/launch"
	"github.com/lepe/remmote/internal/proto"
	"github.com/lepe/remmote/internal/tlsutil"
	"github.com/lepe/remmote/internal/xconn"
	"github.com/lepe/remmote/internal/xwin"
)

// Options configures the server.
type Options struct {
	Display       string
	ListenAddr    string
	FPS           int
	Downscale     int           // 1, 2 or 4: divide wire dimensions; input stays in host coordinates
	Quality       int           // 1-100, initial encoder quality
	Codec         uint8         // proto.CodecJPEG or proto.CodecWebP
	FullRefresh   time.Duration // periodic keyframe interval
	NoClipboard   bool          // disable clipboard synchronization
	Exec          string        // run this command and share only its windows
	Window        uint32        // seed the window set with this window id
	Maximize      bool          // with -exec/-window: maximize the shared window on the host screen
	ResizeDesktop bool          // whole-desktop mode: let a viewer's window resize the host screen (RANDR)
	TLS           bool          // encrypt the stream with TLS (still no client authentication)
	TLSValue      string        // the -tls argument: default mode, a shared secret, or a fingerprint
	TLSCertFile   string        // PEM certificate for -tls ("" = generate and cache one)
	TLSKeyFile    string        // PEM private key for -tls ("" = generate and cache one)
}

// StreamSource is the capture backend the server streams: the root
// window (capture.Capturer) or an application's window set
// (capture.Scene). Both satisfy this interface.
type StreamSource interface {
	FPS() int
	FullRefresh() time.Duration
	ScreenRect() image.Rectangle
	HasDamage() bool
	Changed() <-chan struct{}
	Resized() <-chan struct{}
	TakePending() []image.Rectangle
	Capture(r image.Rectangle) (image.Image, error)
	ApplyResize() error
	Close() error
}

// dumper is the optional diagnostics path (-dump-frame).
type dumper interface{ DumpFrame(path string) error }

// surfaceResizer is the optional capability of a StreamSource: making the
// shared surface match a viewer's window size. The root screen does it
// through RANDR (and may refuse), an application window through EWMH.
type surfaceResizer interface {
	ResizeTo(w, h int) error
}

// Bounds on a viewer-driven resize request, in host pixels. Requests
// arrive from the wire (uint16) and are only ever a viewer window's size,
// so these exist to keep a malformed or hostile value from asking the X
// server for a 65535 px framebuffer or a 1 px one.
const (
	minResizePx = 64
	maxResizePx = 16384
)

// Server is the running screen-sharing host.
type Server struct {
	opts Options
	log  *slog.Logger

	xc      *xconn.Conn
	ixc     *xconn.Conn // input-only: never blocked by capture
	src     StreamSource
	inj     *input.Injector
	rt      *input.Router // input front-end: bare injector (root) or window router
	enc     encode.Encoder
	clip    *clipboard.Watcher // nil when disabled
	proc    *launch.Proc       // nil unless -exec
	appName string             // window-mode display name

	// Viewer-driven resize: latest-wins handoff from the session readers
	// to the capture loop, which is the only goroutine allowed to touch
	// the capture source. resizer is nil when the source cannot resize.
	resizer      surfaceResizer
	resizeReq    chan image.Point
	resizeWarned atomic.Bool
	desktop      bool // sharing the root screen (the -resize-desktop policy applies)

	quality atomic.Int32

	mu       sync.Mutex
	sessions map[uint64]*session
	nextID   uint64
	workers  sync.WaitGroup

	kick chan struct{} // a new session wants a keyframe

	// Capture-loop-owned stats (read by the stats ticker under encMu).
	seq     uint32
	encMu   sync.Mutex
	encRing [16]time.Duration
	encIdx  int

	// Capture-loop pacing state (capture-loop goroutine only).
	lastSend      time.Time // last frame batch start (fps pacing)
	dirtySinceKey bool      // delta frames sent since the last keyframe

	// Atomic counters reset by the stats ticker.
	frames   atomic.Int64
	bytesOut atomic.Int64
	encZRAW  atomic.Int64
	encJPEG  atomic.Int64
	encOther atomic.Int64
}

// New wires the X stack together. The returned server must be Run.
func New(opts Options, log *slog.Logger) (*Server, error) {
	return NewContext(context.Background(), opts, log)
}

// NewContext also permits cancellation while waiting for a launched app.
func NewContext(ctx context.Context, opts Options, log *slog.Logger) (*Server, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.Downscale == 0 {
		opts.Downscale = 1
	}
	if opts.Downscale != 1 && opts.Downscale != 2 && opts.Downscale != 4 {
		return nil, fmt.Errorf("server: downscale must be 1, 2 or 4")
	}

	if opts.Quality < 1 || opts.Quality > 100 {
		return nil, fmt.Errorf("server: quality %d out of range 1..100", opts.Quality)
	}
	if opts.ResizeDesktop && (opts.Exec != "" || opts.Window != 0) {
		log.Warn("-resize-desktop has no effect when sharing an application; the viewer resizes the shared window itself")
	}
	xc, err := xconn.Dial(opts.Display)
	if err != nil {
		return nil, err
	}
	if !xc.Ext.XTEST {
		xc.Close()
		return nil, fmt.Errorf("server: XTEST extension missing on %s — input injection is mandatory for remote control",
			xconn.DisplayString(opts.Display))
	}
	if xc.Ext.DAMAGE {
		log.Info("damage tracking enabled")
	} else {
		log.Warn("DAMAGE missing: streaming periodic full frames only")
	}
	if xc.Ext.SHM {
		log.Info("MIT-SHM capture enabled")
	} else {
		log.Warn("MIT-SHM missing: using slower core GetImage")
	}

	km, err := input.LoadKeymap(xc.X)
	if err != nil {
		xc.Close()
		return nil, err
	}
	sw, sh := xc.ScreenSize()
	// Input gets its own X connection. Sharing the capture connection
	// serialises every XTEST request behind SHM GetImage and the
	// geometry round trips of the capture loop, so injection latency
	// would follow the capture load — on a busy host that is exactly the
	// "mouse feels laggy" symptom.
	ixc, err := xconn.Dial(opts.Display)
	if err != nil {
		xc.Close()
		return nil, fmt.Errorf("server: input X connection: %w", err)
	}
	inj, err := input.NewInjector(ixc.X, km, sw, sh, log)
	if err != nil {
		ixc.Close()
		xc.Close()
		return nil, fmt.Errorf("server: XTEST init: %w", err)
	}

	// Resolve the capture mode: -exec spawns and shares an application,
	// -window seeds the set from an existing window, else the root.
	var (
		src     StreamSource
		router  *input.Router
		proc    *launch.Proc
		appName string
	)
	switch {
	case opts.Exec != "":
		res, err := launch.Bootstrap(ctx,
			xwin.NewClient(xc.X, xc.Root()), opts.Exec, opts.Display, log)
		if err != nil {
			xc.Close()
			return nil, fmt.Errorf("server: exec %q: %w", opts.Exec, err)
		}
		proc = res.Proc
		if err := ctx.Err(); err != nil {
			proc.Kill()
			ixc.Close()
			xc.Close()
			return nil, err
		}
		seed := capture.SceneSeed{Windows: nil, PID: uint32(res.Proc.Pid()), Class: res.Class,
			Maximize: opts.Maximize}
		if res.Win != 0 {
			seed.Windows = []xproto.Window{res.Win}
		}
		sc, err := capture.NewScene(xc, capture.Options{FPS: opts.FPS, FullRefresh: opts.FullRefresh}, seed, log)
		if err != nil {
			proc.Kill()
			xc.Close()
			return nil, err
		}
		src, router, appName = sc, input.NewRouter(inj, ixc.X, sc, log), execName(opts.Exec)

	case opts.Window != 0:
		// Adopt the seed's WM_CLASS so sibling windows of the same app
		// join the tracked set during rescans.
		xq := xwin.NewClient(xc.X, xc.Root())
		_, class, _ := xq.WMClass(xproto.Window(opts.Window))
		sc, err := capture.NewScene(xc, capture.Options{FPS: opts.FPS, FullRefresh: opts.FullRefresh},
			capture.SceneSeed{Windows: []xproto.Window{xproto.Window(opts.Window)}, Class: class,
				Maximize: opts.Maximize}, log)
		if err != nil {
			xc.Close()
			return nil, err
		}
		src, router = sc, input.NewRouter(inj, ixc.X, sc, log)

	default:
		if opts.Maximize {
			log.Warn("-maximize has no effect when sharing the whole desktop; use it with -exec or -window")
		}
		cap, err := capture.New(xc, capture.Options{FPS: opts.FPS, FullRefresh: opts.FullRefresh}, log)
		if err != nil {
			xc.Close()
			return nil, err
		}
		src = cap
	}
	if router == nil {
		router = input.NewRouter(inj, ixc.X, nil, log)
	}
	// The resize capability is asked of the unwrapped source: the scaling
	// wrapper below only changes wire dimensions, never the surface.
	var resizer surfaceResizer
	if r, ok := src.(surfaceResizer); ok {
		resizer = r
	}
	desktop := opts.Exec == "" && opts.Window == 0

	enc, err := encode.New(opts.Codec)
	if err != nil {
		if proc != nil {
			proc.Kill()
		}
		src.Close()
		xc.Close()
		return nil, err
	}

	if opts.Downscale > 1 {
		src = &scaledSource{StreamSource: src, factor: opts.Downscale}
	}
	s := &Server{
		opts:      opts,
		log:       log,
		xc:        xc,
		ixc:       ixc,
		src:       src,
		inj:       inj,
		rt:        router,
		enc:       enc,
		proc:      proc,
		appName:   appName,
		resizer:   resizer,
		resizeReq: make(chan image.Point, 1),
		desktop:   desktop,
		sessions:  make(map[uint64]*session),
		kick:      make(chan struct{}, 1),
	}
	s.quality.Store(int32(opts.Quality))
	return s, nil
}

func execName(cmdline string) string {
	if f := strings.Fields(cmdline); len(f) > 0 {
		return f[0]
	}
	return cmdline
}

// DumpFrame captures the whole source once and writes it as PNG — the
// bring-up and diagnostics path (-dump-frame).
func (s *Server) DumpFrame(path string) error {
	if d, ok := s.src.(dumper); ok {
		return d.DumpFrame(path)
	}
	return fmt.Errorf("server: source cannot dump frames")
}

// TestInject moves the host pointer 50 px right and back — the input
// bring-up path (-test-inject).
func (s *Server) TestInject() {
	s.inj.MovePointer(50, 50)
	time.Sleep(200 * time.Millisecond)
	s.inj.MovePointer(150, 80)
	s.inj.Key(0x61, true) // 'a'
	s.inj.Key(0x61, false)
	s.ixc.X.Sync() // diagnostics exits immediately: wait for queued input
}

// Run serves until ctx is canceled, then tears everything down. It
// blocks.
func (s *Server) Run(ctx context.Context) (err error) {
	var inputDone, appDone chan struct{}
	defer func() {
		s.shutdown()
		if inputDone != nil {
			<-inputDone
		}
		if appDone != nil {
			<-appDone
		}
	}()

	ln, err := net.Listen("tcp", s.opts.ListenAddr)
	if err != nil {
		return fmt.Errorf("server: listen %s: %w", s.opts.ListenAddr, err)
	}
	fingerprint := ""
	clientAuth := false
	if s.opts.TLS {
		cfg, tlsErr := tlsutil.ServerConfig(s.opts.TLSCertFile, s.opts.TLSKeyFile, s.opts.TLSValue)
		if tlsErr != nil {
			_ = ln.Close()
			return tlsErr
		}
		fp, fpErr := tlsutil.Fingerprint(cfg.Certificates[0].Certificate[0])
		if fpErr != nil {
			_ = ln.Close()
			return fpErr
		}
		fingerprint = fp
		// Only the shared-secret mode asks clients for a certificate; say
		// which of the two you are in, since only one of them is a check.
		clientAuth = cfg.ClientAuth != tls.NoClientCert
		ln = tls.NewListener(ln, cfg)
	}
	switch {
	case !s.opts.TLS:
		s.log.Warn("NO AUTHENTICATION, NO ENCRYPTION: anyone who can reach this port gains full control of this machine",
			"listen", s.opts.ListenAddr, "display", xconn.DisplayString(s.opts.Display))
	case clientAuth:
		s.log.Warn("TLS: encrypted, and clients are authenticated — only a peer holding the shared secret can connect, and anyone with it gains full control of this machine",
			"listen", s.opts.ListenAddr, "display", xconn.DisplayString(s.opts.Display),
			"fingerprint", fingerprint)
	default:
		// Kept as loud as the plaintext banner: encryption alone does not
		// make this machine private to anyone who can reach the port.
		s.log.Warn("TLS: the stream is encrypted, but there is no client authentication — anyone who completes the handshake gains full control of this machine",
			"listen", s.opts.ListenAddr, "display", xconn.DisplayString(s.opts.Display),
			"fingerprint", fingerprint)
	}
	r := s.src.ScreenRect()
	s.log.Info("listening", "addr", ln.Addr().String(),
		"source", fmt.Sprintf("%dx%d", r.Dx(), r.Dy()),
		"codec", codecName(s.enc.Codec()), "fps", s.src.FPS())

	if s.proc != nil {
		appDone = make(chan struct{})
		go func() {
			defer close(appDone)
			<-s.proc.Done()
			if ctx.Err() == nil {
				s.log.Info("application exited; server staying up", "err", s.proc.Err())
			}
		}()
	}
	// Unchecked input requests preserve X ordering without a round trip per
	// event. Drain asynchronous protocol errors on the dedicated connection.
	inputDone = make(chan struct{})
	go func() {
		defer close(inputDone)
		for {
			ev, err := s.ixc.X.WaitForEvent()
			if err != nil {
				s.log.Warn("input X error", "err", err)
			}
			if ev == nil && err == nil {
				return
			}
		}
	}()
	s.startClipboard(ctx)
	s.workers.Add(4)
	go func() { defer s.workers.Done(); s.acceptLoop(ctx, ln) }()
	go func() { defer s.workers.Done(); s.captureLoop(ctx) }()
	go func() { defer s.workers.Done(); s.statsLoop(ctx) }()
	go func() { defer s.workers.Done(); s.pingLoop(ctx) }()

	<-ctx.Done()
	s.log.Info("shutting down")
	_ = ln.Close() // wake Accept before waiting for connection handlers
	if s.clip != nil {
		s.clip.Close()
	}
	s.workers.Wait() // no capture or input may touch X/SHM after this point
	return nil
}

// startClipboard launches the clipboard watcher: local copies are pushed
// to every client; remote text becomes the host clipboard.
func (s *Server) startClipboard(ctx context.Context) {
	if s.opts.NoClipboard {
		s.log.Info("clipboard sync disabled")
		return
	}
	w, err := clipboard.New(s.opts.Display, 0, s.log)
	if err != nil {
		s.log.Warn("clipboard sync unavailable", "err", err)
		return
	}
	s.clip = w
	s.log.Info("clipboard sync enabled")
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		w.Run(ctx, func(text string) {
			if len(text) > proto.MaxClipboard {
				return
			}
			s.forEachSession(func(sess *session) {
				sess.deliver(newFrame(proto.MsgClipboard, 0, (&proto.ClipboardData{Text: text}).Encode(), false))
			})
		})
	}()
}

func (s *Server) acceptLoop(ctx context.Context, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed (shutdown)
		}
		if ctx.Err() != nil {
			conn.Close()
			return
		}
		s.workers.Add(1)
		go func() { defer s.workers.Done(); s.handleConn(ctx, conn) }()
	}
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer conn.Close()
	finished := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-finished:
			return
		case <-ctx.Done():
		}
		_ = conn.SetReadDeadline(time.Now())
		// A writer may be blocked in a frame write. Allow a brief goodbye,
		// then force-close even when that peer never reads again.
		timer := time.NewTimer(shutdownGrace)
		defer timer.Stop()
		select {
		case <-finished:
		case <-timer.C:
			conn.Close()
		}
	}()
	defer func() { close(finished); <-watcherDone }()
	sess, err := s.handshake(conn)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("handshake failed", "remote", conn.RemoteAddr(), "err", err)
		}
		return
	}
	defer s.deregister(sess)
	if ctx.Err() != nil {
		return
	}
	s.register(sess)
	s.log.Info("client connected", "remote", conn.RemoteAddr(), "id", sess.id)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() { defer workers.Done(); sess.writer(ctx) }()
	go func() { defer workers.Done(); sess.pump.run(ctx) }()
	sess.reader(ctx)
	cancel()
	workers.Wait()
	s.log.Info("client gone", "remote", conn.RemoteAddr(), "id", sess.id,
		"frames", sess.framesOut.Load(), "dropped", sess.dropped.Load())
}

func (s *Server) handshake(conn net.Conn) (*session, error) {
	// Look through a TLS wrapper too, or these stop applying the moment
	// -tls is switched on.
	if tc := tlsutil.TCP(conn); tc != nil {
		_ = tc.SetNoDelay(true)
		// Bound stale frames already handed to TCP; a multi-MiB buffer can
		// hide seconds of desktop updates from the bounded outbox.
		_ = tc.SetWriteBuffer(256 << 10)
		_ = tc.SetReadBuffer(1 << 20)
	}
	// Set before the first read: with TLS that read also runs the
	// handshake, so a peer that stalls mid-handshake is bounded too.
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	// This bufio.Reader must live for the whole session: it may hold
	// buffered bytes past the hello.
	br := bufio.NewReader(conn)
	t, _, payload, err := proto.ReadMsg(br)
	if err != nil {
		return nil, fmt.Errorf("read hello: %w", err)
	}
	if t != proto.MsgClientHello {
		return nil, fmt.Errorf("expected ClientHello, got %v", t)
	}
	hello, err := proto.DecodeClientHello(payload)
	if err != nil {
		return nil, err
	}
	if hello.Version != proto.ProtoVersion {
		c := &proto.Close{Code: proto.CloseVersion, Reason: fmt.Sprintf("server speaks version %d, client %d",
			proto.ProtoVersion, hello.Version)}
		_ = proto.WriteMsg(conn, proto.MsgClose, 0, c.Encode())
		return nil, fmt.Errorf("version mismatch: client %d, server %d", hello.Version, proto.ProtoVersion)
	}

	name, _ := os.Hostname()
	if s.appName != "" {
		name = s.appName + "@" + name
	}
	rect := s.src.ScreenRect()
	srvHello := &proto.ServerHello{
		Version: proto.ProtoVersion,
		Width:   uint16(rect.Dx()),
		Height:  uint16(rect.Dy()),
		Depth:   s.xc.Depth(),
		Quality: uint8(s.quality.Load()),
		Name:    name,
	}
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := proto.WriteMsg(conn, proto.MsgServerHello, 0, srvHello.Encode()); err != nil {
		return nil, fmt.Errorf("write ServerHello: %w", err)
	}

	s.mu.Lock()
	s.nextID++
	sess := newSession(s, s.nextID, conn, br)
	s.sessions[sess.id] = sess
	s.mu.Unlock()
	return sess, nil
}

func (s *Server) register(sess *session) {
	// A freshly connected client must get a keyframe before any delta:
	// kick the capture loop.
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *Server) deregister(sess *session) {
	s.mu.Lock()
	delete(s.sessions, sess.id)
	s.mu.Unlock()
}

// broadcast hands f to every session without blocking. Slow clients drop
// the frame and are marked for a keyframe.
func (s *Server) broadcast(f *frame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		sess.deliver(f)
	}
}

func (s *Server) forEachSession(fn func(*session)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		fn(sess)
	}
}

func (s *Server) shutdown() {
	if s.proc != nil {
		s.proc.Kill()
	}
	if err := s.src.Close(); err != nil {
		s.log.Warn("capturer close", "err", err)
	}
	s.ixc.Close()
	s.xc.Close()
}

// requestResize hands a viewer's window size (already in host
// coordinates) to the capture loop, which is the only goroutine allowed
// to resize the shared surface. Latest wins: dragging a window edge
// produces a burst of sizes, and only the last one is worth an X round
// trip.
func (s *Server) requestResize(w, h int) {
	if s.resizer == nil {
		return
	}
	w = min(max(w, minResizePx), maxResizePx)
	h = min(max(h, minResizePx), maxResizePx)
	if s.desktop && !s.opts.ResizeDesktop {
		if s.resizeWarned.CompareAndSwap(false, true) {
			s.log.Warn("a viewer asked to resize the host screen; ignoring it — start the server with -resize-desktop to let viewers change the screen size",
				"width", w, "height", h)
		}
		return
	}
	p := image.Pt(w, h)
	select {
	case s.resizeReq <- p:
	default:
		select { // drop the stale size, install the newest one
		case <-s.resizeReq:
		default:
		}
		select {
		case s.resizeReq <- p:
		default:
		}
	}
}

// applyResize applies one recorded viewer size to the shared surface.
// Capture-loop only.
func (s *Server) applyResize(p image.Point) {
	if s.resizer == nil {
		return
	}
	if err := s.resizer.ResizeTo(p.X, p.Y); err != nil {
		s.log.Warn("shared surface refused to resize; the viewer keeps letterboxing",
			"width", p.X, "height", p.Y, "err", err)
		return
	}
	s.log.Debug("shared surface resize requested", "width", p.X, "height", p.Y)
}

// captureLoop paces frame production: damage-driven with an fps-capped
// merge window, plus keyframes on resize, client join, dropped frames, and
// (when anything changed) the periodic refresh ticker. It is the only
// goroutine that captures.
func (s *Server) captureLoop(ctx context.Context) {
	minInterval := time.Second / time.Duration(s.src.FPS())
	ticker := time.NewTicker(s.src.FullRefresh())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.kick:
			s.sendKeyframe()
		case p := <-s.resizeReq:
			s.applyResize(p)
		case <-s.src.Resized():
			if err := s.src.ApplyResize(); err != nil {
				s.log.Error("apply resize", "err", err)
				continue
			}
			rect := s.src.ScreenRect()
			s.broadcast(newFrame(proto.MsgScreenResize, 0,
				(&proto.ScreenResize{Width: uint16(rect.Dx()), Height: uint16(rect.Dy())}).Encode(), false))
			s.sendKeyframe()
		case <-ticker.C:
			// Keyframes exist to heal drift: with no deltas since the
			// last one there is nothing to heal, so an idle desktop
			// costs no capture, no encode, no bandwidth. Without damage
			// tracking the ticker is the only frame source — stay
			// unconditional there.
			if s.dirtySinceKey || !s.src.HasDamage() {
				s.sendKeyframe()
			}
		case <-s.src.Changed():
			if !s.src.HasDamage() {
				continue // no tracking: ticker supplies full frames
			}
			// Merge window: cap the frame rate by waiting out the
			// remainder of the interval since the last batch — never a
			// full interval, so an update after an idle gap goes out
			// immediately.
			if wait := minInterval - time.Since(s.lastSend); wait > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
				}
			}
			rects := s.src.TakePending()
			if len(rects) == 0 {
				continue
			}
			s.lastSend = time.Now()
			union := image.Rectangle{}
			for _, r := range rects {
				union = union.Union(r)
			}
			if capture.ShouldFullRefresh(union, s.src.ScreenRect()) {
				s.sendKeyframe()
			} else {
				for _, r := range rects {
					s.sendFrame(r, false)
				}
				s.dirtySinceKey = true
			}
		}
		// Anyone who dropped frames gets a keyframe now.
		if s.takeNeedsKeyframe() {
			s.sendKeyframe()
		}
	}
}

func (s *Server) takeNeedsKeyframe() bool {
	needed := false
	s.forEachSession(func(sess *session) {
		if sess.needKey.CompareAndSwap(true, false) {
			needed = true
		}
	})
	return needed
}

// sendKeyframe captures and broadcasts the full screen.
func (s *Server) sendKeyframe() {
	s.lastSend = time.Now()
	s.sendFrame(s.src.ScreenRect(), true)
}

// sendFrame captures r, encodes it, and broadcasts. Capture-loop only.
func (s *Server) sendFrame(r image.Rectangle, keyframe bool) {
	if r.Empty() {
		return
	}
	img, err := s.src.Capture(r)
	if err != nil {
		s.log.Warn("capture", "rect", r, "err", err)
		return
	}
	t0 := time.Now()
	codec, data, err := s.enc.Encode(img, int(s.quality.Load()))
	s.recordEncode(time.Since(t0))
	if err != nil {
		s.log.Warn("encode", "err", err)
		return
	}
	s.seq++
	msg := &proto.RectUpdate{
		Seq:   s.seq,
		X:     uint16(r.Min.X),
		Y:     uint16(r.Min.Y),
		W:     uint16(r.Dx()),
		H:     uint16(r.Dy()),
		Codec: codec,
		Data:  data,
	}
	flags := uint8(0)
	if keyframe {
		flags = proto.FlagKeyframe
		s.dirtySinceKey = false
	}
	s.frames.Add(1)
	s.bytesOut.Add(int64(len(data)))
	switch codec {
	case proto.CodecZRAW:
		s.encZRAW.Add(1)
	case proto.CodecJPEG:
		s.encJPEG.Add(1)
	default:
		s.encOther.Add(1)
	}
	s.broadcast(newFrame(proto.MsgRectUpdate, flags, msg.Encode(), keyframe))
}

func (s *Server) recordEncode(d time.Duration) {
	s.encMu.Lock()
	s.encRing[s.encIdx] = d
	s.encIdx = (s.encIdx + 1) % len(s.encRing)
	s.encMu.Unlock()
}

// pingLoop pings every client every 10 s so dead peers are reaped by the
// 30 s read deadline instead of hanging on a half-open TCP connection.
func (s *Server) pingLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p := (&proto.PingPong{Nonce: uint64(time.Now().UnixNano()), TS: uint64(time.Now().UnixMilli())}).Encode()
			s.forEachSession(func(sess *session) {
				sess.deliver(newFrame(proto.MsgPing, 0, p, false))
			})
		}
	}
}

func (s *Server) statsLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			frames := s.frames.Swap(0)
			bytes := s.bytesOut.Swap(0)
			zraw := s.encZRAW.Swap(0)
			jpeg := s.encJPEG.Swap(0)
			other := s.encOther.Swap(0)
			var drops uint64
			n := 0
			var inEvents, inMoves, inDropped int64
			var inWaitMax time.Duration
			s.forEachSession(func(sess *session) {
				n++
				drops += sess.dropped.Load()
				st := sess.pump.takeInputStats()
				inEvents += st.events
				inMoves += st.moves
				inDropped += st.dropped
				if st.waitMax > inWaitMax {
					inWaitMax = st.waitMax
				}
			})
			s.log.Info("stats", "clients", n, "frames", frames,
				"kbps", bytes/10240, // bytes per 10 s → kB/s
				"encode_p95ms", s.encodeP95().Milliseconds(),
				"zraw", zraw, "jpeg", jpeg, "other", other,
				"in_events", inEvents, "in_moves", inMoves,
				"in_wait_maxms", inWaitMax.Milliseconds(),
				"dropped", drops)
		}
	}
}

func (s *Server) encodeP95() time.Duration {
	s.encMu.Lock()
	defer s.encMu.Unlock()
	var ds []time.Duration
	for _, d := range s.encRing {
		if d > 0 {
			ds = append(ds, d)
		}
	}
	if len(ds) == 0 {
		return 0
	}
	// Small n: simple insertion sort for the p95 index.
	for i := 1; i < len(ds); i++ {
		for j := i; j > 0 && ds[j] < ds[j-1]; j-- {
			ds[j], ds[j-1] = ds[j-1], ds[j]
		}
	}
	return ds[(len(ds)*95-1)/100]
}

// codecName labels the configured encoder for logs; the hybrid
// pseudo-codec never appears on the wire, so proto.CodecName doesn't
// cover it.
func codecName(c uint8) string {
	if c == encode.CodecHybrid {
		return "hybrid"
	}
	return proto.CodecName(c)
}
