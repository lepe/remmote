// Package daemon hosts the remmote control API: one listener, one
// session, and the lifecycle around both — starting the shared surface
// from a SessionSpec, upgrading viewers to the binary stream, and
// terminating the service when the session is deliberately ended.
//
// The session outlives every viewer: closing a window detaches, nothing
// more. Ending a session is an explicit act, and it ends the service too
// — there is one session per daemon and no reason to sit idle after it.
package daemon

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lepe/remmote/internal/api"
	"github.com/lepe/remmote/internal/hostenv"
	"github.com/lepe/remmote/internal/proto"
	"github.com/lepe/remmote/internal/stream"
	"github.com/lepe/remmote/internal/tlsutil"
)

// Options configures the daemon.
type Options struct {
	ListenAddr  string
	TLS         bool   // encrypt the listener (control and stream alike)
	TLSValue    string // the -tls argument: default mode, a shared secret, or a fingerprint
	TLSCertFile string // PEM certificate for -tls ("" = generate and cache one)
	TLSKeyFile  string // PEM private key for -tls ("" = generate and cache one)

	// Initial is the session to start at boot (from the daemon's own
	// flags). Nil means idle: the daemon waits for a client to say what
	// to share.
	Initial *api.SessionSpec

	// AllowExec names the commands a *client* may ask to launch (source
	// "app"), by basename. Empty — the default — refuses all of them:
	// launching a program on someone else's machine is the operator's
	// decision. A session started from Initial is never restricted.
	AllowExec []string

	Log *slog.Logger
}

// Daemon is a running service: a control API over one listener and at
// most one session.
type Daemon struct {
	opts Options
	log  *slog.Logger
	ring *logRing

	ctx    context.Context
	cancel context.CancelFunc

	mu   sync.Mutex
	sess *session
}

// session is the one session a daemon can hold, and everything the
// daemon needs to stop it again.
type session struct {
	spec api.SessionSpec
	ctx  context.Context // canceled to stop it
	// attachWG tracks the viewers currently inside stream.Attach; no
	// Attach may start once stopping is set, so waiting is safe.
	attachWG sync.WaitGroup
	done     chan struct{} // closed when runSession has released everything
	ready    chan struct{} // closed when the session is live or has failed

	cancel   context.CancelFunc
	stopping bool

	// Written by runSession, read under the daemon's mutex.
	state   string
	err     string
	started time.Time
	display string         // the display actually shared ("" until one is resolved)
	st      *stream.Stream // nil while starting or after a failure
}

// New builds a daemon; it must be Run.
func New(opts Options) (*Daemon, error) {
	if opts.Log == nil {
		opts.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Daemon{opts: opts, log: opts.Log, ring: newLogRing(200)}, nil
}

// Run serves until ctx is canceled or the session is terminated, then
// tears everything down. A deliberate terminate returns nil: the service
// stops because it was asked to, not because it broke.
func (d *Daemon) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	d.ctx, d.cancel = ctx, cancel

	ln, err := net.Listen("tcp", d.opts.ListenAddr)
	if err != nil {
		return fmt.Errorf("daemon: listen %s: %w", d.opts.ListenAddr, err)
	}

	srv := &http.Server{
		Handler:  d.mux(),
		ErrorLog: log.New(&slogWriter{d.log}, "", 0),
	}
	if d.opts.TLS {
		cfg, err := tlsutil.ServerConfig(d.opts.TLSCertFile, d.opts.TLSKeyFile, d.opts.TLSValue)
		if err != nil {
			_ = ln.Close()
			return err
		}
		fingerprint, err := tlsutil.Fingerprint(cfg.Certificates[0].Certificate[0])
		if err != nil {
			_ = ln.Close()
			return err
		}
		// Only the shared-secret mode asks clients for a certificate; say
		// which of the two you are in, since only one of them is a check.
		if cfg.ClientAuth != tls.NoClientCert {
			d.log.Warn("TLS: encrypted, and clients are authenticated — only a peer holding the shared secret can connect, and anyone with it gains full control of this machine",
				"listen", d.opts.ListenAddr, "fingerprint", fingerprint)
		} else {
			// Kept as loud as the plaintext banner: encryption alone does
			// not make this machine private to anyone who can reach the port.
			d.log.Warn("TLS: the stream is encrypted, but there is no client authentication — anyone who completes the handshake gains full control of this machine",
				"listen", d.opts.ListenAddr, "fingerprint", fingerprint)
		}
		srv.TLSConfig = cfg
	} else {
		d.log.Warn("NO AUTHENTICATION, NO ENCRYPTION: anyone who can reach this port gains full control of this machine",
			"listen", d.opts.ListenAddr)
	}
	d.log.Info("listening", "addr", ln.Addr().String(),
		"proto", proto.ProtoVersion, "tls", d.opts.TLS,
		"exec", strings.Join(d.opts.AllowExec, ","))

	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		if d.opts.TLS {
			_ = srv.ServeTLS(ln, "", "")
		} else {
			_ = srv.Serve(ln)
		}
	}()

	if d.opts.Initial != nil {
		spec := *d.opts.Initial
		if _, err := d.startSession(spec, false); err != nil {
			d.log.Warn("startup session failed", "err", err)
		}
	}

	<-ctx.Done()
	d.stopSession()
	shutCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	_ = srv.Shutdown(shutCtx)
	<-serveDone
	return nil
}

