// Package hub is the client's connection manager: the speed dial of
// saved connections, the editor for what to share, and the panel for
// what is being shared right now.
//
// The session lives on the host; the hub is a view onto it. Closing the
// viewer window detaches and nothing else — the panel keeps saying what
// is running, and opening the viewer again picks up where it left off.
// Stopping a session is a deliberate act, and it stops the daemon too.
package hub

import (
	"context"
	"crypto/tls"
	"fmt"
	"image"
	"log/slog"
	"strconv"
	"strings"

	"github.com/lepe/remmote/internal/api"
	"github.com/lepe/remmote/internal/auth"
	"github.com/lepe/remmote/internal/client"
	"github.com/lepe/remmote/internal/profile"
	"github.com/lepe/remmote/internal/tlsutil"
	"github.com/lepe/remmote/internal/ui"
)

// Options configures the hub.
type Options struct {
	Display string // where the hub's own window opens
	Store   *profile.Store
	Log     *slog.Logger
}

// The screens of the hub.
const (
	screenList = iota
	screenEditor
	screenSession
)

// hub is the state behind the window.
type hub struct {
	opts Options
	log  *slog.Logger
	win  *ui.Window

	screen   int
	profiles []profile.Profile
	selected int

	draft     draft
	sourceNow string // the source the form was built for
	form      *ui.Form
	formErr   string

	conn    *api.Client
	info    api.SessionInfo
	stopEvt func()
	viewer  context.CancelFunc // closes the viewer window (detach)
	viewing bool
	logs    []string
	status  string
	modal   string // a question awaiting y / Esc
	confirm func(ui.KeyEvent) bool
	err     string
	width   int
}

// draft is what the editor edits: every field is text here, and becomes
// a profile on save. A form edits strings; pretending otherwise is how
// editors end up with half-numbers in them.
type draft struct {
	name, server, identity string
	source                 string
	appCmd                 string
	windowID               string
	maximize               bool
	displayKind            string
	displayName            string
	createServer           string
	createSize             string
	createWM               string
	createHost             string
	codec                  string
	quality                string
	fps                    string
	downscale              string
	clipboard              bool
	resizeDesktop          bool
	upscale                string
	fastScale              bool
}

// Run opens the hub and returns when its window closes.
func Run(ctx context.Context, opts Options) error {
	if opts.Log == nil {
		opts.Log = slog.New(slog.NewTextHandler(nil, nil))
	}
	win, err := ui.Open(opts.Display, "remmote", 760, 540, opts.Log)
	if err != nil {
		return err
	}
	defer win.Close()

	h := &hub{opts: opts, log: opts.Log, win: win, screen: screenList}
	h.reload()
	h.status = "Enter connects · e edits · n is a new connection · Del deletes"
	// Closing the window is the way out — and so is Ctrl+C.
	defer func() {
		if h.viewer != nil {
			h.viewer()
		}
		if h.stopEvt != nil {
			h.stopEvt()
		}
	}()
	go func() {
		<-ctx.Done()
		win.Close()
	}()
	win.Draw(h.Paint)
	return win.Pump(h)
}

// reload reads the saved connections.
func (h *hub) reload() {
	list, err := h.opts.Store.List()
	if err != nil {
		h.err = err.Error()
		return
	}
	h.profiles = list
	if h.selected >= len(h.profiles) {
		h.selected = max(len(h.profiles)-1, 0)
	}
}

// Paint draws whatever screen is up.
func (h *hub) Paint(p *ui.Painter) {
	w, _ := p.Size()
	h.width = w
	switch h.screen {
	case screenList:
		h.paintList(p)
	case screenEditor:
		h.paintEditor(p)
	case screenSession:
		h.paintSession(p)
	}
	if h.modal != "" {
		h.paintModal(p)
	}
	if h.err != "" {
		h.paintError(p)
	}
}

