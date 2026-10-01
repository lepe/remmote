// Package api is the remmote control contract: the JSON a client sends a
// daemon to say what should be shared (SessionSpec), what a daemon says
// about itself and the session (HostInfo, SessionInfo), the event stream
// that reports a session starting (Event), a client for all of it, and
// the connection upgrade that turns an HTTP request into the binary
// remmote stream.
//
// The video itself is not JSON: POST /api/v1/attach upgrades the
// connection and everything after the 101 response is the usual v4 frame
// protocol. Control is JSON so the whole API is scriptable with curl and
// remmote-ctl before any GUI exists.
package api

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lepe/remmote/internal/encode"
	"github.com/lepe/remmote/internal/proto"
	"github.com/lepe/remmote/internal/stream"
)

// SessionSpec is what a client asks a daemon to share. The same JSON is
// a control API request body and the contents of a saved connection
// profile, so a profile can be hand-edited or pasted into curl.
type SessionSpec struct {
	// Source picks what to share: "desktop" (the whole screen), "app"
	// (run App.Command and share only its windows) or "window" (one
	// existing window and the windows it spawns).
	Source string `json:"source"`

	// Display is the X display to share.
	Display DisplaySpec `json:"display"`

	// Maximize maximizes the shared window on the host screen — with
	// source "app" or "window" only.
	Maximize bool `json:"maximize,omitempty"`

	App    *AppSpec    `json:"app,omitempty"`
	Window *WindowSpec `json:"window,omitempty"`

	// Stream is how to encode what is shared.
	Stream StreamSpec `json:"stream"`
}

// DisplaySpec names the X display to share: one that already exists
// ("existing"), or one the daemon creates for this session ("create").
type DisplaySpec struct {
	Kind   string      `json:"kind"`             // "existing" or "create"
	Name   string      `json:"name,omitempty"`   // "existing": e.g. ":0"
	Create *CreateSpec `json:"create,omitempty"` // "create": what to make
}

// CreateSpec describes a display the daemon creates for the session and
// tears down with it.
type CreateSpec struct {
	Server      string `json:"server"`                // "xvfb" or "xephyr"
	Size        string `json:"size,omitempty"`        // e.g. "1280x800"
	WM          string `json:"wm,omitempty"`          // window manager to run inside; empty or "none" = none
	HostDisplay string `json:"hostDisplay,omitempty"` // "xephyr": where its window opens
}

// AppSpec is the application to launch and share (source "app").
type AppSpec struct {
	Command string `json:"command"` // command line; run without a shell
}

// WindowSpec is an existing window to share (source "window").
type WindowSpec struct {
	ID string `json:"id"` // hex window id, e.g. "0x3400007"
}

// StreamSpec is how the shared surface is encoded and sent. Every field
// is optional; the defaults are the ones documented here.
type StreamSpec struct {
	Codec         string `json:"codec,omitempty"`         // hybrid | zraw | jpeg | webp (default hybrid)
	FPS           int    `json:"fps,omitempty"`           // 1..240, default 60
	Quality       int    `json:"quality,omitempty"`       // 1..100, default 75 (zraw is lossless)
	Downscale     int    `json:"downscale,omitempty"`     // 1, 2 or 4, default 1
	Refresh       string `json:"refresh,omitempty"`       // keyframe interval (Go duration), default "2s"
	Clipboard     *bool  `json:"clipboard,omitempty"`     // default true
	ResizeDesktop bool   `json:"resizeDesktop,omitempty"` // let a viewer resize the host screen
}

// Spec source and display kinds, and the display servers the daemon may
// be asked to create.
const (
	SourceDesktop = "desktop"
	SourceApp     = "app"
	SourceWindow  = "window"

	KindExisting = "existing"
	KindCreate   = "create"

	CreateXvfb   = "xvfb"
	CreateXephyr = "xephyr"
)

// HostInfo is what a daemon says about itself: enough for a client to
// fill in its option lists without guessing what the host has.
type HostInfo struct {
	ProtoVersion   int          `json:"protoVersion"`   // remmote stream protocol this daemon speaks
	Codecs         []string     `json:"codecs"`         // what it can actually encode with
	TLS            bool         `json:"tls"`            // the listener is encrypted
	CanCreate      bool         `json:"canCreate"`      // it can create displays (Xvfb/Xephyr installed)
	Displays       []string     `json:"displays"`       // displays that are up right now, e.g. ":0"
	WindowManagers []string     `json:"windowManagers"` // window managers installed here, best first
	AllowExec      bool         `json:"allowExec"`      // launching an app is permitted
	Session        *SessionInfo `json:"session,omitempty"`
}

// Session states. "lost" is a session that failed to start or has ended:
// its record, error and log stay until replaced or the service stops.
const (
	StateStarting = "starting"
	StateLive     = "live"
	StateLost     = "lost"
)

