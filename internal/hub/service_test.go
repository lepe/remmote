package hub

import (
	"errors"
	"strings"
	"testing"
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
