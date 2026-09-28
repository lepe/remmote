package capture

import (
	"fmt"
	"image"
	"image/png"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/damage"
	"github.com/jezek/xgb/shm"
	"github.com/jezek/xgb/xproto"

	"github.com/lepe/remmote/internal/xconn"
	"github.com/lepe/remmote/internal/xwin"
)

// rescanInterval is the backstop cadence for window-set rediscovery
// (belt and braces on top of Create/Map/Reparent notifications).
const rescanInterval = 2 * time.Second

// rejectCooldown suppresses re-querying a window that was recently
// evaluated and rejected.
const rejectCooldown = 2 * time.Second

// SceneSeed seeds a Scene with the initial tracked window set.
type SceneSeed struct {
	Windows []xproto.Window // initial tracked windows
	PID     uint32          // child process id for _NET_WM_PID matching (0 = none)
	Class   string          // seed WM_CLASS res_class ("" = unknown)
}

// trackedWin is one window of the shared application.
type trackedWin struct {
	id     xproto.Window
	rect   image.Rectangle // root coordinates (interior)
	mapped bool
	dmg    damage.Damage
	dmgOK  bool
	dirty  image.Rectangle // window-local pending damage bbox
	stale  bool            // geometry refresh needed
	errs   int             // consecutive geometry errors
}

// Scene captures a set of application windows instead of the root
// window: it tracks membership (dialogs and popups join via
// WM_TRANSIENT_FOR / _NET_WM_PID / WM_CLASS), maintains per-window
// damage, and composites everything into a canvas — the bounding box of
// the tracked set in root coordinates — that streams exactly like a
// small desktop. It satisfies the server's StreamSource and the input
// Router's WindowMap.
//
// Concurrency contract (mirrors Capturer): the pump goroutine is the
// only WaitForEvent caller and issues no requests; every other method
// runs on the single capture-loop goroutine (TakePending / ApplyResize /
// Capture are its only X round-trip entry points). scratch is written
// exclusively by the capture loop.
type Scene struct {
	xc   *xconn.Conn
	log  *slog.Logger
	opts Options
	xq   *xwin.Client

	pred xwin.Predicates // PID + SeedClass (Tracked filled per call)

	mu         sync.Mutex
	windows    []*trackedWin     // stacking order, bottom → top
	canvas     image.Rectangle   // root coordinates; Min is the canvas origin
	scratch    *image.RGBA       // canvas-sized composite (capture-loop only)
	pending    []image.Rectangle // structural dirty, canvas coords
	stackDirty bool
	rejected   map[xproto.Window]time.Time

	seg       *SHMSegment
	segID     shm.Seg
	hasSHM    bool
	shmWarned bool

	changed    chan struct{}
	resized    chan struct{}
	candidates chan xproto.Window // 0 = rescan sentinel
	destroyed  chan xproto.Window
	quit       chan struct{}
	closeOnce  sync.Once
}

// NewScene wires the Scene, seeds it, and starts its pump.
func NewScene(xc *xconn.Conn, opts Options, seed SceneSeed, log *slog.Logger) (*Scene, error) {
	if opts.FPS <= 0 {
		opts.FPS = 30
	}
	if opts.FullRefresh <= 0 {
		opts.FullRefresh = 2 * time.Second
	}
	if _, err := damage.QueryVersion(xc.X, 1, 1).Reply(); err != nil {
		return nil, fmt.Errorf("scene: damage query version: %w", err)
	}
	s := &Scene{
		xc:         xc,
		log:        log,
		opts:       opts,
		xq:         xwin.NewClient(xc.X, xc.Root()),
		pred:       xwin.Predicates{PID: seed.PID, SeedClass: seed.Class},
		canvas:     image.Rect(0, 0, 1, 1),
		rejected:   make(map[xproto.Window]time.Time),
		changed:    make(chan struct{}, 1),
		resized:    make(chan struct{}, 1),
		candidates: make(chan xproto.Window, 64),
		destroyed:  make(chan xproto.Window, 64),
		quit:       make(chan struct{}),
	}
	if err := s.xq.SelectSubstructureNotifyRoot(); err != nil {
		return nil, fmt.Errorf("scene: root substructure notify: %w", err)
	}
	s.scratch = image.NewRGBA(image.Rect(0, 0, 1, 1))
	s.allocBuffers()
	for _, w := range seed.Windows {
		s.track(w)
	}
	// Adopt siblings immediately so the first client's kick keyframe
	// already shows the full application, not just the seed window.
	s.rescan()
	s.recomputeCanvas()
	go s.pump()
	go s.rescanTick()
	return s, nil
}

