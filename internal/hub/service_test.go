package hub

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lepe/remmote/internal/api"
	"github.com/lepe/remmote/internal/auth"
	"github.com/lepe/remmote/internal/profile"
	"github.com/lepe/remmote/internal/tlsutil"
)

// Pairing only exists where TLS does, so the empty -tls value must mean
// "auto" there — a device with nothing to verify yet is the whole point
// of the call. (It once meant "no TLS", and the daemon answered "Client
// sent an HTTP request to an HTTPS server".)
func TestPairingAlwaysSpeaksTLS(t *testing.T) {
	for _, value := range []string{"", "off", "auto"} {
		cfg, err := pairingConfig(value)
		if err != nil {
			t.Fatalf("pairingConfig(%q): %v", value, err)
		}
		if cfg == nil {
			t.Fatalf("pairingConfig(%q) = nil, want a TLS configuration", value)
		}
	}
	// A pin is a different question — that is tlsutil's tested business —
	// but it must at least be accepted here.
	if _, err := pairingConfig("SHA256:0000000000000000000000000000000000000000000000000000000000000000"); err != nil {
		t.Fatalf("a pinned pairing was refused: %v", err)
	}
}

// The transport says "wrong scheme" in words meant for programmers; a
// person gets told what to do about it.
func TestExplainSaysWhatToDo(t *testing.T) {
	got := explain(errors.New("net/http: server gave HTTP response to HTTPS client"), "host:7677")
	if got == nil || !strings.Contains(got.Error(), "plain HTTP") {
		t.Fatalf("plain-http error not explained: %v", got)
	}
	got = explain(errors.New("Client sent an HTTP request to an HTTPS server."), "host:7677")
	if got == nil || !strings.Contains(got.Error(), "HTTPS") {
		t.Fatalf("https error not explained: %v", got)
	}
	// Anything else is passed through untouched.
	other := errors.New("connection refused")
	if explain(other, "host") != other {
		t.Fatal("an unrelated error was rewritten")
	}
	if explain(nil, "host") != nil {
		t.Fatal("explain(nil) is not nil")
	}
}

// recordFor is the pairing record a test wants to look at: how the
// service would reach that host again.
func recordFor(t *testing.T, server, name string) Device {
	t.Helper()
	rec, ok := deviceForRecord(server, name)
	if !ok {
		t.Fatalf("no pairing record for %q at %s", name, server)
	}
	return rec
}

// pairServer is a daemon's pairing endpoint and nothing else: enough to
// pair against a real authority over a real TLS connection.
func pairServer(t *testing.T, a *auth.Authority) *httptest.Server {
	t.Helper()
	return pairServerNamed(t, a, []string{"127.0.0.1"})
}

// pairServerNamed is pairServer with a certificate naming only the given
// hosts: what a daemon whose certificate does not cover the address it is
// dialed by looks like from here.
func pairServerNamed(t *testing.T, a *auth.Authority, hosts []string) *httptest.Server {
	t.Helper()
	cert, err := a.ServerCert(hosts)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := a.TLSConfig(cert)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(pairMux(t, a))
	srv.TLS = cfg
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// plainPairServer is the same endpoint with no TLS at all, which is the
// only kind of host the unsafe choice can reach.
func plainPairServer(t *testing.T, a *auth.Authority) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(pairMux(t, a))
	t.Cleanup(srv.Close)
	return srv
}

// pairMux is the pairing endpoint on its own, so both servers above can
// serve it and only the transport differs.
func pairMux(t *testing.T, a *auth.Authority) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/pair", func(w http.ResponseWriter, r *http.Request) {
		var req api.PairRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		certPEM, role, err := a.Pair(req.Code, req.Name, req.Role, []byte(req.CSR))
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		json.NewEncoder(w).Encode(api.PairResponse{ // nolint:errcheck // the test reads the status
			Cert: string(certPEM), CA: string(a.CACertPEM()), Role: role,
		})
	})
	// The host call is what a probe and a connection both begin with, so
	// a test that reaches this server can tell that it reached it.
	mux.HandleFunc("GET /api/v1/host", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(api.HostInfo{}) // nolint:errcheck // the test reads the error
	})
	return mux
}

