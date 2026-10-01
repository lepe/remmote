package hub

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/lepe/remmote/internal/api"
	"github.com/lepe/remmote/internal/auth"
	"github.com/lepe/remmote/internal/client"
	"github.com/lepe/remmote/internal/profile"
	"github.com/lepe/remmote/internal/tlsutil"
)

// Service is the connection manager's behaviour, in plain Go. The
// interface — a Wails window — is a thin layer over it, and the same
// actions stay available to anything else that wants them.
//
// The session lives on the host; this is a view onto it. Closing the
// viewer detaches and nothing more, and stopping a session is a
// deliberate act that stops the daemon too.
type Service struct {
	store   *profile.Store
	log     *slog.Logger
	display string // the local display the viewer window opens on

	mu         sync.Mutex
	conn       *api.Client
	info       api.SessionInfo
	logs       []string
	status     string
	viewing    bool
	viewerStop context.CancelFunc
	watchStop  func()
	push       func(api.Event)
}

// NewService builds the manager over a profile store.
func NewService(store *profile.Store, display string, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.NewTextHandler(nil, nil))
	}
	return &Service{
		store:   store,
		log:     log,
		display: display,
		status:  "choose a connection, or make a new one",
	}
}

// OnPush sets where session events go; the interface subscribes to show
// the log and the state as they change.
func (s *Service) OnPush(fn func(api.Event)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.push = fn
}

// Summary is one row of the connection list.
type Summary struct {
	Name     string `json:"name"`
	Server   string `json:"server"`
	Source   string `json:"source"`
	Identity string `json:"identity"`
}

// Profiles lists the saved connections.
func (s *Service) Profiles() ([]Summary, error) {
	list, err := s.store.List()
	if err != nil {
		return nil, err
	}
	out := make([]Summary, 0, len(list))
	for _, p := range list {
		out = append(out, Summary{Name: p.Name, Server: p.Server,
			Source: sourceName(p.Spec.Source), Identity: p.Identity})
	}
	return out, nil
}

// NewDraft is what the editor shows for a connection that does not exist
// yet.
func (s *Service) NewDraft() draft { return emptyDraft() }

// Edit is what the editor shows for a saved connection.
func (s *Service) Edit(name string) (draft, error) {
	p, err := s.store.Get(name)
	if err != nil {
		return draft{}, err
	}
	return draftOf(p), nil
}

// Save writes a connection (new or changed) and validates it first.
func (s *Service) Save(d draft) error {
	p := d.profile()
	if err := p.Spec.Validate(); err != nil {
		return err
	}
	if p.Server == "" {
		return fmt.Errorf("the connection needs a server (host:port)")
	}
	return s.store.Put(p)
}

// Delete forgets a connection.
func (s *Service) Delete(name string) error { return s.store.Delete(name) }

// Connect starts the selected connection's session — or joins the one
// already running on that host. The panel shows what is really there;
// changing it is a separate, deliberate act.
func (s *Service) Connect(name string) (api.SessionInfo, error) {
	p, err := s.store.Get(name)
	if err != nil {
		return api.SessionInfo{}, err
	}
	c, err := clientFor(p)
	if err != nil {
		return api.SessionInfo{}, explain(err, p.Server)
	}

	s.mu.Lock()
	if s.watchStop != nil {
		s.watchStop()
	}
	s.conn = c
	s.info = api.SessionInfo{State: api.StateStarting, Spec: p.Spec}
	s.status = "connecting to " + p.Server + "…"
	s.logs = nil
	s.mu.Unlock()

	ctx := context.Background()
	info, err := c.Session(ctx)
	switch {
	case err != nil:
		return s.fail(explain(err, p.Server))
	case info != nil:
		s.mu.Lock()
		s.info = *info
		s.status = "a session is already running on this host"
		s.mu.Unlock()
		s.watch()
		return *info, nil
	}
	started, err := c.Start(ctx, p.Spec, false)
	if err != nil {
		return s.fail(explain(err, p.Server))
	}
	s.mu.Lock()
	s.info = *started
	s.status = "starting…"
	s.mu.Unlock()
	s.watch()
	return *started, nil
}