// FPS is the configured frame cap.
func (s *Scene) FPS() int { return s.opts.FPS }

// FullRefresh is the configured keyframe interval.
func (s *Scene) FullRefresh() time.Duration { return s.opts.FullRefresh }

// HasDamage is always true in window mode: membership evaluation must
// run even without pixel damage.
func (s *Scene) HasDamage() bool { return true }

// ScreenRect is the canvas size in canvas coordinates.
func (s *Scene) ScreenRect() image.Rectangle {
	s.mu.Lock()
	defer s.mu.Unlock()
	return image.Rect(0, 0, s.canvas.Dx(), s.canvas.Dy())
}

// Changed is signaled (coalesced) when damage or structural change
// arrives.
func (s *Scene) Changed() <-chan struct{} { return s.changed }

// Resized is signaled when the canvas geometry changed and
// ApplyResize must run.
func (s *Scene) Resized() <-chan struct{} { return s.resized }

// ApplyResize re-evaluates membership, refreshes geometry, and rebuilds
// the canvas if needed. Capture-loop only.
func (s *Scene) ApplyResize() error {
	s.evalCandidates()
	s.refreshGeometry()
	s.recomputeCanvas()
	return nil
}

// WindowAt returns the topmost tracked window at root coordinates.
func (s *Scene) WindowAt(x, y int) (xproto.Window, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.windows) - 1; i >= 0; i-- {
		w := s.windows[i]
		if w.mapped && image.Pt(x, y).In(w.rect) {
			return w.id, true
		}
	}
	return 0, false
}

// TopWindow returns the topmost tracked window.
func (s *Scene) TopWindow() (xproto.Window, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.windows) - 1; i >= 0; i-- {
		if s.windows[i].mapped {
			return s.windows[i].id, true
		}
	}
	return 0, false
}

// CanvasOrigin translates canvas coordinates to root coordinates.
func (s *Scene) CanvasOrigin() image.Point {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.canvas.Min
}

// MarkStackDirty hints that stacking changed; the next evaluation
// resorts.
func (s *Scene) MarkStackDirty() {
	s.mu.Lock()
	s.stackDirty = true
	s.mu.Unlock()
	s.signal(s.changed)
}