// title draws a screen's heading, with an optional way back.
func (h *hub) title(p *ui.Painter, text, back string) {
	p.Fill(image.Rect(0, 0, h.width, 40), ui.Panel)
	if back != "" {
		p.Text(16, 26, back, ui.Dim)
	}
	p.Text(60, 26, text, ui.Fg)
	p.Fill(image.Rect(0, 40, h.width, 41), ui.Border)
}

// footer draws the hint line at the bottom.
func (h *hub) footer(p *ui.Painter, text string) {
	_, ph := p.Size()
	p.Fill(image.Rect(0, ph-28, h.width, ph), ui.Panel)
	p.Text(16, ph-9, ui.Truncate(text, h.width-32), ui.Dim)
}

// --- the speed dial ---

func (h *hub) paintList(p *ui.Painter) {
	h.title(p, "Connections", "")
	items := make([]string, 0, len(h.profiles))
	subs := make([]string, 0, len(h.profiles))
	for _, pr := range h.profiles {
		items = append(items, pr.Name)
		subs = append(subs, fmt.Sprintf("%s · %s", pr.Server, sourceName(pr.Spec.Source)))
	}
	list := &ui.List{X: 16, Y: 96, W: h.width - 32, H: 340,
		Items: items, Sub: subs, Selected: &h.selected}
	list.Paint(p, true)

	for _, b := range []*ui.Button{
		{X: 16, Y: 56, W: 120, Label: "Connect", Primary: true},
		{X: 146, Y: 56, W: 90, Label: "New"},
		{X: 246, Y: 56, W: 90, Label: "Edit"},
		{X: 346, Y: 56, W: 90, Label: "Delete"},
	} {
		b.Paint(p, false)
	}
	if len(h.profiles) == 0 {
		p.Text(24, 140, "Nothing saved yet.", ui.Dim)
		p.Text(24, 162, "New makes a connection; it is saved for next time.", ui.Dim)
	}
	h.footer(p, h.status)
}

// --- the editor ---

