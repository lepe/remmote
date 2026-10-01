package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"strings"
	"testing"
)

// mustKey is a throwaway key for a certificate that only has to exist.
func mustKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestPinAcceptsThePairedCertificateAndRefusesAnother(t *testing.T) {
	// A pair of self-signed certificates standing in for a host and for
	// whoever answers in its place.
	host, err := newCertificate(mustKey(t))
	if err != nil {
		t.Fatal(err)
	}
	other, err := newCertificate(mustKey(t))
	if err != nil {
		t.Fatal(err)
	}
	pin, err := Fingerprint(host.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}

	// The certificate the daemon presents is signed by an authority the
	// device does not have, so the library's own verification would fail
	// it: the pin has to be what decides, on its own.
	cfg, err := Pin(&tls.Config{MinVersion: tls.VersionTLS12}, pin)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.VerifyPeerCertificate([][]byte{host.Certificate[0]}, nil); err != nil {
		t.Fatalf("the paired certificate was refused: %v", err)
	}
	err = cfg.VerifyPeerCertificate([][]byte{other.Certificate[0]}, nil)
	if err == nil {
		t.Fatal("a different certificate was accepted")
	}
	if !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("the refusal does not say what happened: %v", err)
	}
	if err := cfg.VerifyPeerCertificate(nil, nil); err == nil {
		t.Fatal("a server with no certificate was accepted")
	}
}

func TestPinKeepsAnExistingCheck(t *testing.T) {
	cert, err := newCertificate(mustKey(t))
	if err != nil {
		t.Fatal(err)
	}
	pin, err := Fingerprint(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	// A revocation check the identity already had: pinning must not drop it.
	called := false
	cfg, err := Pin(&tls.Config{
		MinVersion: tls.VersionTLS12,
		VerifyPeerCertificate: func([][]byte, [][]*x509.Certificate) error {
			called = true
			return nil
		},
	}, pin)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.VerifyPeerCertificate([][]byte{cert.Certificate[0]}, nil); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("pinning replaced the check that was already there")
	}
}

func TestPinRejectsAValueThatIsNotAFingerprint(t *testing.T) {
	if _, err := Pin(&tls.Config{}, "not-a-fingerprint"); err == nil {
		t.Fatal("a malformed fingerprint was accepted")
	}
}