// mux routes the control API.
func (d *Daemon) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+api.PathHost, d.handleHost)
	mux.HandleFunc("GET "+api.PathSession, d.handleSessionGet)
	mux.HandleFunc("POST "+api.PathSession, d.handleSessionPost)
	mux.HandleFunc("DELETE "+api.PathSession, d.handleSessionDelete)
	mux.HandleFunc("GET "+api.PathEvents, d.handleEvents)
	mux.HandleFunc("POST "+api.PathAttach, d.handleAttach)
	return mux
}

func (d *Daemon) handleHost(w http.ResponseWriter, r *http.Request) {
	h := api.HostInfo{
		ProtoVersion:   int(proto.ProtoVersion),
		Codecs:         api.Codecs(),
		TLS:            d.opts.TLS,
		CanCreate:      hostenv.Available(),
		WindowManagers: hostenv.WMs(),
		AllowExec:      len(d.opts.AllowExec) > 0,
	}
	d.mu.Lock()
	if d.sess != nil {
		info := d.infoLocked(d.sess, false)
		h.Session = &info
	}
	d.mu.Unlock()
	writeJSON(w, http.StatusOK, h)
}

func (d *Daemon) handleSessionGet(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	s := d.sess
	var info api.SessionInfo
	if s != nil {
		info = d.infoLocked(s, true)
	}
	d.mu.Unlock()
	if s == nil {
		apiError(w, http.StatusNotFound, "no session is running on this daemon")
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (d *Daemon) handleSessionPost(w http.ResponseWriter, r *http.Request) {
	var spec api.SessionSpec
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&spec); err != nil {
		apiError(w, http.StatusBadRequest, "bad session spec: %v", err)
		return
	}
	if err := spec.Validate(); err != nil {
		apiError(w, http.StatusBadRequest, "%v", err)
		return
	}
	// The rest is policy the daemon owns, not the spec: what it can and
	// will do on this machine.
	if _, err := spec.StreamOptions(); err != nil {
		apiError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if spec.Display.Kind == api.KindCreate {
		if !hostenv.Available() {
			apiError(w, http.StatusBadRequest,
				"this daemon cannot create displays: Xvfb or Xephyr is not installed")
			return
		}
		if wm := spec.Display.Create.WM; wm != "" && !hostenv.HasWM(wm) {
			apiError(w, http.StatusBadRequest, "window manager %q is not installed here", wm)
			return
		}
	}
	if spec.Source == api.SourceApp && !d.execAllowed(spec.App.Command) {
		apiError(w, http.StatusForbidden,
			"this daemon does not allow launching applications; start it with -allow-exec %s",
			commandBase(spec.App.Command))
		return
	}
	info, err := d.startSession(spec, r.URL.Query().Get("replace") == "1")
	if err != nil {
		if e, ok := err.(*api.Error); ok {
			apiError(w, e.Status, "%s", e.Message)
			return
		}
		apiError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// handleSessionDelete is the deliberate end: the session stops and the
// service with it. The response goes out first — the daemon exits once
// it has been delivered.
func (d *Daemon) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	d.stopSession()
	d.log.Info("session terminated by request; stopping the service")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
	go d.cancel()
}

// handleEvents streams state changes and log lines as SSE.
func (d *Daemon) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		apiError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	// Flush the headers now: a subscriber is waiting on this response
	// before it says anything, and net/http would otherwise sit on them.
	fl.Flush()

	ch, unsub := d.ring.Subscribe()
	defer unsub()

	// Where we are right now, so a late subscriber is not blind until the
	// next change.
	d.mu.Lock()
	state := ""
	if d.sess != nil {
		state = d.sess.state
	}
	d.mu.Unlock()
	if state != "" {
		writeEvent(w, fl, api.Event{Type: "state", State: state, Time: time.Now()})
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			writeEvent(w, fl, ev)
		}
	}
}

// attachWait bounds how long an attach waits for a session that is still
// starting: a viewer connects as soon as something is being shared, and
// creating a display or finding an application's window takes a moment.
// Long enough for a window search, short enough to answer by.
const attachWait = 20 * time.Second

// handleAttach upgrades the connection to the binary remmote stream and
// hands it to the session's viewers. This is where "detached" and
// "terminated" part ways: the session is untouched by a disconnect.
func (d *Daemon) handleAttach(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	s := d.sess
	d.mu.Unlock()
	if s == nil {
		apiError(w, http.StatusNotFound, "no session is running on this daemon")
		return
	}
	// Starting is not "not there yet": wait for the session rather than
	// make every viewer time and retry it by hand.
	select {
	case <-s.ready:
	case <-time.After(attachWait):
	case <-r.Context().Done():
		return
	}

	d.mu.Lock()
	if d.sess != s || s.st == nil || s.state != api.StateLive || s.stopping {
		state, errMsg := s.state, s.err
		d.mu.Unlock()
		switch state {
		case api.StateStarting:
			apiError(w, http.StatusConflict, "the session is still starting; try again")
		case api.StateLost:
			apiError(w, http.StatusConflict, "the session failed to start: %s", errMsg)
		default:
			apiError(w, http.StatusNotFound, "no session is running on this daemon")
		}
		return
	}
	st, sctx := s.st, s.ctx
	s.attachWG.Add(1) // under the mutex: no Add after the session stops
	d.mu.Unlock()

	hj, ok := w.(http.Hijacker)
	if !ok {
		s.attachWG.Done()
		apiError(w, http.StatusInternalServerError, "connection upgrade unsupported")
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		s.attachWG.Done()
		return
	}
	// The hijacked reader may already hold stream bytes the client sent
	// after its request; hand them along with the connection.
	if _, err := brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: remmote\r\nConnection: Upgrade\r\n\r\n"); err != nil {
		s.attachWG.Done()
		conn.Close()
		return
	}
	if err := brw.Flush(); err != nil {
		s.attachWG.Done()
		conn.Close()
		return
	}
	go func() {
		defer s.attachWG.Done()
		st.Attach(sctx, &bufferedConn{Conn: conn, r: brw.Reader})
	}()
}