// buildForm assembles the editor's widgets for the source in use: the
// fields that depend on the source come and go with it.
func (h *hub) buildForm() {
	d := &h.draft
	f := &ui.Form{}
	f.Add(
		&ui.Label{X: 16, Y: 58, Text: "Name", Dim: true},
		&ui.Field{X: 140, Y: 54, W: 340, Value: &d.name, Hint: "Workstation"},
		&ui.Label{X: 16, Y: 82, Text: "Server", Dim: true},
		&ui.Field{X: 140, Y: 78, W: 340, Value: &d.server, Hint: "host:7677"},
		&ui.Label{X: 16, Y: 106, Text: "Device", Dim: true},
		&ui.Field{X: 140, Y: 102, W: 340, Value: &d.identity, Hint: "paired device name, or empty"})
	y := 138

	f.Add(&ui.Label{X: 16, Y: y, Text: "Share", Dim: true},
		&ui.Radio{X: 140, Y: y - 4, Options: []string{"desktop", "app", "window"}, Value: &d.source})
	y += 30

	switch d.source {
	case "app":
		f.Add(&ui.Label{X: 16, Y: y, Text: "Command", Dim: true},
			&ui.Field{X: 140, Y: y - 4, W: 480, Value: &d.appCmd, Hint: "xcalc"})
		y += 28
	case "window":
		f.Add(&ui.Label{X: 16, Y: y, Text: "Window id", Dim: true},
			&ui.Field{X: 140, Y: y - 4, W: 340, Value: &d.windowID, Hint: "0x3400007 (from xwininfo)"})
		y += 28
	}
	if d.source != "desktop" {
		f.Add(&ui.Check{X: 140, Y: y, Label: "maximize on the host screen", Value: &d.maximize})
		y += 28
	}

	f.Add(&ui.Label{X: 16, Y: y, Text: "Display", Dim: true},
		&ui.Select{X: 140, Y: y - 4, W: 200,
			Options: []string{"existing", "create"}, Value: &d.displayKind})
	y += 28
	if d.displayKind == "create" {
		f.Add(&ui.Label{X: 16, Y: y, Text: "Make", Dim: true},
			&ui.Select{X: 140, Y: y - 4, W: 160,
				Options: []string{"xvfb", "xephyr"}, Value: &d.createServer},
			&ui.Label{X: 320, Y: y, Text: "Size", Dim: true},
			&ui.Field{X: 370, Y: y - 4, W: 140, Value: &d.createSize, Hint: "1280x800"})
		y += 28
		f.Add(&ui.Label{X: 16, Y: y, Text: "Window manager", Dim: true},
			&ui.Field{X: 140, Y: y - 4, W: 200, Value: &d.createWM, Hint: "openbox, or none"})
		if d.createServer == "xephyr" {
			f.Add(&ui.Label{X: 360, Y: y, Text: "Host display", Dim: true},
				&ui.Field{X: 480, Y: y - 4, W: 100, Value: &d.createHost, Hint: ":0"})
		}
		y += 28
	} else {
		f.Add(&ui.Label{X: 16, Y: y, Text: "Name", Dim: true},
			&ui.Field{X: 140, Y: y - 4, W: 200, Value: &d.displayName, Hint: ":0"})
		y += 28
	}

	f.Add(&ui.Label{X: 16, Y: y, Text: "Codec", Dim: true},
		&ui.Select{X: 140, Y: y - 4, W: 130,
			Options: []string{"hybrid", "zraw", "jpeg", "webp"}, Value: &d.codec},
		&ui.Label{X: 290, Y: y, Text: "Quality", Dim: true},
		&ui.Field{X: 360, Y: y - 4, W: 60, Value: &d.quality, Hint: "75"},
		&ui.Label{X: 440, Y: y, Text: "FPS", Dim: true},
		&ui.Field{X: 480, Y: y - 4, W: 60, Value: &d.fps, Hint: "60"})
	y += 28
	f.Add(&ui.Label{X: 16, Y: y, Text: "Downscale", Dim: true},
		&ui.Select{X: 140, Y: y - 4, W: 90, Options: []string{"1", "2", "4"}, Value: &d.downscale},
		&ui.Check{X: 260, Y: y, Label: "clipboard", Value: &d.clipboard})
	y += 28
	f.Add(&ui.Check{X: 140, Y: y, Label: "let the viewer resize the host screen", Value: &d.resizeDesktop})
	y += 32
	f.Add(&ui.Label{X: 16, Y: y, Text: "Upscale", Dim: true},
		&ui.Select{X: 140, Y: y - 4, W: 90, Options: []string{"1", "2", "4"}, Value: &d.upscale},
		&ui.Check{X: 260, Y: y, Label: "fast scaling", Value: &d.fastScale})

	h.form = f
	h.sourceNow = d.source
	f.FocusFirst()
}

func (h *hub) paintEditor(p *ui.Painter) {
	h.title(p, "Connection", "< back")
	if h.form == nil {
		h.buildForm()
	}
	h.form.Paint(p)
	_, ph := p.Size()
	y := ph - 76
	for _, b := range []*ui.Button{
		{X: 16, Y: y, W: 120, Label: "Save", Primary: true},
		{X: 146, Y: y, W: 170, Label: "Save & connect"},
		{X: 326, Y: y, W: 100, Label: "Cancel"},
	} {
		b.Paint(p, false)
	}
	if h.formErr != "" {
		p.Text(446, y+15, ui.Truncate(h.formErr, h.width-466), ui.Danger)
	}
	h.footer(p, "Tab moves between fields · the daemon decides what it allows")
}

// --- the session panel ---