// DumpFrame captures the whole canvas once and writes it as PNG.
func (s *Scene) DumpFrame(path string) error {
	img, err := s.Capture(s.ScreenRect())
	if err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

// Close stops the pump and releases resources (not the X connection).
func (s *Scene) Close() error {
	s.closeOnce.Do(func() { close(s.quit) })
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseSegment()
	for _, w := range s.windows {
		if w.dmgOK {
			damage.Destroy(s.xc.X, w.dmg)
		}
	}
	s.windows = nil
	return nil
}

// TakePending evaluates candidates, refreshes stale geometry, recomputes
// the canvas, and returns the pending dirty rects in canvas
// coordinates. Capture-loop only.
func (s *Scene) TakePending() []image.Rectangle {
	s.ApplyResize()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []image.Rectangle
	canvasLocal := image.Rect(0, 0, s.canvas.Dx(), s.canvas.Dy())
	for _, w := range s.windows {
		if w.dirty.Empty() {
			continue
		}
		r := w.dirty.Add(w.rect.Min).Sub(s.canvas.Min).Intersect(canvasLocal)
		w.dirty = image.Rectangle{}
		if !r.Empty() {
			out = append(out, r)
		}
	}
	out = append(out, s.pending...)
	s.pending = nil
	return out
}

// Capture composites every mapped window overlapping r (canvas
// coordinates) into the scratch buffer and returns the subimage. The
// result is valid until the next Capture. Capture-loop only.
func (s *Scene) Capture(r image.Rectangle) (image.Image, error) {
	s.mu.Lock()
	canvasLocal := image.Rect(0, 0, s.canvas.Dx(), s.canvas.Dy())
	if len(s.windows) == 0 {
		s.mu.Unlock()
		return s.scratch, nil // nothing tracked: retain last frame
	}
	type job struct {
		w       *trackedWin
		overlap image.Rectangle // canvas coords
	}
	var jobs []job
	r = r.Intersect(canvasLocal)
	for _, w := range s.windows {
		if !w.mapped {
			continue
		}
		overlap := w.rect.Sub(s.canvas.Min).Intersect(r)
		if overlap.Empty() {
			continue
		}
		jobs = append(jobs, job{w: w, overlap: overlap})
	}
	s.mu.Unlock()

	if r.Empty() {
		return nil, fmt.Errorf("scene: empty capture rect")
	}
	for _, j := range jobs {
		// Clear accumulated damage *before* reading pixels (same
		// race-free ordering as the root capturer).
		if j.w.dmgOK {
			damage.Subtract(s.xc.X, j.w.dmg, 0, 0)
		}
		local := j.overlap.Add(s.canvas.Min).Sub(j.w.rect.Min)
		if err := s.captureWindowRegion(j.w.id, local, j.overlap); err != nil {
			s.log.Debug("window capture failed", "window", uint32(j.w.id), "err", err)
		}
	}
	return s.scratch.SubImage(r), nil
}

// captureWindowRegion grabs local (window-relative) rect from the window
// drawable into scratch at overlap (canvas coordinates). Capture-loop
// only; runs without s.mu (scratch has a single writer).
func (s *Scene) captureWindowRegion(id xproto.Window, local, overlap image.Rectangle) error {
	w, h := local.Dx(), local.Dy()
	dst := s.scratch.SubImage(overlap).(*image.RGBA)

	if s.hasSHM {
		cookie := shm.GetImage(s.xc.X, xproto.Drawable(id),
			int16(local.Min.X), int16(local.Min.Y), uint16(w), uint16(h),
			0xFFFFFFFF, xproto.ImageFormatZPixmap, s.segID, 0)
		reply, err := cookie.Reply()
		if err == nil && (reply.Depth == 24 || reply.Depth == 32) {
			return BGRAToRGBA(dst, s.seg.Mem(), w, h, s.xc.LSBFirst())
		}
		if err != nil && !s.shmWarned {
			s.shmWarned = true
			s.log.Warn("SHM GetImage failed; using core GetImage", "err", err)
		}
	}

	const maxBandBytes = 4 << 20
	bandH := maxBandBytes / (w * 4)
	if bandH < 1 {
		bandH = 1
	}
	for y0 := 0; y0 < h; y0 += bandH {
		bh := min(bandH, h-y0)
		reply, err := xproto.GetImage(s.xc.X, xproto.ImageFormatZPixmap,
			xproto.Drawable(id),
			int16(local.Min.X), int16(local.Min.Y+y0), uint16(w), uint16(bh), 0xFFFFFFFF).Reply()
		if err != nil {
			return err
		}
		band := s.scratch.SubImage(image.Rect(overlap.Min.X, overlap.Min.Y+y0,
			overlap.Min.X+w, overlap.Min.Y+y0+bh)).(*image.RGBA)
		if err := BGRAToRGBA(band, reply.Data, w, bh, s.xc.LSBFirst()); err != nil {
			return err
		}
	}
	return nil
}

// ---- capture-loop internals (sole mutators of the window set) ----

// evalCandidates drains the candidate queue: the zero id triggers a full
// rescan, anything else is evaluated against the membership predicates.
func (s *Scene) evalCandidates() {
	rescan := false
loop:
	for {
		select {
		case w := <-s.candidates:
			if w == 0 {
				rescan = true
			} else {
				s.consider(w)
			}
		default:
			break loop
		}
	}
	s.drainDestroyed()
	if rescan || s.takeStackDirty() {
		s.rescan()
	}
}

// takeStackDirty reports and clears the rescan hint.
func (s *Scene) takeStackDirty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.stackDirty
	s.stackDirty = false
	return v
}

func (s *Scene) drainDestroyed() {
	removed := false
loop:
	for {
		select {
		case id := <-s.destroyed:
			s.removeWindow(id)
			removed = true
		default:
			break loop
		}
	}
	if removed {
		s.dirtyAll()
		s.recomputeCanvas()
	}
}