// SessionInfo is the session as the daemon sees it: the spec in force,
// what came of it, and its recent log.
type SessionInfo struct {
	State     string      `json:"state"`
	Spec      SessionSpec `json:"spec"`
	Display   string      `json:"display,omitempty"` // the display actually shared
	Width     int         `json:"width,omitempty"`   // shared surface size
	Height    int         `json:"height,omitempty"`
	AppPID    int         `json:"appPid,omitempty"` // launched app's process group, 0 when none
	StartedAt time.Time   `json:"startedAt,omitempty"`
	Error     string      `json:"error,omitempty"`
	Log       []string    `json:"log,omitempty"`
}

// Event is one step of a session's life, streamed as SSE.
type Event struct {
	Type  string    `json:"type"` // "state" or "log"
	State string    `json:"state,omitempty"`
	Line  string    `json:"line,omitempty"`
	Time  time.Time `json:"time,omitempty"`
}

// PairRequest pairs a device: the pairing code the operator passed on,
// the name to pair it under, and the signing request its own key made
// (so its private key never travels).
type PairRequest struct {
	Code string `json:"code"`
	Name string `json:"name"`
	CSR  string `json:"csr"` // PEM certificate signing request
}

// PairResponse is what a freshly paired device keeps: its certificate,
// and the CA it verifies the daemon against.
type PairResponse struct {
	Cert string `json:"cert"` // PEM
	CA   string `json:"ca"`   // PEM
	Role string `json:"role"`
}

// PairCodeRequest mints an invitation to a role — an admin pairing a
// device as view or control without handing out more admin codes.
type PairCodeRequest struct {
	Role string `json:"role"`
	TTL  string `json:"ttl,omitempty"` // Go duration; default 10m
}

// PairCodeResponse is a single-use pairing code and when it dies.
type PairCodeResponse struct {
	Code    string    `json:"code"`
	Role    string    `json:"role"`
	Expires time.Time `json:"expires"`
}

// ClientInfo is a paired device as the daemon lists it.
type ClientInfo struct {
	Name     string    `json:"name"`
	Role     string    `json:"role"`
	PairedAt time.Time `json:"pairedAt"`
	Revoked  bool      `json:"revoked,omitempty"`
}

// Error is a control API failure: the daemon's message and its status.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

// Validate fills the spec's defaults in place and rejects what cannot be
// served. Policy warnings ("this has no effect") are not errors: they are
// logged by the stream when the session starts, exactly as the CLI does.
func (s *SessionSpec) Validate() error {
	switch s.Source {
	case "":
		s.Source = SourceDesktop
	case SourceDesktop, SourceApp, SourceWindow:
	default:
		return fmt.Errorf("source must be %s, %s or %s (got %q)",
			SourceDesktop, SourceApp, SourceWindow, s.Source)
	}
	if err := s.Display.validate(); err != nil {
		return err
	}
	switch s.Source {
	case SourceApp:
		if s.Window != nil {
			return fmt.Errorf("source %s must not carry a window", SourceApp)
		}
		if s.App == nil || strings.TrimSpace(s.App.Command) == "" {
			return fmt.Errorf("source %s needs app.command", SourceApp)
		}
	case SourceWindow:
		if s.App != nil {
			return fmt.Errorf("source %s must not carry an app", SourceWindow)
		}
		if s.Window == nil {
			return fmt.Errorf("source %s needs window.id", SourceWindow)
		}
		if _, err := s.Window.parseID(); err != nil {
			return err
		}
	default:
		if s.App != nil || s.Window != nil {
			return fmt.Errorf("source %s must not carry app or window", SourceDesktop)
		}
	}
	return s.Stream.validate()
}

func (d *DisplaySpec) validate() error {
	switch d.Kind {
	case "":
		d.Kind = KindExisting
	case KindExisting:
		if strings.TrimSpace(d.Name) == "" {
			return fmt.Errorf("display.kind %s needs display.name", KindExisting)
		}
	case KindCreate:
		if d.Create == nil {
			return fmt.Errorf("display.kind %s needs display.create", KindCreate)
		}
		switch d.Create.Server {
		case CreateXvfb, CreateXephyr:
		default:
			return fmt.Errorf("display.create.server must be %s or %s (got %q)",
				CreateXvfb, CreateXephyr, d.Create.Server)
		}
		// The window manager is optional, and its "none" words all mean
		// the same thing: run no window manager at all.
		switch strings.ToLower(strings.TrimSpace(d.Create.WM)) {
		case "none", "no", "off", "-":
			d.Create.WM = ""
		}
		if d.Create.Size != "" {
			w, h, err := parseSize(d.Create.Size)
			if err != nil {
				return err
			}
			if w < 160 || h < 160 || w > 16384 || h > 16384 {
				return fmt.Errorf("display.create.size %s out of range 160..16384", d.Create.Size)
			}
		}
	default:
		return fmt.Errorf("display.kind must be %s or %s (got %q)", KindExisting, KindCreate, d.Kind)
	}
	return nil
}