// startSession starts spec as the daemon's session. With replace, a
// running session is stopped first ("terminate and start mine"); without
// it, a running one is refused with 409.
func (d *Daemon) startSession(spec api.SessionSpec, replace bool) (*api.SessionInfo, error) {
	for {
		d.mu.Lock()
		s := d.sess
		busy := s != nil && (s.state == api.StateStarting || s.state == api.StateLive)
		d.mu.Unlock()
		if !busy {
			break
		}
		if !replace {
			return nil, &api.Error{Status: http.StatusConflict,
				Message: "a session is already running; replace it to start this one"}
		}
		d.stopSession()
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if old := d.sess; old != nil {
		old.cancel() // a failed record holds nothing, but its context can go
	}
	sctx, cancel := context.WithCancel(d.ctx)
	s := &session{spec: spec, ctx: sctx, cancel: cancel,
		done: make(chan struct{}), ready: make(chan struct{}), state: api.StateStarting}
	d.sess = s
	d.ring.Add(fmt.Sprintf("starting session: %s on %s", spec.Source, spec.Display.Name))
	d.ring.Publish(api.Event{Type: "state", State: api.StateStarting, Time: time.Now()})
	go d.runSession(s)
	info := d.infoLocked(s, false)
	return &info, nil
}

// runSession builds the stream, serves it, and releases everything it
// held — created display, launched app, capture source, X connections —
// before returning.
func (d *Daemon) runSession(s *session) {
	defer close(s.done)

	// The session's log lines go to the daemon's log *and* into the ring
	// the API serves, in the same text format.
	sessionLog := slog.New(&teeHandler{base: d.log.Handler(), sink: d.ring.Add})

	// Resolve the display first: create one when asked — it dies with
	// the session — and otherwise open what is already there.
	disp, err := d.openDisplay(s, sessionLog)
	if err != nil {
		d.fail(s, err)
		return
	}
	defer disp.Close()
	disp.Activate() // every X client this session starts sees its cookie

	d.mu.Lock()
	s.display = disp.Name
	d.mu.Unlock()

	spec := s.spec
	spec.Display = api.DisplaySpec{Kind: api.KindExisting, Name: disp.Name}
	opts, err := spec.StreamOptions()
	if err != nil {
		d.fail(s, err)
		return
	}
	st, err := stream.NewContext(s.ctx, opts, sessionLog)
	if err != nil {
		d.fail(s, err)
		return
	}

	d.mu.Lock()
	s.st = st
	s.started = time.Now()
	r := st.ScreenRect()
	d.mu.Unlock()
	d.setState(s, api.StateLive)
	close(s.ready) // waiting viewers may attach now
	// What is actually on offer — the line a script or an operator reads
	// to see the session is really sharing something.
	d.ring.Add(fmt.Sprintf("sharing %s on %s: %dx%d, %s, %d fps",
		s.spec.Source, s.spec.Display.Name, r.Dx(), r.Dy(), st.CodecName(), st.FPS()))
	d.log.Info("sharing", "display", s.spec.Display.Name,
		"source", fmt.Sprintf("%dx%d", r.Dx(), r.Dy()),
		"codec", st.CodecName(), "fps", st.FPS())

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_ = st.Run(s.ctx)
	}()
	<-s.ctx.Done()
	<-runDone
	s.attachWG.Wait() // no viewer may inject input after this point
	st.Close()
}

