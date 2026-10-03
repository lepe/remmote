//go:build hub

// This file is the only place that knows about Wails: the window, the
// bindings the page calls, and the events that keep it true. Everything
// the interface can do lives in Service (plain Go, tested on its own).
package hub

import (
	"context"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/lepe/remmote/internal/api"
	"github.com/lepe/remmote/internal/version"
)

// App is what the window sees: the manager's behaviour, with the events
// that keep the panel honest. Its exported methods — the embedded
// Service's, plus the few below — are the page's whole API.
type App struct {
	*Service
	ctx context.Context
}

// NewApp wraps a manager for the window.
func NewApp(svc *Service) *App { return &App{Service: svc} }

// Startup wires the session's events to the page before it loads.
func (a *App) Startup(ctx context.Context) {
	a.ctx = ctx
	a.Service.OnPush(func(ev api.Event) {
		runtime.EventsEmit(ctx, "session", ev)
	})
}

// Shutdown detaches the viewer: closing the window is not a way to stop
// a session.
func (a *App) Shutdown(context.Context) {
	a.Service.CloseViewer()
}

// Title is what the window is called; the page shows it too.
func (a *App) Title() string { return "remmote " + version.Version }