// Encryption on means TLS and the certificate checked: pairing keeps the
// host's certificate, and later connections are pinned to it. The pin is
// generated during pairing — never asked for — and a host answering with
// a different certificate is refused in words a person can act on.
func TestEncryptedPairingPinsTheHostCertificate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	authority, err := auth.Load(filepath.Join(home, "authority"))
	if err != nil {
		t.Fatal(err)
	}
	srv := pairServer(t, authority)
	addr := srv.Listener.Addr().String()
	pin, err := tlsutil.ServerFingerprint(addr)
	if err != nil {
		t.Fatal(err)
	}
	code, err := authority.NewPairingCode(auth.RoleAdmin, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	rec, err := (&Service{}).PairDevice("desk", addr, "control", code, true)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Fingerprint != pin {
		t.Fatalf("pinned %q, want %q", rec.Fingerprint, pin)
	}
	if !rec.Encrypted {
		t.Fatal("the record does not say the link is encrypted")
	}
	if got := recordFor(t, addr, "desk").Fingerprint; got != pin {
		t.Fatalf("later connections would pin %q, want %q", got, pin)
	}

	// A host that answers with a different certificate is refused. This
	// authority's certificate is not signed by the one the device holds, so
	// only the pin can be what decides.
	other, err := auth.Load(filepath.Join(home, "other"))
	if err != nil {
		t.Fatal(err)
	}
	impostor := pairServer(t, other)
	id, err := auth.LoadIdentity(auth.DefaultIdentityDir("desk"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := api.NewClientPinned(impostor.Listener.Addr().String(), id, pin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Host(context.Background()); err == nil {
		t.Fatal("a host with a different certificate was accepted")
	} else if !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("the refusal does not explain itself: %v", err)
	}
}

// Encryption off is not encryption of a weaker kind: there is none, and
// nothing is pinned because there is no certificate to pin. The record
// says so, and later connections go back over the same unencrypted path.
func TestUnencryptedPairingKeepsNothingToVerify(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	authority, err := auth.Load(filepath.Join(home, "authority"))
	if err != nil {
		t.Fatal(err)
	}
	srv := plainPairServer(t, authority)
	addr := srv.Listener.Addr().String()
	code, err := authority.NewPairingCode(auth.RoleAdmin, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	rec, err := (&Service{}).PairDevice("desk", addr, "control", code, false)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Encrypted {
		t.Fatal("the record claims the link is encrypted")
	}
	if rec.Fingerprint != "" {
		t.Fatalf("a fingerprint was kept although there is no TLS: %q", rec.Fingerprint)
	}
	if got := recordFor(t, addr, "desk").Fingerprint; got != "" {
		t.Fatalf("later connections would pin %q although there is no TLS", got)
	}

	// And it reaches the host the way it was paired: unencrypted.
	c, err := deviceClient(rec)
	if err != nil {
		t.Fatalf("an unencrypted device could not be configured: %v", err)
	}
	if _, err := c.Host(context.Background()); err != nil {
		t.Fatalf("an unencrypted device did not reach the host: %v", err)
	}
}

// Encryption on and encryption off are the only choices there are: there
// is no "encrypted but not verified" left to select, because the
// fingerprint is generated during pairing and kept. The same host paired
// both ways produces two records that say so.
func TestEncryptionIsEitherOnOrOff(t *testing.T) {
	for _, tc := range []struct {
		encrypted bool
		wantPin   bool
	}{
		{encrypted: true, wantPin: true},
		{encrypted: false, wantPin: false},
	} {
		home := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", home)
		authority, err := auth.Load(filepath.Join(home, "authority"))
		if err != nil {
			t.Fatal(err)
		}
		var srv *httptest.Server
		if tc.encrypted {
			srv = pairServer(t, authority)
		} else {
			srv = plainPairServer(t, authority)
		}
		code, err := authority.NewPairingCode(auth.RoleAdmin, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		rec, err := (&Service{}).PairDevice("desk", srv.Listener.Addr().String(), "control", code, tc.encrypted)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Encrypted != tc.encrypted {
			t.Fatalf("encrypted=%v recorded as %v", tc.encrypted, rec.Encrypted)
		}
		if (rec.Fingerprint != "") != tc.wantPin {
			t.Fatalf("encrypted=%v kept fingerprint %q", tc.encrypted, rec.Fingerprint)
		}
	}
}

// A saved device can be changed and saved again: the label, the address
// and whether the link is encrypted. The credential is not something to
// edit — it is a key on this machine — and the role is a note, so it can
// be corrected without the host being asked anything.
func TestUpdateDeviceSavesTheChangesAndKeepsTheCredential(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	authority, err := auth.Load(filepath.Join(home, "authority"))
	if err != nil {
		t.Fatal(err)
	}
	srv := pairServer(t, authority)
	code, err := authority.NewPairingCode(auth.RoleAdmin, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{}
	orig, err := s.PairDevice("desk", srv.Listener.Addr().String(), "admin", code, true)
	if err != nil {
		t.Fatal(err)
	}

	rec, err := s.UpdateDevice("desk", Device{
		Name: "Laptop", Server: orig.Server, Role: "view", Encrypted: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Name != "Laptop" || rec.Role != "view" {
		t.Fatalf("saved %q/%q, want Laptop/view", rec.Name, rec.Role)
	}
	if rec.Credential != orig.Credential {
		t.Fatalf("the credential changed from %q to %q", orig.Credential, rec.Credential)
	}
	if !rec.PairedAt.Equal(orig.PairedAt) {
		t.Fatalf("the pairing time changed from %v to %v", orig.PairedAt, rec.PairedAt)
	}
	if rec.Fingerprint != orig.Fingerprint {
		t.Fatalf("the pin changed from %q to %q", orig.Fingerprint, rec.Fingerprint)
	}

	list, err := s.Devices()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "Laptop" {
		t.Fatalf("the store holds %+v, want one device named Laptop", list)
	}

	// Turning encryption off drops the pin, because there is nothing to
	// check a certificate against when there is no certificate.
	rec, err = s.UpdateDevice("Laptop", Device{
		Name: "Laptop", Server: orig.Server, Role: "view", Encrypted: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Fingerprint != "" {
		t.Fatalf("encryption off kept the pin %q", rec.Fingerprint)
	}
	if rec.Encrypted {
		t.Fatal("the record still claims the link is encrypted")
	}
}

// The things that would leave the store unusable are refused where they
// are typed, not at the next connection.
func TestUpdateDeviceRefusesWhatWouldNotWork(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	authority, err := auth.Load(filepath.Join(home, "authority"))
	if err != nil {
		t.Fatal(err)
	}
	srv := pairServer(t, authority)
	code, err := authority.NewPairingCode(auth.RoleAdmin, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{}
	if _, err := s.PairDevice("desk", srv.Listener.Addr().String(), "admin", code, true); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		what string
		rec  Device
	}{
		{"no name", Device{Name: " ", Server: "h:7677", Role: "view", Encrypted: true}},
		{"no server", Device{Name: "x", Server: "", Role: "view", Encrypted: true}},
		{"a role that is not one", Device{Name: "x", Server: "h:7677", Role: "root", Encrypted: true}},
	} {
		if _, err := s.UpdateDevice("desk", tc.rec); err == nil {
			t.Fatalf("%s was accepted", tc.what)
		}
	}
	if _, err := s.UpdateDevice("never-paired", Device{Name: "x", Server: "h:7677", Role: "view", Encrypted: true}); err == nil {
		t.Fatal("a device that was never paired was updated")
	}
}

// A host this machine paired is reached the way it was paired. When that
// cannot be done the answer is an error, never a connection without the
// credential or the certificate check — otherwise a record that cannot be
// used would quietly become a less safe one.
func TestAPairedHostIsNeverReachedWithoutItsCredential(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	authority, err := auth.Load(filepath.Join(home, "authority"))
	if err != nil {
		t.Fatal(err)
	}
	srv := pairServer(t, authority)
	addr := srv.Listener.Addr().String()
	code, err := authority.NewPairingCode(auth.RoleAdmin, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{}
	rec, err := s.PairDevice("desk", addr, "control", code, true)
	if err != nil {
		t.Fatal(err)
	}

	// A record whose pin cannot be used: the credential is fine, the
	// fingerprint is not.
	broken := rec
	broken.Fingerprint = "not-a-fingerprint"
	list := []Device{broken}
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(devicesPath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err = link(addr, "", "")
	if err == nil {
		t.Fatal("a paired host was reached although its record could not be used")
	}
	if strings.Contains(err.Error(), "not encrypted") ||
		strings.Contains(err.Error(), "plain HTTP") {
		t.Fatalf("the failure was reported as a fallback to no encryption: %v", err)
	}

	// And with a sound record the same host is reached again.
	list = []Device{rec}
	if raw, err = json.Marshal(list); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(devicesPath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	c, device, err := link(addr, "", "")
	if err != nil {
		t.Fatalf("a paired host could not be reached: %v", err)
	}
	if device != "desk" {
		t.Fatalf("reached it as %q, want desk", device)
	}
	if _, err := c.Host(context.Background()); err != nil {
		t.Fatalf("the host was not reachable once the record was sound: %v", err)
	}
}

// The viewer checks the host the way pairing established it: the
// certificate that was learned then, not the name the address happens to
// carry. The control link has always been pinned like this while the
// viewer verified the name instead, so a host dialed by an address its
// certificate does not cover answered the control link and refused the
// viewer — "remote error: tls: bad certificate" in the host's log.
func TestViewerChecksTheHostTheWayPairingPinnedIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	authority, err := auth.Load(filepath.Join(home, "authority"))
	if err != nil {
		t.Fatal(err)
	}
	// A certificate naming an address nothing dials by: only the pin can
	// say this answer is still the right host.
	srv := pairServerNamed(t, authority, []string{"somewhere-else"})
	addr := srv.Listener.Addr().String()
	code, err := authority.NewPairingCode(auth.RoleAdmin, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Service{}).PairDevice("desk", addr, "control", code, true); err != nil {
		t.Fatal(err)
	}

	cfg, err := viewerTLS(profile.Profile{Server: addr}, "desk")
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil {
		t.Fatal("the viewer's link was configured for no TLS although the host was paired with it")
	}

	// The same link without the pin is refused — the name it is dialed by
	// is not one the certificate covers — so what follows is about the
	// pin and not about a certificate that happens to fit.
	id, err := auth.LoadIdentity(auth.DefaultIdentityDir("desk"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := id.TLSConfig(addr)
	if err != nil {
		t.Fatal(err)
	}
	if conn, err := tls.Dial("tcp", addr, plain); err == nil {
		conn.Close()
		t.Fatal("a name the certificate does not cover was accepted without the pin")
	}
	conn, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatalf("the viewer's link refused the host it was paired with: %v", err)
	}
	conn.Close()

	// A host answering with a different certificate is still refused.
	other, err := auth.Load(filepath.Join(home, "other"))
	if err != nil {
		t.Fatal(err)
	}
	impostor := pairServer(t, other)
	if conn, err := tls.Dial("tcp", impostor.Listener.Addr().String(), cfg); err == nil {
		conn.Close()
		t.Fatal("a host with a different certificate was accepted")
	}
}

// A host paired without encryption is reached without it — the viewer
// too. There is no certificate to check because there is no encryption;
// the record is what says so.
func TestViewerGoesBackTheWayTheHostWasPaired(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	authority, err := auth.Load(filepath.Join(home, "authority"))
	if err != nil {
		t.Fatal(err)
	}
	srv := plainPairServer(t, authority)
	addr := srv.Listener.Addr().String()
	code, err := authority.NewPairingCode(auth.RoleAdmin, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Service{}).PairDevice("desk", addr, "control", code, false); err != nil {
		t.Fatal(err)
	}

	cfg, err := viewerTLS(profile.Profile{Server: addr}, "desk")
	if err != nil {
		t.Fatalf("the viewer's link could not be configured: %v", err)
	}
	if cfg != nil {
		t.Fatal("a host paired without encryption was reached with TLS")
	}
}

// And when the record cannot vouch for the host, the answer is an error:
// the viewer is never opened on a link that checks less than pairing
// established.
func TestViewerRefusesARecordItCannotUse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	authority, err := auth.Load(filepath.Join(home, "authority"))
	if err != nil {
		t.Fatal(err)
	}
	srv := pairServer(t, authority)
	addr := srv.Listener.Addr().String()
	code, err := authority.NewPairingCode(auth.RoleAdmin, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := (&Service{}).PairDevice("desk", addr, "control", code, true)
	if err != nil {
		t.Fatal(err)
	}

	broken := rec
	broken.Fingerprint = "not-a-fingerprint"
	raw, err := json.Marshal([]Device{broken})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(devicesPath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := viewerTLS(profile.Profile{Server: addr}, "desk")
	if err == nil {
		t.Fatal("the viewer's link was configured although the record could not be used")
	}
	if cfg != nil {
		t.Fatal("a refused record still produced a link")
	}
	if strings.Contains(err.Error(), "not encrypted") {
		t.Fatalf("the failure was reported as a fallback to no encryption: %v", err)
	}
}

// Terminating a session asks the daemon to end the session — never to
// stop serving — and returns this machine's panel to how it was before
// the connection was made.
func TestTerminateResetsToBeforeConnecting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)

	var mu sync.Mutex
	var started, terminated bool
	eventsClosed := make(chan struct{})

	mux := http.NewServeMux()
	mux.HandleFunc(api.PathSession, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			if !started || terminated {
				http.Error(w, "no session is running on this daemon", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(api.SessionInfo{State: api.StateLive})
		case http.MethodPost:
			started = true
			_ = json.NewEncoder(w).Encode(api.SessionInfo{State: api.StateStarting})
		case http.MethodDelete:
			terminated = true
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		}
	})
	// The event stream stays open until the client hangs up, as a real
	// daemon's does — a Terminate that leaves it behind shows here.
	mux.HandleFunc(api.PathEvents, func(w http.ResponseWriter, r *http.Request) {
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-r.Context().Done()
		close(eventsClosed)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	addr := srv.Listener.Addr().String()

	store, err := profile.Open("")
	if err != nil {
		t.Fatal(err)
	}
	spec := api.SessionSpec{Display: api.DisplaySpec{Kind: "existing", Name: ":0"}}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(profile.Profile{Name: "desk", Server: addr, Spec: spec}); err != nil {
		t.Fatal(err)
	}
	s := NewService(store, "", nil)

	info, err := s.Connect("desk")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if info.State != api.StateStarting {
		t.Fatalf("the session did not start: state %q", info.State)
	}

	if err := s.Terminate(); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	mu.Lock()
	asked := terminated
	mu.Unlock()
	if !asked {
		t.Fatal("the daemon was never asked to end the session")
	}
	if s.conn != nil {
		t.Fatal("the connection survived the terminate")
	}
	if s.info.State != "" || s.info.Error != "" || s.info.Spec.Source != "" {
		t.Fatalf("the session record survived the terminate: %+v", s.info)
	}
	if s.logs != nil {
		t.Fatal("the session's log survived the terminate")
	}
	if s.device != "" {
		t.Fatalf("the credential survived the terminate: %q", s.device)
	}
	if st := s.Status(); st != "choose a connection, or make a new one" {
		t.Fatalf("the panel was left saying %q", st)
	}
	after, errAfter := s.Session()
	if errAfter != nil {
		t.Fatalf("the panel could not be read after the terminate: %v", errAfter)
	}
	if after.State != "" || after.Error != "" {
		t.Fatalf("after the terminate the panel still reports %+v", after)
	}
	select {
	case <-eventsClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("the session's event stream was left open")
	}
}

// Connecting to a host that is already sharing puts the viewer into what
// is on its screen; connecting to a host with nothing to join — no
// session, or only a failed one — starts a new one. Which branch ran is
// read off the daemon: joining never asks it to start anything.
func TestConnectJoinsWhatIsSharingAndStartsOverWhatIsNot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)

	var mu sync.Mutex
	var state string // "" (nothing), "live", "lost" — what GET /session says
	var postGoesLive bool
	var starts int
	var attaches int
	var streams sync.WaitGroup

	mux := http.NewServeMux()
	mux.HandleFunc(api.PathSession, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			if state == "" {
				http.Error(w, "no session is running on this daemon", http.StatusNotFound)
				return
			}
			info := api.SessionInfo{State: state}
			if state == api.StateLost {
				info.Error = "the display broke"
			}
			_ = json.NewEncoder(w).Encode(info)
		case http.MethodPost:
			starts++
			if postGoesLive {
				state = api.StateLive // the fresh session comes up
			}
			_ = json.NewEncoder(w).Encode(api.SessionInfo{State: api.StateStarting})
		case http.MethodDelete:
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		}
	})
	// Not a stream: enough to count that the viewer asked to attach. The
	// refusal ends that dial; the client retries until its context ends,
	// which the test's Terminate provides.
	mux.HandleFunc(api.PathAttach, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attaches++
		mu.Unlock()
		http.Error(w, "no stream here", http.StatusNotImplemented)
	})
	// The event stream: the current state, said again every so often —
	// a stand-in for the daemon's snapshot on subscribe and its state
	// changes, close enough to drive the watcher.
	mux.HandleFunc(api.PathEvents, func(w http.ResponseWriter, r *http.Request) {
		streams.Add(1)
		defer streams.Done()
		fl, _ := w.(http.Flusher)
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
			mu.Lock()
			st := state
			mu.Unlock()
			if st == "" {
				continue
			}
			b, _ := json.Marshal(api.Event{Type: "state", State: st})
			fmt.Fprintf(w, "data: %s\n\n", b)
			if fl != nil {
				fl.Flush()
			}
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	store, err := profile.Open("")
	if err != nil {
		t.Fatal(err)
	}
	spec := api.SessionSpec{Display: api.DisplaySpec{Kind: "existing", Name: ":0"}}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(profile.Profile{Name: "desk", Server: srv.Listener.Addr().String(), Spec: spec}); err != nil {
		t.Fatal(err)
	}
	s := NewService(store, "", slog.New(slog.NewTextHandler(io.Discard, nil)))

	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			good := ok()
			mu.Unlock()
			if good {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("never saw %s", what)
	}

	// A session is already sharing: Connect joins it and the viewer asks
	// to attach; the daemon is never asked to start anything.
	state = api.StateLive
	info, err := s.Connect("desk")
	if err != nil {
		t.Fatalf("connect to a sharing host: %v", err)
	}
	if info.State != api.StateLive {
		t.Fatalf("the running session was not reported: %+v", info)
	}
	waitFor("an attach from the viewer", func() bool { return attaches > 0 })
	mu.Lock()
	joined := starts == 0
	mu.Unlock()
	if !joined {
		t.Fatal("a host that was already sharing was asked to start a session")
	}
	if err := s.Terminate(); err != nil {
		t.Fatalf("terminate: %v", err)
	}

	// A session that only failed is nothing to join: Connect starts over,
	// and the failure stands the auto-viewer down — no attach follows.
	state = api.StateLost
	time.Sleep(300 * time.Millisecond) // let the first viewer's dial drain
	mu.Lock()
	attachedSoFar := attaches
	mu.Unlock()
	if _, err := s.Connect("desk"); err != nil {
		t.Fatalf("connect to a failed session's host: %v", err)
	}
	waitFor("a fresh start", func() bool { return starts > 0 })
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	stoodDown := attaches == attachedSoFar
	mu.Unlock()
	if !stoodDown {
		t.Fatal("a viewer was opened for a session that failed to start")
	}
	if err := s.Terminate(); err != nil {
		t.Fatalf("terminate: %v", err)
	}

	// And with nothing there at all, Connect starts one — and opens the
	// viewer in it as soon as it is live, without a click.
	state = ""
	postGoesLive = true
	if _, err := s.Connect("desk"); err != nil {
		t.Fatalf("connect to an idle host: %v", err)
	}
	waitFor("a second fresh start", func() bool { return starts > 1 })
	waitFor("the viewer that opens itself", func() bool { return attaches > attachedSoFar })
	if err := s.Terminate(); err != nil {
		t.Fatalf("terminate: %v", err)
	}

	done := make(chan struct{})
	go func() {
		streams.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the session's event streams were left open")
	}
}

// The window list is the daemon's answer, with the display asked for and
// errors said out loud — what the editor's picker shows the person who
// cannot read hex ids off xwininfo.
func TestWindowsListsWhatTheDaemonShares(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	s := NewService(nil, "", nil)

	mux := http.NewServeMux()
	mux.HandleFunc(api.PathWindows, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("display") != ":0" {
			http.Error(w, "which display?", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode([]api.WindowInfo{
			{ID: "0x2a", Title: "xcalc", Class: "XCalc"},
			{ID: "0x2b"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	addr := srv.Listener.Addr().String()

	wins, err := s.Windows(addr, "", "", ":0")
	if err != nil {
		t.Fatalf("windows: %v", err)
	}
	if len(wins) != 2 || wins[0].ID != "0x2a" || wins[0].Title != "xcalc" || wins[0].Class != "XCalc" {
		t.Fatalf("the list did not come through: %+v", wins)
	}
	if _, err := s.Windows(addr, "", "", ":99"); err == nil {
		t.Fatal("a display the daemon was not told about went unrefused")
	}
}
