package hub

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

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
	device     string // the credential this machine reached the host with
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
	c, device, err := link(p.Server, p.TLS, p.Identity)
	if err != nil {
		return api.SessionInfo{}, explain(err, p.Server)
	}
	s.mu.Lock()
	s.device = device
	s.mu.Unlock()

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
	cfg, err := viewerTLS(p, s.device)
	if err != nil {
		s.mu.Unlock()
		return err
	}
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
	c, _, err := link(server, tlsValue, identityName)
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
// before this call — that is what the call establishes. The authority
// that comes back is what every later connection is verified against.
func (s *Service) Pair(server, role, code, name string) (string, error) {
	cfg, err := pairingConfig("")
	if err != nil {
		return "", explain(err, server)
	}
	c := api.NewClient(server, cfg)
	keyPEM, csrPEM, err := auth.NewIdentity(name)
	if err != nil {
		return "", err
	}
	id, err := c.Pair(context.Background(), code, name, role, keyPEM, csrPEM)
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
	c, _, err := link(server, tlsValue, identityName)
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
	c, _, err := link(server, tlsValue, identityName)
	if err != nil {
		return nil, explain(err, server)
	}
	list, err := c.Clients(context.Background())
	return list, explain(err, server)
}

// Revoke withdraws one device's admission (an admin action).
func (s *Service) Revoke(server, identityName, tlsValue, name string) error {
	c, _, err := link(server, tlsValue, identityName)
	if err != nil {
		return explain(err, server)
	}
	return explain(c.Revoke(context.Background(), name), server)
}

// Device is a host this machine is paired with: what it is called, where
// it is, and the credentials used with it.
type Device struct {
	Name        string    `json:"name"`                  // what the host is called
	Server      string    `json:"server"`                // host:port
	Credential  string    `json:"credential"`            // the credential pairing made for it
	Role        string    `json:"role"`                  // what it may do there: view, control or admin
	Encrypted   bool      `json:"encrypted"`             // whether the link is TLS at all — the unsafe option is none
	Fingerprint string    `json:"fingerprint,omitempty"` // the host's certificate, when Encrypted is set
	PairedAt    time.Time `json:"pairedAt"`
}

// Devices lists the hosts this machine is paired with.
func (s *Service) Devices() ([]Device, error) {
	raw, err := os.ReadFile(devicesPath())
	if err != nil {
		if os.IsNotExist(err) {
			return []Device{}, nil
		}
		return nil, err
	}
	var list []Device
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("devices: %w", err)
	}
	return list, nil
}

// PairDevice pairs this machine with a host and keeps the record of it.
// The credential it makes is named after the host, and its key never
// leaves this machine.
//
// Encryption is the only choice to make here, and it is not a question of
// how much: the link is TLS and checked, or it is not encrypted at all.
// With encryption on, the host's certificate is read off the pairing
// connection and kept, so every later connection is checked against the
// same one — the pairing code vouches for the first answer, and the
// certificate is what is checked from then on. Nothing is typed in and
// nothing is compared by eye: the fingerprint is generated here.
func (s *Service) PairDevice(name, server, role, code string, encrypted bool) (Device, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Device{}, fmt.Errorf("the device needs a name")
	}
	if strings.TrimSpace(server) == "" {
		return Device{}, fmt.Errorf("the device needs a server address")
	}
	var c *api.Client
	if encrypted {
		cfg, err := pairingConfig("")
		if err != nil {
			return Device{}, explain(err, server)
		}
		c = api.NewClient(server, cfg)
	} else {
		c = api.NewClient(server, nil) // no TLS at all: the unsafe choice
	}
	keyPEM, csrPEM, err := auth.NewIdentity(name)
	if err != nil {
		return Device{}, err
	}
	id, err := c.Pair(context.Background(), code, name, role, keyPEM, csrPEM)
	if err != nil {
		return Device{}, explain(err, server)
	}
	if err := id.Save(auth.DefaultIdentityDir(name)); err != nil {
		return Device{}, err
	}
	rec := Device{Name: name, Server: server, Credential: name,
		Role: id.Role, Encrypted: encrypted, PairedAt: time.Now()}
	if encrypted {
		// The fingerprint is generated here and kept out of sight: it is
		// how "verified" is implemented, not something a person has to
		// read, copy or compare.
		rec.Fingerprint, err = tlsutil.ServerFingerprint(server)
		if err != nil {
			return Device{}, fmt.Errorf("could not read %s's certificate to pin it: %w", server, err)
		}
	}
	if err := s.saveDevice(rec); err != nil {
		return Device{}, err
	}
	return rec, nil
}

// RemoveDevice forgets a host and the credential made for it. The host
// is told to revoke it too, when this machine holds a credential that
// may say so; either way this machine stops using it.
func (s *Service) RemoveDevice(name string) error {
	list, err := s.Devices()
	if err != nil {
		return err
	}
	kept := make([]Device, 0, len(list))
	var gone *Device
	for i := range list {
		if list[i].Name == name {
			gone = &list[i]
			continue
		}
		kept = append(kept, list[i])
	}
	if gone == nil {
		return fmt.Errorf("no device named %q", name)
	}
	if auth.AtLeast(gone.Role, auth.RoleAdmin) {
		// Best effort: a host that is offline keeps its side of the
		// record until someone revokes it there.
		if c, err := deviceClient(*gone); err == nil {
			_ = c.Revoke(context.Background(), gone.Credential)
		}
	}
	_ = os.RemoveAll(auth.DefaultIdentityDir(gone.Credential))
	return s.writeDevices(kept)
}