func (h *hub) paintSession(p *ui.Painter) {
	h.title(p, "Session", "< back")
	info := h.info
	colour := ui.Dim
	switch info.State {
	case api.StateLive:
		colour = ui.Ok
	case api.StateStarting:
		colour = ui.Accent
	case api.StateLost:
		colour = ui.Danger
	}
	p.Fill(image.Rect(16, 58, 26, 68), colour)
	p.Text(34, 68, info.State, ui.Fg)
	if info.Display != "" {
		p.Text(140, 68, fmt.Sprintf("%s %dx%d", info.Display, info.Width, info.Height), ui.Dim)
	}
	p.Text(16, 92, ui.Truncate(fmt.Sprintf("%s · %s",
		sourceName(info.Spec.Source), info.Spec.Stream.Codec), h.width-32), ui.Dim)
	if info.Spec.Source == api.SourceApp && info.Spec.App != nil {
		p.Text(16, 114, ui.Truncate("app: "+info.Spec.App.Command, h.width-32), ui.Dim)
	}
	if info.Error != "" {
		p.Text(16, 136, ui.Truncate(info.Error, h.width-32), ui.Danger)
	}

	_, ph := p.Size()
	y := ph - 76
	for _, b := range []*ui.Button{
		{X: 16, Y: y, W: 160, Label: "Open viewer", Primary: !h.viewing, Disabled: h.viewing},
		{X: 186, Y: y, W: 190, Label: "Terminate session", Danger: true},
	} {
		b.Paint(p, false)
	}
	if h.viewing {
		p.Text(390, y+15, "closing the viewer window detaches", ui.Dim)
	}

	p.Text(16, 158, "log", ui.Dim)
	box := image.Rect(16, 168, h.width-16, ph-88)
	p.Fill(box, ui.Input)
	p.Rect(box, ui.Border)
	rows := max((box.Dy()-16)/18, 1)
	start := max(len(h.logs)-rows, 0)
	for i, line := range h.logs[start:] {
		p.Text(24, 184+i*18, ui.Truncate(line, h.width-56), ui.Dim)
	}
	h.footer(p, h.status)
}

// paintModal draws a question over the screen.
func (h *hub) paintModal(p *ui.Painter) {
	_, ph := p.Size()
	r := image.Rect(80, ph/2-70, h.width-80, ph/2+70)
	p.Fill(r, ui.PanelHi)
	p.Rect(r, ui.Border)
	p.Text(r.Min.X+20, r.Min.Y+30, "Are you sure?", ui.Fg)
	p.Text(r.Min.X+20, r.Min.Y+58, ui.Truncate(h.modal, r.Dx()-40), ui.Dim)
	p.Text(r.Min.X+20, r.Min.Y+86, "y confirms · Esc keeps things as they are", ui.Dim)
}

// paintError draws a message over the screen until it is dismissed.
func (h *hub) paintError(p *ui.Painter) {
	_, ph := p.Size()
	r := image.Rect(80, ph/2-70, h.width-80, ph/2+70)
	p.Fill(r, ui.PanelHi)
	p.Rect(r, ui.Danger)
	p.Text(r.Min.X+20, r.Min.Y+30, "Something went wrong", ui.Fg)
	p.Text(r.Min.X+20, r.Min.Y+58, ui.Truncate(h.err, r.Dx()-40), ui.Dim)
	p.Text(r.Min.X+20, r.Min.Y+86, "Enter or Esc closes this", ui.Dim)
}

// --- events ---

// OnKey handles a key press on whichever screen is up.
func (h *hub) OnKey(ev ui.KeyEvent) {
	switch {
	case h.err != "":
		if ev.Enter || ev.Escape {
			h.err = ""
		}
	case h.confirm != nil:
		if h.confirm(ev) {
			h.confirm = nil
			h.modal = ""
		}
	default:
		switch h.screen {
		case screenList:
			h.keyList(ev)
		case screenEditor:
			h.keyEditor(ev)
		case screenSession:
			h.keySession(ev)
		}
	}
}

func (h *hub) keyList(ev ui.KeyEvent) {
	switch {
	case ev.Escape:
		h.win.Close()
	case ev.Enter:
		h.connect()
	case ev.Char == 'n':
		h.edit(profile.Profile{Server: "127.0.0.1:7677"})
	case (ev.Char == 'e') && h.selected < len(h.profiles):
		h.edit(h.profiles[h.selected])
	case (ev.Delete || ev.Bksp || ev.Char == 'd') && h.selected < len(h.profiles):
		h.confirmDelete()
	case ev.Up:
		h.selected = max(h.selected-1, 0)
	case ev.Down:
		h.selected = min(h.selected+1, max(len(h.profiles)-1, 0))
	}
}