// openDisplay opens the display the spec names, or creates one for the
// session when it asks for that — a display that dies with the session.
func (d *Daemon) openDisplay(s *session, log *slog.Logger) (*hostenv.Display, error) {
	if s.spec.Display.Kind == api.KindCreate {
		c := s.spec.Display.Create
		return hostenv.Create(s.ctx, hostenv.CreateOptions{
			Server:      c.Server,
			Size:        c.Size,
			WM:          c.WM,
			HostDisplay: c.HostDisplay,
		}, log)
	}
	return hostenv.Open(s.spec.Display.Name, log)
}

// fail records a session that never came up. The record stays — with its
// error and log — until it is replaced or the service stops: a failure
// the operator cannot see is a failure they cannot fix.
func (d *Daemon) fail(s *session, err error) {
	d.mu.Lock()
	s.err = err.Error()
	s.state = api.StateLost
	d.mu.Unlock()
	close(s.ready) // nothing will come of this session
	d.ring.Add("session failed: " + err.Error())
	d.log.Warn("session failed", "err", err)
	d.ring.Publish(api.Event{Type: "state", State: api.StateLost, Time: time.Now()})
}

func (d *Daemon) setState(s *session, state string) {
	d.mu.Lock()
	s.state = state
	d.mu.Unlock()
	d.ring.Add("session " + state)
	d.log.Info("session " + state)
	d.ring.Publish(api.Event{Type: "state", State: state, Time: time.Now()})
}

// stopSession stops the session — if any — and waits for its resources
// to be released. A record of a failed session is dropped; it holds
// nothing.
func (d *Daemon) stopSession() {
	d.mu.Lock()
	s := d.sess
	d.sess = nil
	if s != nil {
		s.stopping = true // no Attach may start from here on
	}
	d.mu.Unlock()
	if s == nil {
		return
	}
	s.cancel()
	<-s.done
}

// infoLocked reports the session as the daemon sees it. The caller holds
// the mutex.
func (d *Daemon) infoLocked(s *session, withLog bool) api.SessionInfo {
	info := api.SessionInfo{
		State:     s.state,
		Spec:      s.spec,
		Error:     s.err,
		StartedAt: s.started,
		Display:   s.display,
	}
	if s.st != nil {
		r := s.st.ScreenRect()
		info.Width, info.Height = r.Dx(), r.Dy()
		info.AppPID = s.st.AppPID()
	}
	if withLog {
		info.Log = d.ring.Tail()
	}
	return info
}