// saveDevice adds or replaces one record.
func (s *Service) saveDevice(rec Device) error {
	list, err := s.Devices()
	if err != nil {
		return err
	}
	for i := range list {
		if list[i].Name == rec.Name {
			list[i] = rec
			return s.writeDevices(list)
		}
	}
	return s.writeDevices(append(list, rec))
}

func (s *Service) writeDevices(list []Device) error {
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	path := devicesPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// devicesPath is where this machine remembers its hosts.
func devicesPath() string {
	return filepath.Join(auth.ConfigHome(), "devices.json")
}

// ProbeResult is what reaching a host tells you: what it offers, and
// which of this machine's credentials it accepted ("" when it wanted
// none).
type ProbeResult struct {
	Host   api.HostInfo `json:"host"`
	Device string       `json:"device"`
}

// Probe reaches a host and reports what it offers. Everything else in
// the editor waits on this: a form full of choices the host cannot
// honour is worse than no form.
func (s *Service) Probe(server, tlsValue, named string) (ProbeResult, error) {
	c, device, err := link(server, tlsValue, named)
	if err != nil {
		return ProbeResult{}, explain(err, server)
	}
	h, err := c.Host(context.Background())
	if err != nil {
		return ProbeResult{}, explain(err, server)
	}
	s.mu.Lock()
	s.device = device
	s.mu.Unlock()
	return ProbeResult{Host: *h, Device: device}, nil
}

// --- helpers ---

// link builds the control client for a host: this machine's own
// credential when the host wants one, and the -tls value otherwise. The
// second result is the credential used — a person should not have to
// remember which key opens which door.
func link(server, tlsValue, named string) (*api.Client, string, error) {
	name := strings.TrimSpace(named)
	if name == "" {
		name = deviceFor(server, tlsValue)
	}
	if name != "" {
		id, err := auth.LoadIdentity(auth.DefaultIdentityDir(name))
		if err != nil {
			return nil, name, err
		}
		// A credential this machine paired has a record of how it was
		// paired: pinned and encrypted, or not encrypted at all.
		if rec, ok := deviceForRecord(server, name); ok {
			c, err := deviceClient(rec)
			return c, name, err
		}
		c, err := api.NewClientIdentity(server, id)
		return c, name, err
	}
	cfg, err := tlsValueConfig(tlsValue)
	if err != nil {
		return nil, "", err
	}
	return api.NewClient(server, cfg), "", nil
}

// deviceClient reaches a host as one of the devices this machine paired
// with it, the way that pairing said to: pinned and encrypted, or not
// encrypted at all. There is nothing in between, and the record cannot
// say there is.
func deviceClient(rec Device) (*api.Client, error) {
	if !rec.Encrypted {
		return api.NewClient(rec.Server, nil), nil
	}
	id, err := auth.LoadIdentity(auth.DefaultIdentityDir(rec.Credential))
	if err != nil {
		return nil, err
	}
	return api.NewClientPinned(rec.Server, id, rec.Fingerprint)
}

// deviceForRecord is the pairing record for a credential, when there is
// one: how this machine paired with that host, which is what says how to
// reach it again.
func deviceForRecord(server, name string) (Device, bool) {
	list, err := (&Service{}).Devices()
	if err != nil {
		return Device{}, false
	}
	for _, rec := range list {
		if rec.Credential == name && rec.Server == server {
			return rec, true
		}
	}
	return Device{}, false
}

// deviceFor finds the credential a host accepts: the first of this
// machine's that it admits. Credentials are ordered so the one that can
// do the most is tried first.
func deviceFor(server, tlsValue string) string {
	for _, name := range credentialNames() {
		rec, ok := deviceForRecord(server, name)
		if !ok {
			id, err := auth.LoadIdentity(auth.DefaultIdentityDir(name))
			if err != nil {
				continue
			}
			c, err := api.NewClientIdentity(server, id)
			if err != nil {
				continue
			}
			if _, err := c.Host(context.Background()); err == nil {
				return name
			}
			continue
		}
		c, err := deviceClient(rec)
		if err != nil {
			continue
		}
		if _, err := c.Host(context.Background()); err == nil {
			return name
		}
	}
	return ""
}

// credentialNames lists this machine's paired devices, the most capable
// first (admin before control before view).
func credentialNames() []string {
	entries, err := os.ReadDir(filepath.Join(auth.ConfigHome(), "credentials"))
	if err != nil {
		return nil
	}
	type candidate struct {
		name string
		rank int
	}
	var list []candidate
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id, err := auth.LoadIdentity(auth.DefaultIdentityDir(e.Name()))
		if err != nil {
			continue
		}
		list = append(list, candidate{name: e.Name(), rank: roleRank(id.Role)})
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].rank > list[j].rank })
	out := make([]string, 0, len(list))
	for _, c := range list {
		out = append(out, c.name)
	}
	return out
}

// viewerTLS is the link the viewer window opens with: the credential the
// host accepted, or the profile's -tls value when it wanted none.
func viewerTLS(p profile.Profile, device string) (*tls.Config, error) {
	if device != "" {
		id, err := auth.LoadIdentity(auth.DefaultIdentityDir(device))
		if err != nil {
			return nil, err
		}
		return id.TLSConfig(p.Server)
	}
	return tlsValueConfig(p.TLS)
}

// roleRank is what a role can do, as a number.
func roleRank(role string) int {
	switch role {
	case auth.RoleAdmin:
		return 3
	case auth.RoleControl:
		return 2
	case auth.RoleView:
		return 1
	}
	return 0
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
		return fmt.Errorf("%s speaks HTTPS: turn Encryption on to reach it", server)
	case strings.Contains(msg, "server gave HTTP response to HTTPS client"):
		return fmt.Errorf("%s speaks plain HTTP: turn Encryption off to reach it", server)
	}
	return err
}