func (h *hub) keyEditor(ev ui.KeyEvent) {
	if ev.Escape {
		h.screen = screenList
		h.form = nil
		return
	}
	if h.form != nil && h.form.Key(ev) && h.draft.source != h.sourceNow {
		h.buildForm()
	}
}

func (h *hub) keySession(ev ui.KeyEvent) {
	switch {
	case ev.Escape:
		h.screen = screenList
	case ev.Enter:
		h.openViewer()
	}
}

// OnMouse routes a pointer event to the screen.
func (h *hub) OnMouse(ev ui.MouseEvent) {
	if h.err != "" || h.confirm != nil {
		return
	}
	switch h.screen {
	case screenList:
		h.mouseList(ev)
	case screenEditor:
		h.mouseEditor(ev)
	case screenSession:
		h.mouseSession(ev)
	}
}

// hit reports whether a pointer event landed on a row of a button.
func hit(ev ui.MouseEvent, x, w, y int) bool {
	return ev.Kind == ui.MousePress && ev.X >= x && ev.X < x+w && ev.Y >= y && ev.Y < y+ui.RowHeight
}

func (h *hub) mouseList(ev ui.MouseEvent) {
	if ev.Kind == ui.MousePress {
		row := (ev.Y - 97) / ui.RowHeight
		if ev.Y >= 96 && ev.Y < 436 && row >= 0 && row < len(h.profiles) {
			h.selected = row
			return
		}
	}
	switch {
	case hit(ev, 16, 120, 56):
		h.connect()
	case hit(ev, 146, 90, 56):
		h.edit(profile.Profile{Server: "127.0.0.1:7677"})
	case hit(ev, 246, 90, 56) && h.selected < len(h.profiles):
		h.edit(h.profiles[h.selected])
	case hit(ev, 346, 90, 56) && h.selected < len(h.profiles):
		h.confirmDelete()
	}
}

func (h *hub) mouseEditor(ev ui.MouseEvent) {
	_, ph := h.win.Size()
	y := ph - 76
	switch {
	case hit(ev, 16, 120, y):
		h.save(false)
	case hit(ev, 146, 170, y):
		h.save(true)
	case hit(ev, 326, 100, y):
		h.screen = screenList
		h.form = nil
	default:
		if h.form != nil && h.form.Mouse(ev) && h.draft.source != h.sourceNow {
			h.buildForm()
		}
	}
}

func (h *hub) mouseSession(ev ui.MouseEvent) {
	_, ph := h.win.Size()
	y := ph - 76
	switch {
	case hit(ev, 16, 160, y):
		h.openViewer()
	case hit(ev, 186, 190, y):
		h.confirmTerminate()
	}
}

// OnResize keeps the window's size for the layouts.
func (h *hub) OnResize(width, _ int) { h.width = width }

// --- actions ---

// edit opens the editor on a profile.
func (h *hub) edit(p profile.Profile) {
	h.draft = draftOf(p)
	h.form = nil
	h.formErr = ""
	h.screen = screenEditor
}

// save writes the profile and optionally connects with it.
func (h *hub) save(connect bool) {
	p := h.draft.profile()
	if err := p.Spec.Validate(); err != nil {
		h.formErr = err.Error()
		return
	}
	if p.Server == "" {
		h.formErr = "the connection needs a server (host:port)"
		return
	}
	if err := h.opts.Store.Put(p); err != nil {
		h.formErr = err.Error()
		return
	}
	h.reload()
	h.selected = indexOf(h.profiles, p.Name)
	h.screen = screenList
	h.form = nil
	if connect {
		h.connect()
	}
}

