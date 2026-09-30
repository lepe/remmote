package auth

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPairingIssuesVerifiableIdentity(t *testing.T) {
	dir := t.TempDir()
	a, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The CA's key is what admits devices; it is owner-only.
	for _, name := range []string{caCertName, caKeyName} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("%s is %v, want 0600", name, st.Mode().Perm())
		}
	}

	code, err := a.NewPairingCode(RoleAdmin, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, csrPEM, err := NewIdentity("laptop")
	if err != nil {
		t.Fatal(err)
	}
	if len(keyPEM) == 0 || len(csrPEM) == 0 {
		t.Fatal("identity generation produced nothing")
	}
	certPEM, role, err := a.Pair(code, "laptop", csrPEM)
	if err != nil {
		t.Fatal(err)
	}
	if role != RoleAdmin {
		t.Fatalf("paired as %q, want %q", role, RoleAdmin)
	}

	// The certificate must chain to the CA — that is what makes it worth
	// anything — and carry the device's name and role.
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(a.CACertPEM())
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("issued certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("issued certificate does not verify against the CA: %v", err)
	}
	if cert.Subject.CommonName != "laptop" {
		t.Fatalf("certificate name %q, want laptop", cert.Subject.CommonName)
	}

	if got, ok := a.RoleOf(cert); !ok || got != RoleAdmin {
		t.Fatalf("RoleOf = %q %v, want admin true", got, ok)
	}

	// A pairing code is single use.
	if _, _, err := a.Pair(code, "second", mustCSR(t, "second")); err == nil {
		t.Fatal("a pairing code was used twice")
	}

	// An unknown device is not admitted.
	_, stranger, err := NewIdentity("stranger")
	if err != nil {
		t.Fatal(err)
	}
	if block, _ := pem.Decode(stranger); block != nil {
		if c, err := x509.ParseCertificate(block.Bytes); err == nil {
			if _, ok := a.RoleOf(c); ok {
				t.Fatal("an unpaired certificate was admitted")
			}
		}
	}

	// Revocation names one device and no other.
	code2, err := a.NewPairingCode(RoleView, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	phoneCSR := mustCSR(t, "phone")
	phonePEM, role, err := a.Pair(code2, "phone", phoneCSR)
	if err != nil {
		t.Fatal(err)
	}
	if role != RoleView {
		t.Fatalf("phone paired as %q, want view", role)
	}
	if err := a.Revoke("phone"); err != nil {
		t.Fatal(err)
	}
	block, _ = pem.Decode(phonePEM)
	phoneCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.RoleOf(phoneCert); ok {
		t.Fatal("a revoked device is still admitted")
	}
	if got, ok := a.RoleOf(cert); !ok || got != RoleAdmin {
		t.Fatal("revoking one device affected another")
	}
}

func TestRolesRank(t *testing.T) {
	for _, tc := range []struct {
		role, want string
		ok         bool
	}{
		{RoleAdmin, RoleControl, true},
		{RoleAdmin, RoleAdmin, true},
		{RoleControl, RoleView, true},
		{RoleControl, RoleAdmin, false},
		{RoleView, RoleControl, false},
		{"", RoleView, false},
	} {
		if got := AtLeast(tc.role, tc.want); got != tc.ok {
			t.Errorf("AtLeast(%q, %q) = %v, want %v", tc.role, tc.want, got, tc.ok)
		}
	}
	if Valid("root") {
		t.Error("Valid(root) = true")
	}
}

func TestIdentityRoundTrip(t *testing.T) {
	dir := t.TempDir()
	a, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	code, err := a.NewPairingCode(RoleControl, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, csrPEM, err := NewIdentity("desk")
	if err != nil {
		t.Fatal(err)
	}
	certPEM, _, err := a.Pair(code, "desk", csrPEM)
	if err != nil {
		t.Fatal(err)
	}
	id := &Identity{Name: "desk", Role: RoleControl, Key: keyPEM, Cert: certPEM, CA: a.CACertPEM()}
	save := filepath.Join(dir, "device")
	if err := id.Save(save); err != nil {
		t.Fatal(err)
	}
	back, err := LoadIdentity(save)
	if err != nil {
		t.Fatal(err)
	}
	if back.Name != "desk" || back.Role != RoleControl {
		t.Fatalf("identity came back as %q/%q, want desk/control", back.Name, back.Role)
	}
	if _, err := back.TLSConfig("localhost:7677"); err != nil {
		t.Fatalf("identity would not configure TLS: %v", err)
	}
}

// mustCSR makes a signing request for name.
func mustCSR(t *testing.T, name string) []byte {
	t.Helper()
	_, csr, err := NewIdentity(name)
	if err != nil {
		t.Fatal(err)
	}
	return csr
}