// dirtyAll schedules a full-canvas refresh — used when a window vanishes
// so the windows beneath it are recaptured.
func (s *Scene) dirtyAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, image.Rect(0, 0, s.canvas.Dx(), s.canvas.Dy()))
}

// consider evaluates one window against the predicates and tracks it on
// acceptance.
func (s *Scene) consider(w xproto.Window) {
	if w == 0 || w == s.xc.Root() {
		return
	}
	if s.isTracked(w) {
		return
	}
	if _, rejected := s.rejectedRecent(w); rejected {
		return
	}
	// In-window children (menus inside the window tree) are captured for
	// free by GetImage on their ancestor — never track them.
	if s.hasTrackedAncestor(w) {
		return
	}
	transient, _ := s.xq.PropUint32(w, "WM_TRANSIENT_FOR")
	pid, _ := s.xq.PropUint32(w, "_NET_WM_PID")
	_, class, _ := s.xq.WMClass(w)
	if !s.predicates().Accept(xproto.Window(transient), pid, class) {
		s.mu.Lock()
		s.rejected[w] = time.Now()
		s.mu.Unlock()
		return
	}
	s.track(w)
}

func (s *Scene) predicates() xwin.Predicates {
	p := s.pred
	p.Tracked = s.isTracked
	return p
}

func (s *Scene) isTracked(w xproto.Window) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.windows {
		if t.id == w {
			return true
		}
	}
	return false
}

func (s *Scene) rejectedRecent(w xproto.Window) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.rejected[w]
	if ok && time.Since(t) < rejectCooldown {
		return t, true
	}
	delete(s.rejected, w)
	return time.Time{}, false
}

// hasTrackedAncestor walks up (≤4 parents) looking for a tracked window.
func (s *Scene) hasTrackedAncestor(w xproto.Window) bool {
	for i := 0; i < 4; i++ {
		p, err := s.xq.Parent(w)
		if err != nil || p == 0 || p == s.xc.Root() {
			return false
		}
		if s.isTracked(p) {
			return true
		}
		w = p
	}
	return false
}

// track starts tracking a window: structure events, damage, geometry.
func (s *Scene) track(w xproto.Window) {
	t := &trackedWin{id: w}
	if err := s.xq.SelectStructureNotify(w); err != nil {
		s.log.Debug("structure notify failed", "window", uint32(w), "err", err)
	}
	if dmgID, err := s.xc.X.NewId(); err == nil {
		if err := damage.CreateChecked(s.xc.X, damage.Damage(dmgID),
			xproto.Drawable(w), damage.ReportLevelBoundingBox).Check(); err == nil {
			t.dmg, t.dmgOK = damage.Damage(dmgID), true
		} else {
			s.log.Warn("damage create failed; window tracked without it", "window", uint32(w), "err", err)
		}
	}
	if rect, err := s.xq.RootRect(w); err == nil {
		t.rect = rect
	}
	t.mapped = s.xq.IsViewable(w)

	s.mu.Lock()
	s.windows = append(s.windows, t)
	rect := t.rect
	s.mu.Unlock()
	s.log.Info("tracking window", "window", uint32(w), "rect", rect.String(), "mapped", t.mapped)
	s.recomputeCanvas()
	s.mu.Lock()
	s.addPendingCanvas(rect.Sub(s.canvas.Min))
	s.mu.Unlock()
	s.signal(s.changed)
}

// removeWindow drops a window from the tracked set.
func (s *Scene) removeWindow(id xproto.Window) {
	s.mu.Lock()
	for i, t := range s.windows {
		if t.id == id {
			if t.dmgOK {
				damage.Destroy(s.xc.X, t.dmg)
			}
			s.windows = append(s.windows[:i], s.windows[i+1:]...)
			s.log.Info("window gone", "window", uint32(id))
			break
		}
	}
	s.mu.Unlock()
}