// connect starts (or joins) the session of the selected profile.
func (h *hub) connect() {
	if h.selected >= len(h.profiles) {
		return
	}
	p := h.profiles[h.selected]
	c, err := clientFor(p)
	if err != nil {
		h.err = err.Error()
		return
	}
	h.conn = c
	h.screen = screenSession
	h.info = api.SessionInfo{State: api.StateStarting, Spec: p.Spec}
	h.status = "connecting to " + p.Server + "…"

	// A session that is already running is what there is, whatever this
	// profile says: the panel shows the truth, and changing it is a
	// deliberate act.
	ctx := context.Background()
	info, err := c.Session(ctx)
	switch {
	case err != nil:
		h.info.State, h.info.Error = api.StateLost, err.Error()
	case info != nil:
		h.info = *info
		h.status = "a session is already running on this host"
		h.watch()
	default:
		started, err := c.Start(ctx, p.Spec, false)
		if err != nil {
			h.info.State, h.info.Error = api.StateLost, err.Error()
			return
		}
		h.info = *started
		h.status = "starting…"
		h.watch()
	}
}

// watch follows the session's events so the panel stays true.
func (h *hub) watch() {
	if h.stopEvt != nil {
		h.stopEvt()
	}
	events, stop, err := h.conn.Events(context.Background())
	if err != nil {
		return
	}
	h.stopEvt = stop
	go func() {
		for ev := range events {
			switch ev.Type {
			case "log":
				h.logs = append(h.logs, ev.Line)
				if len(h.logs) > 200 {
					h.logs = h.logs[len(h.logs)-200:]
				}
			case "state":
				switch ev.State {
				case api.StateLive:
					h.status = "live — open the viewer, or close this and come back"
				case api.StateLost:
					h.status = "the session failed; the log says why"
				}
			}
		}
	}()
}

// openViewer opens the viewer window on the session. It is a window of
// its own: closing it detaches.
func (h *hub) openViewer() {
	if h.viewing || h.conn == nil || h.selected >= len(h.profiles) {
		return
	}
	p := h.profiles[h.selected]
	ctx, cancel := context.WithCancel(context.Background())
	h.viewer, h.viewing = cancel, true
	h.status = "viewer open — closing its window detaches"
	go func() {
		defer func() {
			h.viewing = false
			h.status = "detached; the session is still running"
		}()
		_ = client.Run(ctx, client.Options{
			Display:    h.opts.Display,
			ServerAddr: p.Server,
			Upscale:    p.Viewer.Upscale,
			FastScale:  p.Viewer.FastScale,
			Quality:    p.Viewer.Quality,
			TLSConfig:  tlsConfig(p),
		}, h.log)
	}()
}

// confirmDelete asks before forgetting a saved connection.
func (h *hub) confirmDelete() {
	name := h.profiles[h.selected].Name
	h.modal = fmt.Sprintf("Delete the connection %q?", name)
	h.confirm = func(ev ui.KeyEvent) bool {
		if ev.Char == 'y' {
			_ = h.opts.Store.Delete(name)
			h.reload()
			return true
		}
		return ev.Escape
	}
}

// confirmTerminate asks before ending the session and the service.
func (h *hub) confirmTerminate() {
	h.modal = "Terminate the session and stop the daemon?"
	h.confirm = func(ev ui.KeyEvent) bool {
		if ev.Char == 'y' {
			go func() { _ = h.conn.Terminate(context.Background()) }()
			if h.viewer != nil {
				h.viewer()
			}
			h.info = api.SessionInfo{State: "stopped"}
			h.status = "terminated; the daemon has stopped"
			h.screen = screenList
			return true
		}
		return ev.Escape
	}
}

// --- profile ↔ draft ---

