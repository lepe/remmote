package auth

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
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
	certPEM, role, err := a.Pair(code, "laptop", "", csrPEM)
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
	if _, _, err := a.Pair(code, "second", "", mustCSR(t, "second")); err == nil {
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
	phonePEM, role, err := a.Pair(code2, "phone", "", phoneCSR)
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
	// Seat the operator first: the first pair is always an administrator
	// (see TestFirstPairIsAlwaysAdministrator), and this test is about
	// a credential coming back off disk, not about the role.
	boot, err := a.NewPairingCode(RoleAdmin, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Pair(boot, "operator", "", mustCSR(t, "operator")); err != nil {
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
	certPEM, _, err := a.Pair(code, "desk", "", csrPEM)
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

// A code grants at most what it carries: a device cannot pair itself
// into more than the operator's invitation allows.
func TestPairingRoleIsCappedByTheCode(t *testing.T) {
	dir := t.TempDir()
	a, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A device asking for more than its code carries gets what the code
	// carries — so pair an operator first, since the very first pair is
	// always an administrator and would not show the cap at all.
	boot, err := a.NewPairingCode(RoleAdmin, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Pair(boot, "operator", "", mustCSR(t, "operator")); err != nil {
		t.Fatal(err)
	}
	code, err := a.NewPairingCode(RoleView, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, role, err := a.Pair(code, "greedy", RoleAdmin, mustCSR(t, "greedy"))
	if err != nil {
		t.Fatal(err)
	}
	if role != RoleView {
		t.Fatalf("a view code granted %q", role)
	}

	// And the role asked for is the one granted, when the code carries it.
	code, err = a.NewPairingCode(RoleAdmin, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, role, err = a.Pair(code, "modest", RoleControl, mustCSR(t, "modest"))
	if err != nil {
		t.Fatal(err)
	}
	if role != RoleControl {
		t.Fatalf("asked for control, got %q", role)
	}
}

// The code is something a person types: long enough to be unguessable,
// and free of the letters and digits that look alike.
func TestPairingCodeIsLongAndLegible(t *testing.T) {
	a, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		code, err := a.NewPairingCode(RoleControl, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if len(code) < 16 || len(code) > 24 {
			t.Fatalf("code %q is %d characters, want 16..24", code, len(code))
		}
		for _, r := range code {
			if !strings.ContainsRune("ABCDEFGHJKLMNPQRSTUVWXYZ23456789", r) {
				t.Fatalf("code %q has %q, which is not in the alphabet", code, r)
			}
		}
		if seen[code] {
			t.Fatalf("code %q was generated twice", code)
		}
		seen[code] = true
	}
}

// The first device is always the administrator: it is the one that mints
// the codes later devices are paired with, and asking for less would
// leave the host able to admit nobody ever again.
func TestFirstPairIsAlwaysAdministrator(t *testing.T) {
	a, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	code, err := a.NewPairingCode(RoleAdmin, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, role, err := a.Pair(code, "operator", RoleView, mustCSR(t, "operator"))
	if err != nil {
		t.Fatal(err)
	}
	if role != RoleAdmin {
		t.Fatalf("the first device was paired as %q, want admin", role)
	}

	// A second device is capped by its code, as before.
	code, err = a.NewPairingCode(RoleView, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, role, err = a.Pair(code, "viewer", RoleAdmin, mustCSR(t, "viewer")); err != nil {
		t.Fatal(err)
	}
	if role != RoleView {
		t.Fatalf("the second device was paired as %q, want view", role)
	}
}
