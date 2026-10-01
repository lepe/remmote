package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lepe/remmote/internal/api"
	"github.com/lepe/remmote/internal/auth"
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
	cert, err := a.ServerCert([]string{"127.0.0.1"})
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
	if _, err := c.Host(context.Background()); err == nil {
		// The pairing endpoint has no Host handler, so a refusal from the
		// server means the request got there. Anything else means it did not.
		if !strings.Contains(err.Error(), "404") {
			t.Fatalf("an unencrypted device did not reach the host: %v", err)
		}
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