// fail records why a session did not come up, for the panel.
func (s *Service) fail(err error) (api.SessionInfo, error) {
	s.mu.Lock()
	s.info.State, s.info.Error = api.StateLost, err.Error()
	s.status = "the session did not start"
	info := s.info
	s.mu.Unlock()
	return info, err
}

// Session is what the daemon is doing right now.
func (s *Service) Session() (api.SessionInfo, error) {
	s.mu.Lock()
	conn, info := s.conn, s.info
	s.mu.Unlock()
	if conn == nil {
		return info, nil
	}
	live, err := conn.Session(context.Background())
	if err != nil {
		return info, err
	}
	if live != nil {
		s.mu.Lock()
		s.info = *live
		s.mu.Unlock()
		return *live, nil
	}
	return info, nil
}

// Logs is the session's recent log, oldest first.
func (s *Service) Logs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.logs...)
}

// Status is the line the interface shows at the bottom.
func (s *Service) Status() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// Viewing reports whether the viewer window is open.
func (s *Service) Viewing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.viewing
}

// Terminate ends the session and stops the daemon with it — the
// deliberate end of the host's sharing.
func (s *Service) Terminate() error {
	s.mu.Lock()
	conn, stop := s.conn, s.viewerStop
	s.info = api.SessionInfo{State: "stopped"}
	s.status = "terminated; the daemon has stopped"
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
	if conn == nil {
		return nil
	}
	return conn.Terminate(context.Background())
}

// OpenViewer opens the viewer window on the session, with the viewing
// preferences of the named connection. It is a window of its own:
// closing it detaches, and the session carries on.
func (s *Service) OpenViewer(name string) error {
	p, err := s.store.Get(name)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.viewing || s.conn == nil {
		s.mu.Unlock()
		return nil
	}
	cfg := tlsConfig(p)
	ctx, cancel := context.WithCancel(context.Background())
	s.viewerStop, s.viewing = cancel, true
	s.status = "viewer open — closing its window detaches"
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			s.viewing = false
			s.status = "detached; the session is still running"
			push := s.push
			s.mu.Unlock()
			if push != nil {
				push(api.Event{Type: "state", State: api.StateLive})
			}
		}()
		_ = client.Run(ctx, client.Options{
			Display:    s.display,
			ServerAddr: p.Server,
			Upscale:    p.Viewer.Upscale,
			FastScale:  p.Viewer.FastScale,
			Quality:    p.Viewer.Quality,
			TLSConfig:  cfg,
		}, s.log)
	}()
	return nil
}

// CloseViewer closes the viewer window (a detach).
func (s *Service) CloseViewer() {
	s.mu.Lock()
	stop := s.viewerStop
	s.viewing = false
	s.status = "detached; the session is still running"
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// watch follows the session's events so the panel stays true.
func (s *Service) watch() {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return
	}
	events, stop, err := conn.Events(context.Background())
	if err != nil {
		return
	}
	s.mu.Lock()
	s.watchStop = stop
	s.mu.Unlock()
	go func() {
		for ev := range events {
			s.mu.Lock()
			if ev.Type == "log" {
				s.logs = append(s.logs, ev.Line)
				if len(s.logs) > 200 {
					s.logs = s.logs[len(s.logs)-200:]
				}
			}
			switch ev.State {
			case api.StateLive:
				s.status = "live — open the viewer, or leave this and come back"
			case api.StateLost:
				s.status = "the session failed; the log says why"
			}
			push := s.push
			s.mu.Unlock()
			if push != nil {
				push(ev)
			}
		}
	}()
}

// --- devices and pairing ---

// Host asks a daemon what it can do, with the credentials named (a
// paired device, or a -tls value).
func (s *Service) Host(server, identityName, tlsValue string) (api.HostInfo, error) {
	c, err := adHoc(server, identityName, tlsValue)
	if err != nil {
		return api.HostInfo{}, explain(err, server)
	}
	h, err := c.Host(context.Background())
	if err != nil {
		return api.HostInfo{}, explain(err, server)
	}
	return *h, nil
}