// draftOf turns a saved profile into what the editor edits.
func draftOf(p profile.Profile) draft {
	d := draft{
		name: p.Name, server: p.Server, identity: p.Identity,
		source: p.Spec.Source, maximize: p.Spec.Maximize,
		codec: p.Spec.Stream.Codec, clipboard: true,
		resizeDesktop: p.Spec.Stream.ResizeDesktop,
	}
	if d.source == "" {
		d.source = api.SourceDesktop
	}
	if p.Spec.App != nil {
		d.appCmd = p.Spec.App.Command
	}
	if p.Spec.Window != nil {
		d.windowID = p.Spec.Window.ID
	}
	d.displayKind = p.Spec.Display.Kind
	if d.displayKind == "" {
		d.displayKind = api.KindExisting
	}
	d.displayName = p.Spec.Display.Name
	if c := p.Spec.Display.Create; c != nil {
		d.createServer, d.createSize, d.createWM, d.createHost = c.Server, c.Size, c.WM, c.HostDisplay
	}
	d.quality = itoa(p.Spec.Stream.Quality)
	d.fps = itoa(p.Spec.Stream.FPS)
	d.downscale = itoa(p.Spec.Stream.Downscale)
	if p.Spec.Stream.Clipboard != nil {
		d.clipboard = *p.Spec.Stream.Clipboard
	}
	d.upscale = itoa(p.Viewer.Upscale)
	if d.upscale == "0" {
		d.upscale = "1"
	}
	d.fastScale = p.Viewer.FastScale
	return d
}

// profile turns the editor's text back into a connection.
func (d draft) profile() profile.Profile {
	p := profile.Profile{
		Name:     strings.TrimSpace(d.name),
		Server:   strings.TrimSpace(d.server),
		Identity: strings.TrimSpace(d.identity),
		Spec: api.SessionSpec{
			Source:   d.source,
			Maximize: d.maximize,
			Stream: api.StreamSpec{
				Codec:         d.codec,
				Quality:       atoi(d.quality),
				FPS:           atoi(d.fps),
				Downscale:     atoi(d.downscale),
				Clipboard:     boolPtr(d.clipboard),
				ResizeDesktop: d.resizeDesktop,
			},
		},
		Viewer: profile.ViewerPrefs{
			Upscale:   atoi(d.upscale),
			FastScale: d.fastScale,
		},
	}
	switch d.source {
	case api.SourceApp:
		p.Spec.App = &api.AppSpec{Command: strings.TrimSpace(d.appCmd)}
	case api.SourceWindow:
		p.Spec.Window = &api.WindowSpec{ID: strings.TrimSpace(d.windowID)}
	}
	p.Spec.Display = api.DisplaySpec{Kind: d.displayKind, Name: strings.TrimSpace(d.displayName)}
	if d.displayKind == api.KindCreate {
		p.Spec.Display.Create = &api.CreateSpec{
			Server:      d.createServer,
			Size:        strings.TrimSpace(d.createSize),
			WM:          strings.TrimSpace(d.createWM),
			HostDisplay: strings.TrimSpace(d.createHost),
		}
	}
	return p
}

// --- helpers ---

// clientFor builds the control client a profile needs.
func clientFor(p profile.Profile) (*api.Client, error) {
	if p.Identity != "" {
		id, err := auth.LoadIdentity(auth.DefaultIdentityDir(p.Identity))
		if err != nil {
			return nil, err
		}
		return api.NewClientIdentity(p.Server, id)
	}
	return api.NewClient(p.Server, tlsConfig(p)), nil
}

// tlsConfig is the -tls half of a profile's link (nil = plaintext).
func tlsConfig(p profile.Profile) *tls.Config {
	if p.Identity != "" {
		id, err := auth.LoadIdentity(auth.DefaultIdentityDir(p.Identity))
		if err != nil {
			return nil
		}
		cfg, err := id.TLSConfig(p.Server)
		if err != nil {
			return nil
		}
		return cfg
	}
	if !tlsutil.On(p.TLS) {
		return nil
	}
	cfg, err := tlsutil.ClientConfig(p.TLS)
	if err != nil {
		return nil
	}
	return cfg
}

// indexOf is where a name sits in the list (-1 when it is not there).
func indexOf(list []profile.Profile, name string) int {
	for i, p := range list {
		if p.Name == name {
			return i
		}
	}
	return -1
}

// sourceName labels a spec source for the lists.
func sourceName(s string) string {
	switch s {
	case api.SourceApp:
		return "application"
	case api.SourceWindow:
		return "window"
	}
	return "desktop"
}

// itoa and atoi keep the form's numbers as text.
func itoa(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func boolPtr(b bool) *bool { return &b }