func (s *StreamSpec) validate() error {
	switch s.Codec {
	case "":
		s.Codec = "hybrid"
	case "hybrid", "zraw", "jpeg", "webp":
	default:
		return fmt.Errorf("codec must be hybrid, zraw, jpeg or webp (got %q)", s.Codec)
	}
	if s.FPS == 0 {
		s.FPS = 60
	}
	if s.FPS < 1 || s.FPS > 240 {
		return fmt.Errorf("fps %d out of range 1..240", s.FPS)
	}
	if s.Quality == 0 {
		s.Quality = 75
	}
	if s.Quality < 1 || s.Quality > 100 {
		return fmt.Errorf("quality %d out of range 1..100", s.Quality)
	}
	switch s.Downscale {
	case 0:
		s.Downscale = 1
	case 1, 2, 4:
	default:
		return fmt.Errorf("downscale must be 1, 2 or 4 (got %d)", s.Downscale)
	}
	if s.Refresh == "" {
		s.Refresh = "2s"
	}
	if _, err := time.ParseDuration(s.Refresh); err != nil {
		return fmt.Errorf("refresh %q is not a duration: %w", s.Refresh, err)
	}
	return nil
}

// parseID reads a hex window id ("0x3400007" or "3400007").
func (w *WindowSpec) parseID() (uint32, error) {
	v, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(w.ID), "0x"), 16, 32)
	if err != nil || v == 0 {
		return 0, fmt.Errorf("window.id must be a hex window id like 0x3400007 (got %q)", w.ID)
	}
	return uint32(v), nil
}

// WindowID is parseID for callers that hold the spec by value.
func (s SessionSpec) WindowID() (uint32, error) {
	if s.Window == nil {
		return 0, fmt.Errorf("no window in this spec")
	}
	return s.Window.parseID()
}

// StreamOptions maps a validated spec onto the stream's options. A spec
// that creates a display has no name yet — the daemon resolves it first
// and maps the display it got.
func (s SessionSpec) StreamOptions() (stream.Options, error) {
	if err := s.Validate(); err != nil {
		return stream.Options{}, err
	}
	codec, err := codecID(s.Stream.Codec)
	if err != nil {
		return stream.Options{}, err
	}
	refresh, _ := time.ParseDuration(s.Stream.Refresh)
	opts := stream.Options{
		Display:       s.Display.Name,
		FPS:           s.Stream.FPS,
		Downscale:     s.Stream.Downscale,
		Quality:       s.Stream.Quality,
		Codec:         codec,
		FullRefresh:   refresh,
		NoClipboard:   s.Stream.Clipboard != nil && !*s.Stream.Clipboard,
		Maximize:      s.Maximize,
		ResizeDesktop: s.Stream.ResizeDesktop,
	}
	switch s.Source {
	case SourceApp:
		opts.Exec = s.App.Command
	case SourceWindow:
		id, err := s.WindowID()
		if err != nil {
			return stream.Options{}, err
		}
		opts.Window = id
	}
	return opts, nil
}

// codecID names the encoder behind a spec's codec field. The hybrid
// pseudo-codec never appears on the wire, so it lives in the encode
// package rather than in proto's codec bytes.
func codecID(name string) (uint8, error) {
	switch name {
	case "hybrid":
		return encode.CodecHybrid, nil
	case "zraw":
		return proto.CodecZRAW, nil
	case "jpeg":
		return proto.CodecJPEG, nil
	case "webp":
		return proto.CodecWebP, nil
	}
	return 0, fmt.Errorf("unknown codec %q (want hybrid, zraw, jpeg or webp)", name)
}

// Codecs lists the codecs this build can really encode with (a webp
// build without libwebp reports no webp).
func Codecs() []string {
	all := []struct {
		name string
		id   uint8
	}{
		{"hybrid", encode.CodecHybrid},
		{"zraw", proto.CodecZRAW},
		{"jpeg", proto.CodecJPEG},
		{"webp", proto.CodecWebP},
	}
	out := make([]string, 0, len(all))
	for _, c := range all {
		if _, err := encode.New(c.id); err == nil {
			out = append(out, c.name)
		}
	}
	return out
}

// parseSize reads "1280x800".
func parseSize(s string) (int, int, error) {
	w, h, ok := strings.Cut(strings.ToLower(strings.TrimSpace(s)), "x")
	if !ok {
		return 0, 0, fmt.Errorf("size must look like 1280x800 (got %q)", s)
	}
	ww, err := strconv.Atoi(strings.TrimSpace(w))
	if err != nil {
		return 0, 0, fmt.Errorf("size must look like 1280x800 (got %q)", s)
	}
	hh, err := strconv.Atoi(strings.TrimSpace(h))
	if err != nil {
		return 0, 0, fmt.Errorf("size must look like 1280x800 (got %q)", s)
	}
	return ww, hh, nil
}