// refreshGeometry updates root-coordinate rects of stale windows and
// drops windows that error twice in a row (destroyed without notice).
func (s *Scene) refreshGeometry() {
	var stale []*trackedWin
	s.mu.Lock()
	for _, t := range s.windows {
		if t.stale {
			stale = append(stale, t)
		}
	}
	s.mu.Unlock()
	for _, t := range stale {
		rect, err := s.xq.RootRect(t.id)
		s.mu.Lock()
		if err != nil {
			t.errs++
		} else {
			t.errs = 0
			t.stale = false
			t.rect = rect
		}
		dead := t.errs >= 2
		id := t.id
		s.mu.Unlock()
		if dead {
			s.removeWindow(id)
		}
	}
	s.recomputeCanvas()
}

// rescan re-sorts the stacking order, adopts matching windows the event
// stream missed, and is the backstop for membership churn.
func (s *Scene) rescan() {
	// Sort tracked windows by their root-level ancestor's stacking order.
	stack := s.xq.Stack()
	order := make(map[xproto.Window]int, len(stack))
	for i, w := range stack {
		order[w] = i
	}
	type entry struct {
		t  *trackedWin
		ix int
	}
	s.mu.Lock()
	entries := make([]entry, 0, len(s.windows))
	for _, t := range s.windows {
		entries = append(entries, entry{t, 1 << 30})
	}
	s.mu.Unlock()
	// Ancestor walk per window (round trips are fine on the capture loop).
	for i := range entries {
		entries[i].ix = s.rootAncestorOrder(entries[i].t.id, order)
	}
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j].ix < entries[j-1].ix; j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
	s.mu.Lock()
	for i, e := range entries {
		s.windows[i] = e.t
	}
	s.mu.Unlock()

	// Adopt matching client windows the events missed.
	for _, w := range s.xq.FindClientWindows() {
		if !s.isTracked(w) {
			s.consider(w)
		}
	}
}

// rootAncestorOrder finds the stacking index of the window's root-level
// ancestor.
func (s *Scene) rootAncestorOrder(w xproto.Window, order map[xproto.Window]int) int {
	for i := 0; i < 5; i++ {
		if ix, ok := order[w]; ok {
			return ix
		}
		p, err := s.xq.Parent(w)
		if err != nil || p == 0 {
			return 1 << 30
		}
		w = p
	}
	return 1 << 30
}

// recomputeCanvas recalculates the canvas bbox (quantized outward to a
// 32 px grid) and reallocates buffers when it changed. The canvas never
// shrinks to nothing: with no mapped windows the last canvas is kept so
// the retained scratch (last frame) keeps streaming.
func (s *Scene) recomputeCanvas() {
	rootW, rootH := s.xc.ScreenSize()
	root := image.Rect(0, 0, int(rootW), int(rootH))
	bbox := image.Rectangle{}
	s.mu.Lock()
	for _, t := range s.windows {
		if t.mapped {
			bbox = bbox.Union(t.rect)
		}
	}
	s.mu.Unlock()
	if bbox.Empty() {
		return
	}
	bbox = quantizeOutward(bbox.Intersect(root))

	s.mu.Lock()
	if bbox == s.canvas {
		s.mu.Unlock()
		return
	}
	s.canvas = bbox
	s.mu.Unlock()

	s.scratch = image.NewRGBA(image.Rect(0, 0, bbox.Dx(), bbox.Dy()))
	s.allocBuffers()
	s.mu.Lock()
	s.pending = append(s.pending, image.Rect(0, 0, bbox.Dx(), bbox.Dy()))
	s.mu.Unlock()
	s.signal(s.resized)
	s.signal(s.changed)
}

// addPendingCanvas records a dirty rect in canvas coordinates; caller
// holds s.mu.
func (s *Scene) addPendingCanvas(r image.Rectangle) {
	canvasLocal := image.Rect(0, 0, s.canvas.Dx(), s.canvas.Dy())
	if r = r.Intersect(canvasLocal); !r.Empty() {
		s.pending = append(s.pending, r)
	}
}

func (s *Scene) allocBuffers() {
	s.releaseSegment()
	if !s.xc.Ext.SHM {
		return
	}
	size := s.scratch.Rect.Dx() * s.scratch.Rect.Dy() * 4
	seg, err := NewSHMSegment(size)
	if err != nil {
		s.log.Warn("SHM segment unavailable; using core GetImage fallback", "err", err)
		return
	}
	segID, err := s.xc.X.NewId()
	if err != nil {
		seg.Close()
		return
	}
	if err := shm.AttachChecked(s.xc.X, shm.Seg(segID), uint32(seg.ID()), false).Check(); err != nil {
		seg.Close()
		s.log.Warn("X rejected SHM attach; using core GetImage fallback", "err", err)
		return
	}
	s.seg, s.segID, s.hasSHM = seg, shm.Seg(segID), true
}

