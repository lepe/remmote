package hub

import (
	"strconv"
	"strings"

	"github.com/lepe/remmote/internal/api"
	"github.com/lepe/remmote/internal/profile"
)

// draft is what the connection editor edits: every field is text here,
// and becomes a profile on save. A form edits strings; pretending
// otherwise is how editors end up with half-numbers in them.
type draft struct {
	Name          string `json:"name"`
	Server        string `json:"server"`
	Identity      string `json:"identity"`
	Source        string `json:"source"`
	AppCmd        string `json:"appCmd"`
	WindowID      string `json:"windowID"`
	Maximize      bool   `json:"maximize"`
	DisplayKind   string `json:"displayKind"`
	DisplayName   string `json:"displayName"`
	CreateServer  string `json:"createServer"`
	CreateSize    string `json:"createSize"`
	CreateWM      string `json:"createWM"`
	CreateHost    string `json:"createHost"`
	Codec         string `json:"codec"`
	Quality       string `json:"quality"`
	FPS           string `json:"fps"`
	Downscale     string `json:"downscale"`
	Clipboard     bool   `json:"clipboard"`
	ResizeDesktop bool   `json:"resizeDesktop"`
	Upscale       string `json:"upscale"`
	FastScale     bool   `json:"fastScale"`
}

// draftOf turns a saved profile into what the editor shows.
func draftOf(p profile.Profile) draft {
	d := draft{
		Name: p.Name, Server: p.Server, Identity: p.Identity,
		Source: p.Spec.Source, Maximize: p.Spec.Maximize,
		Codec: p.Spec.Stream.Codec, Clipboard: true,
		ResizeDesktop: p.Spec.Stream.ResizeDesktop,
	}
	if d.Source == "" {
		d.Source = api.SourceDesktop
	}
	if p.Spec.App != nil {
		d.AppCmd = p.Spec.App.Command
	}
	if p.Spec.Window != nil {
		d.WindowID = p.Spec.Window.ID
	}
	d.DisplayKind = p.Spec.Display.Kind
	if d.DisplayKind == "" {
		d.DisplayKind = api.KindExisting
	}
	d.DisplayName = p.Spec.Display.Name
	if c := p.Spec.Display.Create; c != nil {
		d.CreateServer, d.CreateSize, d.CreateWM, d.CreateHost = c.Server, c.Size, c.WM, c.HostDisplay
	}
	d.Quality = itoa(p.Spec.Stream.Quality)
	d.FPS = itoa(p.Spec.Stream.FPS)
	d.Downscale = itoa(p.Spec.Stream.Downscale)
	if p.Spec.Stream.Clipboard != nil {
		d.Clipboard = *p.Spec.Stream.Clipboard
	}
	d.Upscale = itoa(p.Viewer.Upscale)
	if d.Upscale == "0" {
		d.Upscale = "1"
	}
	d.FastScale = p.Viewer.FastScale
	return d
}

// profile turns the editor's text back into a connection.
func (d draft) profile() profile.Profile {
	p := profile.Profile{
		Name:     strings.TrimSpace(d.Name),
		Server:   strings.TrimSpace(d.Server),
		Identity: strings.TrimSpace(d.Identity),
		Spec: api.SessionSpec{
			Source:   d.Source,
			Maximize: d.Maximize,
			Stream: api.StreamSpec{
				Codec:         d.Codec,
				Quality:       atoi(d.Quality),
				FPS:           atoi(d.FPS),
				Downscale:     atoi(d.Downscale),
				Clipboard:     boolPtr(d.Clipboard),
				ResizeDesktop: d.ResizeDesktop,
			},
		},
		Viewer: profile.ViewerPrefs{
			Upscale:   atoi(d.Upscale),
			FastScale: d.FastScale,
		},
	}
	switch d.Source {
	case api.SourceApp:
		p.Spec.App = &api.AppSpec{Command: strings.TrimSpace(d.AppCmd)}
	case api.SourceWindow:
		p.Spec.Window = &api.WindowSpec{ID: strings.TrimSpace(d.WindowID)}
	}
	p.Spec.Display = api.DisplaySpec{Kind: d.DisplayKind, Name: strings.TrimSpace(d.DisplayName)}
	if d.DisplayKind == api.KindCreate {
		p.Spec.Display.Create = &api.CreateSpec{
			Server:      d.CreateServer,
			Size:        strings.TrimSpace(d.CreateSize),
			WM:          strings.TrimSpace(d.CreateWM),
			HostDisplay: strings.TrimSpace(d.CreateHost),
		}
	}
	return p
}

// emptyDraft is what the editor shows when a connection is new.
func emptyDraft() draft {
	return draft{
		Server:      "127.0.0.1:7677",
		Source:      api.SourceDesktop,
		DisplayKind: api.KindExisting,
		Codec:       "hybrid",
		Clipboard:   true,
		Upscale:     "1",
	}
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

// sourceName labels a spec source for lists and panels.
func sourceName(s string) string {
	switch s {
	case api.SourceApp:
		return "application"
	case api.SourceWindow:
		return "window"
	}
	return "desktop"
}
