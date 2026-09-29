package tlsutil

import (
	"bytes"
	"crypto/tls"
	"flag"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A fingerprint must be accepted in the shapes people actually paste:
// with or without the prefix, with or without colons, in any case.
func TestParseFingerprint(t *testing.T) {
	sum := "SHA256:" + "ab12cd34" + strings.Repeat("ab", 28) // 64 hex digits
	var (
		plain    = strings.TrimPrefix(sum, "SHA256:")
		colonned = "AB:12:CD:34" + strings.Repeat("AB", 28)
	)
	for _, in := range []string{sum, plain, colonned, "  " + plain + " ", "sha256:" + plain} {
		got, err := ParseFingerprint(in)
		if err != nil {
			t.Errorf("ParseFingerprint(%q): %v", in, err)
			continue
		}
		if got != sum {
			t.Errorf("ParseFingerprint(%q) = %q, want %q", in, got, sum)
		}
	}
	// A short or non-hex value must not be silently truncated to look valid.
	for _, bad := range []string{"", "cafe", "nothex", strings.Repeat("zz", 32)} {
		if got, err := ParseFingerprint(bad); err == nil {
			t.Errorf("ParseFingerprint(%q) = %q, want an error", bad, got)
		}
	}
}

// -tls takes an optional value, so the arguments have to be reshaped before
// the flag package sees them. This pins both the reshaping and the reason
// for it: that the value — and every flag after it — survives parsing.
func TestNormalizeArgs(t *testing.T) {
	for _, tc := range []struct{ in, want []string }{
		{[]string{}, []string{}},
		{[]string{"-tls"}, []string{"-tls=auto"}},
		{[]string{"--tls"}, []string{"-tls=auto"}},
		{[]string{"-tls", "auto"}, []string{"-tls=auto"}},
		{[]string{"-tls", "my secret"}, []string{"-tls=my secret"}},
		{[]string{"-tls", "SHA256:abcd"}, []string{"-tls=SHA256:abcd"}},
		// Already in the -flag=value form: untouched.
		{[]string{"-tls=auto"}, []string{"-tls=auto"}},
		// A following flag is never swallowed as the value.
		{[]string{"-tls", "-v"}, []string{"-tls=auto", "-v"}},
		{[]string{"-server", "h:1", "-tls", "s", "-v"}, []string{"-server", "h:1", "-tls=s", "-v"}},
		{[]string{"-v", "-tls"}, []string{"-v", "-tls=auto"}},
	} {
		if got := NormalizeArgs(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("NormalizeArgs(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// End to end: without this, "-tls auto" is rejected outright (flag
	// needs an argument), and the half-measure of a Value with IsBoolFlag
	// would drop the value *and* every flag after it.
	var (
		tlsValue string
		verbose  bool
	)
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.StringVar(&tlsValue, "tls", "off", "")
	fs.BoolVar(&verbose, "v", false, "")
	if err := fs.Parse(NormalizeArgs([]string{"-tls", "auto", "-v"})); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if tlsValue != "auto" {
		t.Errorf("tls = %q, want auto", tlsValue)
	}
	if !verbose {
		t.Error("the flag after -tls was dropped")
	}
}

// The fingerprint covers the public key, not the certificate. Two
// certificates built from one key differ in serial and validity yet must
// fingerprint identically — that is what lets both peers derive their own
// certificate from the same secret and still agree on what is acceptable.
func TestFingerprintCoversPublicKeyOnly(t *testing.T) {
	key, err := keyFromSecret("shared")
	if err != nil {
		t.Fatal(err)
	}
	first, err := newCertificate(key)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newCertificate(key)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.Certificate[0], second.Certificate[0]) {
		t.Fatal("certificates are byte-identical; the serial number should differ")
	}
	f1, err := Fingerprint(first.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	f2, err := Fingerprint(second.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if f1 != f2 {
		t.Fatalf("fingerprints differ for one key: %s vs %s", f1, f2)
	}
}

// The shared secret must produce the same certificate on both machines,
// forever: neither side ever sees the other's log line, so nothing in the
// derivation may depend on time, hostname or randomness.
func TestDerivedCertificateIsDeterministic(t *testing.T) {
	const secret = "correct horse battery staple"
	first, err := derivedCertificate(secret)
	if err != nil {
		t.Fatal(err)
	}
	second, err := derivedCertificate(secret)
	if err != nil {
		t.Fatal(err)
	}
	f1, err := Fingerprint(first.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	f2, err := Fingerprint(second.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if f1 != f2 {
		t.Fatalf("derivation is not deterministic: %s vs %s", f1, f2)
	}

	other, err := derivedCertificate("a different secret")
	if err != nil {
		t.Fatal(err)
	}
	f3, err := Fingerprint(other.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if f3 == f1 {
		t.Fatal("a different secret produced the same certificate")
	}
	if _, err := derivedCertificate(""); err == nil {
		t.Fatal("accepted an empty shared secret")
	}
}

// The default -tls must simply work: encrypt and accept. A value turns on
// verification, and a fingerprint that does not parse must be an error
// rather than a silent fall back to accepting whatever arrives.
func TestClientConfigVerificationDefaults(t *testing.T) {
	for _, v := range []string{"", "off", "auto", "on"} {
		cfg, err := ClientConfig(v)
		if err != nil {
			t.Fatalf("ClientConfig(%q): %v", v, err)
		}
		if cfg.VerifyPeerCertificate != nil {
			t.Errorf("ClientConfig(%q) installed verification without being asked", v)
		}
	}

	// A fingerprint-shaped value installs a pin.
	cfg, err := ClientConfig("SHA256:" + strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("valid pin rejected: %v", err)
	}
	if cfg.VerifyPeerCertificate == nil {
		t.Fatal("a fingerprint must install a verification callback")
	}

	// A malformed *fingerprint* must fail loudly: if a typo silently
	// disabled verification the user would believe a bad pin still checked.
	if _, err := ClientConfig("SHA256:abc"); err == nil {
		t.Fatal("accepted a malformed fingerprint")
	}

	// Anything else is a shared secret, and must verify against the
	// certificate derived from it.
	cfg, err = ClientConfig("not a fingerprint")
	if err != nil {
		t.Fatalf("shared secret rejected: %v", err)
	}
	if cfg.VerifyPeerCertificate == nil {
		t.Fatal("a shared secret must install a verification callback")
	}
}

// -tls-cert and -tls-key are only meaningful as a pair, and neither a shared
// secret nor a fingerprint may be combined with them wrongly.
func TestServerConfigRejectsConflictingForms(t *testing.T) {
	if _, err := ServerConfig("cert.pem", "", "auto"); err == nil {
		t.Error("accepted -tls-cert without -tls-key")
	}
	if _, err := ServerConfig("", "key.pem", "auto"); err == nil {
		t.Error("accepted -tls-key without -tls-cert")
	}
	// A fingerprint names a certificate, which a server cannot invent.
	if _, err := ServerConfig("", "", "SHA256:"+strings.Repeat("ab", 32)); err == nil {
		t.Error("accepted a fingerprint with no certificate to check")
	} else if !strings.Contains(err.Error(), "-tls-cert") {
		t.Errorf("error does not point at -tls-cert/-tls-key: %v", err)
	}
	// A secret derives a certificate, so there is no certificate to load.
	if _, err := ServerConfig("cert.pem", "key.pem", "my secret"); err == nil {
		t.Error("accepted a shared secret together with -tls-cert/-tls-key")
	}
}

// The generated certificate must be cached, or every server restart would
// hand out a new fingerprint and break every client's pin.
func TestServerConfigCachesGeneratedPair(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	first, err := ServerConfig("", "", "auto")
	if err != nil {
		t.Fatal(err)
	}
	second, err := ServerConfig("", "", "auto")
	if err != nil {
		t.Fatal(err)
	}
	f1, err := Fingerprint(first.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	f2, err := Fingerprint(second.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if f1 != f2 {
		t.Fatalf("fingerprint changed across calls: %s != %s", f1, f2)
	}
	certFile, keyFile, err := cachePath()
	if err != nil {
		t.Fatalf("cachePath: %v", err)
	}
	for _, path := range []string{certFile, keyFile} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("cached file missing: %v", err)
		}
	}
	// A private key readable by anyone on the box is not one.
	if fi, err := os.Stat(keyFile); err == nil && fi.Mode().Perm() != 0o600 {
		t.Errorf("key permissions = %o, want 600", fi.Mode().Perm())
	}
}

// startServer runs a loopback TLS server with cfg. dial builds the client
// side from a -tls value, the way the real dial path does; dialWith takes a
// ready-made configuration, for tests that must control exactly what the
// client presents and whether it checks anything itself.
func startServer(t *testing.T, cfg *tls.Config) (
	dial func(tlsValue string) (net.Conn, error),
	dialWith func(clientCfg *tls.Config) (net.Conn, error),
) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	tlsLn := tls.NewListener(ln, cfg)
	// Accept in a loop: a connection rejected during its handshake must
	// still reach a server that is waiting for it, or it hangs.
	go func() {
		for {
			conn, err := tlsLn.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 6)
			if _, err := conn.Read(buf); err == nil {
				_, _ = conn.Write([]byte("pong!!"))
			}
			conn.Close()
		}
	}()

	connect := func(clientCfg *tls.Config) (net.Conn, error) {
		raw, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			return nil, err
		}
		_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
		c := tls.Client(raw, clientCfg)
		if err := c.Handshake(); err != nil {
			raw.Close()
			return nil, err
		}
		return c, nil
	}
	return func(tlsValue string) (net.Conn, error) {
		clientCfg, err := ClientConfig(tlsValue)
		if err != nil {
			return nil, err
		}
		return connect(clientCfg)
	}, connect
}

// talk exchanges data over conn. A TLS 1.3 client completes its own flight
// before the server inspects the certificate it sent, so a handshake that
// returns nil proves nothing about whether it was admitted — only reading
// from the server does.
func talk(conn net.Conn) error {
	defer conn.Close()
	if _, err := conn.Write([]byte("ping!!")); err != nil {
		return err
	}
	buf := make([]byte, 6)
	_, err := conn.Read(buf)
	return err
}

// roundTrip writes through conn and expects the server's reply, proving the
// channel carries data and not merely handshakes.
func roundTrip(t *testing.T, conn net.Conn) {
	t.Helper()
	if err := talk(conn); err != nil {
		t.Fatalf("round trip failed: %v", err)
	}
}

// End to end over loopback for every trust mode: plain -tls, a pinned
// fingerprint, and a shared secret typed identically on both sides.
func TestTLSHandshakeTrustModes(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	srvCfg, err := ServerConfig("", "", "auto")
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := Fingerprint(srvCfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	dial, _ := startServer(t, srvCfg)

	// Plain -tls: encrypts, accepts whatever certificate arrives. This is
	// the path that used to fail before the user ever saw a message.
	if conn, err := dial(""); err != nil {
		t.Fatalf("plain -tls handshake failed: %v", err)
	} else {
		roundTrip(t, conn)
	}

	// The server's own fingerprint gets in.
	if conn, err := dial(fingerprint); err != nil {
		t.Fatalf("pinned handshake failed: %v", err)
	} else {
		roundTrip(t, conn)
	}

	// A different certificate must be refused, with the mismatch named.
	other := "SHA256:" + strings.Repeat("00", 32)
	conn, err := dial(other)
	if err == nil {
		conn.Close()
		t.Fatal("a client with the wrong fingerprint connected")
	}
	if !strings.Contains(err.Error(), "expected") {
		t.Errorf("error does not name the mismatch: %v", err)
	}
}

// The headline case: the same shared secret on both sides, and no log line
// to copy anywhere. A different secret must be refused.
func TestTLSSharedSecretHandshake(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	const secret = "lan-session-word"
	srvCfg, err := ServerConfig("", "", secret)
	if err != nil {
		t.Fatal(err)
	}
	dial, _ := startServer(t, srvCfg)

	if conn, err := dial(secret); err != nil {
		t.Fatalf("shared secret handshake failed: %v", err)
	} else {
		roundTrip(t, conn)
	}

	// A client without the secret still connects (no client
	// authentication) but is not the verified path — and a client with the
	// wrong one must be refused.
	conn, err := dial("wrong word")
	if err == nil {
		conn.Close()
		t.Fatal("a client with the wrong secret connected")
	}
	if !strings.Contains(err.Error(), "shared secret") {
		t.Errorf("error does not say where the expectation came from: %v", err)
	}
}

// The shared secret must gate access, not merely identify the server: a
// server started with one refuses anyone who cannot prove they hold it.
//
// These clients are built by hand instead of going through ClientConfig, so
// they cannot fail on the client side — every refusal here is the server's,
// which is the only thing that makes this authentication.
func TestServerDeniesClientsWithoutTheSharedSecret(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	const secret = "the-session-secret"
	srvCfg, err := ServerConfig("", "", secret)
	if err != nil {
		t.Fatal(err)
	}
	// If a client certificate is never requested, no client could ever be
	// denied and this would all be decoration.
	if srvCfg.ClientAuth != tls.RequireAnyClientCert {
		t.Fatalf("ClientAuth = %v, want RequireAnyClientCert", srvCfg.ClientAuth)
	}
	dial, dialWith := startServer(t, srvCfg)

	// denied reports whether the server let a client through. The handshake
	// result cannot answer that on its own, so a connection that opens is
	// then exercised with real data.
	denied := func(conn net.Conn, dialErr error, why string) {
		t.Helper()
		err := dialErr
		if err == nil {
			err = talk(conn)
		}
		if err == nil {
			t.Fatalf("%s: admitted", why)
		}
		t.Logf("%s refused: %v", why, err)
	}

	// The honest client: it holds the secret, presents its certificate, and
	// is admitted.
	held, err := dial(secret)
	if err != nil {
		t.Fatalf("a client holding the secret was refused: %v", err)
	}
	roundTrip(t, held)

	// A *different* secret: a perfectly valid certificate for a real
	// private key, just not the one this server expects. InsecureSkipVerify
	// keeps the client from rejecting the server first, so the refusal has
	// to be the server's.
	other, err := derivedCertificate("a different secret")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dialWith(&tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true,
		Certificates:       []tls.Certificate{other},
	})
	denied(conn, err, "a client with a different secret")

	// No certificate at all — the bare -tls client talking to a server
	// started with a secret.
	conn, err = dialWith(&tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true})
	denied(conn, err, "a client presenting no certificate")
}

// TCP() must find the socket under the TLS wrapper, or SetNoDelay and the
// buffer sizes stop applying as soon as -tls is turned on.
func TestTCPFindsSocketThroughTLS(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback")
	}
	defer ln.Close()
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if TCP(raw) == nil {
		t.Fatal("TCP() did not find the socket of a plain connection")
	}
	// An unconnected wrapper reports nothing rather than panicking.
	if TCP(&tls.Conn{}) != nil {
		t.Fatal("TCP() returned a socket for a zero-value tls.Conn")
	}
}