func (s *Scene) releaseSegment() {
	if s.segID != 0 {
		_ = shm.DetachChecked(s.xc.X, s.segID).Check()
		s.segID = 0
	}
	if s.seg != nil {
		_ = s.seg.Close()
		s.seg = nil
	}
	s.hasSHM = false
}

// rescanTick is the 2 s backstop: it nudges the capture loop through the
// candidates sentinel.
func (s *Scene) rescanTick() {
	t := time.NewTicker(rescanInterval)
	defer t.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-t.C:
			select {
			case s.candidates <- 0:
			default:
			}
			s.signal(s.changed)
		}
	}
}

// pump is the only WaitForEvent caller; it issues no requests.
func (s *Scene) pump() {
	for {
		select {
		case <-s.quit:
			return
		default:
		}
		ev, err := s.xc.X.WaitForEvent()
		if err != nil {
			return
		}
		if ev == nil && err == nil {
			return
		}
		s.HandleEvent(ev)
	}
}

// HandleEvent dispatches one X event (value types — see the xgb notes).
func (s *Scene) HandleEvent(ev xgb.Event) {
	switch e := ev.(type) {
	case damage.NotifyEvent:
		s.mu.Lock()
		for _, t := range s.windows {
			if xproto.Drawable(t.id) == e.Drawable {
				t.dirty = unionRect(t.dirty, xprotoRect(e.Area))
				break
			}
		}
		s.mu.Unlock()
		s.signal(s.changed)

	case xproto.CreateNotifyEvent:
		s.pushCandidate(e.Window)

	case xproto.ReparentNotifyEvent:
		if e.Parent == s.xc.Root() {
			s.pushCandidate(e.Window)
		}

	case xproto.MapNotifyEvent:
		if s.markMapped(e.Window, true) {
			s.signal(s.changed)
			s.signal(s.resized)
		} else {
			s.pushCandidate(e.Window)
		}

	case xproto.UnmapNotifyEvent:
		if s.markMapped(e.Window, false) {
			s.dirtyAll() // windows beneath must be recaptured
			s.signal(s.changed)
			s.signal(s.resized)
		}

	case xproto.DestroyNotifyEvent:
		if s.isTracked(e.Window) {
			select {
			case s.destroyed <- e.Window:
			default:
			}
		}

	case xproto.ConfigureNotifyEvent:
		s.mu.Lock()
		for _, t := range s.windows {
			if t.id == e.Window {
				t.stale = true
				t.dirty = unionRect(t.dirty, image.Rect(0, 0, 65535, 65535))
				break
			}
		}
		s.mu.Unlock()
		s.signal(s.changed)
		s.signal(s.resized)
	}
}

// markMapped sets the mapped flag for a tracked window; false when the
// window is unknown (→ candidate).
func (s *Scene) markMapped(w xproto.Window, mapped bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.windows {
		if t.id == w {
			if t.mapped != mapped {
				t.mapped = mapped
				t.stale = true
				t.dirty = unionRect(t.dirty, image.Rect(0, 0, 65535, 65535))
			}
			return true
		}
	}
	return false
}

func (s *Scene) pushCandidate(w xproto.Window) {
	if w == 0 {
		return
	}
	select {
	case s.candidates <- w:
	default: // full: the 2 s rescan backstop catches it
	}
}

func (s *Scene) signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// quantizeOutward expands a rect to the 32 px grid so small window
// moves do not churn the canvas size.
func quantizeOutward(r image.Rectangle) image.Rectangle {
	const q = 32
	return image.Rect(floorQ(r.Min.X), floorQ(r.Min.Y), ceilQ(r.Max.X), ceilQ(r.Max.Y))
}

func floorQ(v int) int {
	d := v / 32
	if v%32 != 0 && v < 0 {
		d--
	}
	return d * 32
}

func ceilQ(v int) int {
	if v%32 == 0 {
		return v
	}
	return floorQ(v) + 32
}