// Pair exchanges a pairing code for this machine's credential, and keeps
// it where the other commands will find it. It always speaks TLS: a
// device with no credential yet has nothing to verify the daemon with
// before this call — that is what the call establishes. A pinned value
// is honoured; without one the link is encrypted but unverified, and the
// certificate it gets back is what is checked from then on.
func (s *Service) Pair(server, tlsValue, code, name string) (string, error) {
	cfg, err := pairingConfig(tlsValue)
	if err != nil {
		return "", explain(err, server)
	}
	c := api.NewClient(server, cfg)
	keyPEM, csrPEM, err := auth.NewIdentity(name)
	if err != nil {
		return "", err
	}
	id, err := c.Pair(context.Background(), code, name, keyPEM, csrPEM)
	if err != nil {
		return "", explain(err, server)
	}
	if err := id.Save(auth.DefaultIdentityDir(name)); err != nil {
		return "", err
	}
	return id.Role, nil
}

// PairCode mints an invitation to a role (an admin action).
func (s *Service) PairCode(server, identityName, tlsValue, role string) (string, error) {
	c, err := adHoc(server, identityName, tlsValue)
	if err != nil {
		return "", explain(err, server)
	}
	resp, err := c.PairCode(context.Background(), role, "")
	if err != nil {
		return "", explain(err, server)
	}
	return resp.Code, nil
}

// Clients lists the devices a daemon has paired (an admin action).
func (s *Service) Clients(server, identityName, tlsValue string) ([]api.ClientInfo, error) {
	c, err := adHoc(server, identityName, tlsValue)
	if err != nil {
		return nil, explain(err, server)
	}
	list, err := c.Clients(context.Background())
	return list, explain(err, server)
}

// Revoke withdraws one device's admission (an admin action).
func (s *Service) Revoke(server, identityName, tlsValue, name string) error {
	c, err := adHoc(server, identityName, tlsValue)
	if err != nil {
		return explain(err, server)
	}
	return explain(c.Revoke(context.Background(), name), server)
}

// --- helpers ---

// adHoc builds a control client for one call: a paired device when one
// is named, and with a -tls value otherwise.
func adHoc(server, identityName, tlsValue string) (*api.Client, error) {
	if strings.TrimSpace(identityName) != "" {
		id, err := auth.LoadIdentity(auth.DefaultIdentityDir(identityName))
		if err != nil {
			return nil, err
		}
		return api.NewClientIdentity(server, id)
	}
	cfg, err := tlsValueConfig(tlsValue)
	if err != nil {
		return nil, err
	}
	return api.NewClient(server, cfg), nil
}

// clientFor builds the control client a profile needs.
func clientFor(p profile.Profile) (*api.Client, error) {
	if strings.TrimSpace(p.Identity) != "" {
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
	cfg, err := tlsValueConfig(p.TLS)
	if err != nil {
		return nil
	}
	return cfg
}

// tlsValueConfig turns a -tls value into a client config.
func tlsValueConfig(value string) (*tls.Config, error) {
	if !tlsutil.On(value) {
		return nil, nil
	}
	return tlsutil.ClientConfig(value)
}

// pairingConfig is how a device with no credential yet talks to the
// daemon: always TLS — pairing only exists where TLS does — verified
// when a pin is given and encrypted-only before that. Empty means auto,
// the same thing a bare -tls means on the command line.
func pairingConfig(tlsValue string) (*tls.Config, error) {
	if !tlsutil.On(tlsValue) {
		tlsValue = "auto"
	}
	return tlsutil.ClientConfig(tlsValue)
}

// explain turns the transport's way of saying "wrong scheme" into
// something a person can act on. Go's own wording ("Client sent an HTTP
// request to an HTTPS server") is true and useless.
func explain(err error, server string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "Client sent an HTTP request to an HTTPS server"):
		return fmt.Errorf("%s speaks HTTPS (it uses TLS): leave the -tls value empty for auto, or pin its certificate", server)
	case strings.Contains(msg, "server gave HTTP response to HTTPS client"):
		return fmt.Errorf("%s speaks plain HTTP: set the -tls value to off", server)
	}
	return err
}