// execAllowed says whether a client may ask for this command line. Only
// the basename is matched — the path a command is found at is not part of
// what the operator allowed.
func (d *Daemon) execAllowed(cmdline string) bool {
	base := commandBase(cmdline)
	if base == "" {
		return false
	}
	for _, allowed := range d.opts.AllowExec {
		if allowed == "*" || allowed == base {
			return true
		}
	}
	return false
}

// commandBase is the basename of a command line's first word.
func commandBase(cmdline string) string {
	fields := strings.Fields(cmdline)
	if len(fields) == 0 {
		return ""
	}
	return filepath.Base(fields[0])
}

// writeJSON writes a control API response body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// apiError writes a control API error as {"error": "..."} — the shape
// api.decodeError and api.Upgrade expect.
func apiError(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}

// writeEvent writes one SSE record.
func writeEvent(w http.ResponseWriter, fl http.Flusher, ev api.Event) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
	fl.Flush()
}

// bufferedConn reads through a bufio.Reader that may already hold bytes
// taken from the underlying connection (see handleAttach).
type bufferedConn struct {
	net.Conn
	r interface {
		Read(p []byte) (int, error)
	}
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// logRing keeps a session's recent log lines and fans events out to SSE
// subscribers. A slow subscriber loses lines rather than hold up the
// session; GET /api/v1/session carries the tail.
type logRing struct {
	mu    sync.Mutex
	lines []string
	cap   int
	subs  map[chan api.Event]struct{}
}

func newLogRing(capacity int) *logRing {
	return &logRing{cap: capacity, subs: make(map[chan api.Event]struct{})}
}

// Add files one log line.
func (r *logRing) Add(line string) {
	r.publish(api.Event{Type: "log", Line: line, Time: time.Now()})
}

// Publish files one state change.
func (r *logRing) Publish(ev api.Event) {
	r.publish(ev)
}

func (r *logRing) publish(ev api.Event) {
	r.mu.Lock()
	if ev.Type == "log" {
		r.lines = append(r.lines, ev.Line)
		if len(r.lines) > r.cap {
			r.lines = r.lines[len(r.lines)-r.cap:]
		}
	}
	subs := make([]chan api.Event, 0, len(r.subs))
	for ch := range r.subs {
		subs = append(subs, ch)
	}
	r.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- ev:
		default: // dropped: the tail is on GET /api/v1/session
		}
	}
}

// Tail is a copy of the ring's current lines.
func (r *logRing) Tail() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

// Subscribe returns a fresh event channel and the func that ends it.
func (r *logRing) Subscribe() (<-chan api.Event, func()) {
	ch := make(chan api.Event, 64)
	r.mu.Lock()
	r.subs[ch] = struct{}{}
	r.mu.Unlock()
	return ch, func() {
		r.mu.Lock()
		if _, ok := r.subs[ch]; ok {
			delete(r.subs, ch)
			close(ch)
		}
		r.mu.Unlock()
	}
}

// teeHandler passes every record to the base handler and files a text
// copy in the session's log ring.
type teeHandler struct {
	base slog.Handler
	sink func(string)
}

func (h *teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.base.Enabled(ctx, l)
}

func (h *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &teeHandler{base: h.base.WithAttrs(attrs), sink: h.sink}
}

func (h *teeHandler) WithGroup(name string) slog.Handler {
	return &teeHandler{base: h.base.WithGroup(name), sink: h.sink}
}

func (h *teeHandler) Handle(ctx context.Context, rec slog.Record) error {
	var b bytes.Buffer
	// One record, one line, in the same text format the daemon logs in.
	// No logger uses WithAttrs on this handler's chain, so a plain text
	// handler reproduces the line exactly.
	_ = slog.NewTextHandler(&b, nil).Handle(ctx, rec)
	h.sink(strings.TrimRight(b.String(), "\n"))
	return h.base.Handle(ctx, rec)
}

// slogWriter routes an http.Server's error log into the daemon's log, so
// TLS handshake refusals (e.g. a client presenting no certificate) end up
// where the rest of the story is.
type slogWriter struct {
	log *slog.Logger
}

func (w *slogWriter) Write(p []byte) (int, error) {
	w.log.Info("http", "detail", strings.TrimSpace(string(p)))
	return len(p), nil
}
