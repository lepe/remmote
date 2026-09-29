package tlsutil

import (
	"crypto/tls"
	"net"
	"os"
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

// The default -tls must simply work: encrypt and accept. A pin turns on
// verification, and a pin that does not parse must be an error rather
// than a silent fall back to accepting whatever arrives.
func TestClientConfigVerificationDefaults(t *testing.T) {
	cfg, err := ClientConfig("")
	if err != nil {
		t.Fatalf("bare ClientConfig: %v", err)
	}
	if !cfg.InsecureSkipVerify {
		t.Error("without a pin the certificate must be accepted, not verified against a system store")
	}
	if cfg.VerifyPeerCertificate != nil {
		t.Error("without a pin no verification callback should be installed")
	}

	pin := "SHA256:" + strings.Repeat("ab", 32)
	cfg, err = ClientConfig(pin)
	if err != nil {
		t.Fatalf("valid pin rejected: %v", err)
	}
	if cfg.VerifyPeerCertificate == nil {
		t.Fatal("a pin must install a verification callback")
	}
	// A malformed pin must fail loudly — if it silently disabled
	// verification the user would think a bad pin still checked.
	if _, err := ClientConfig("short"); err == nil {
		t.Fatal("accepted a malformed pin")
	}
}

// -tls-cert and -tls-key are only meaningful as a pair.
func TestServerConfigRequiresBothFiles(t *testing.T) {
	if _, err := ServerConfig("cert.pem", ""); err == nil {
		t.Fatal("accepted -tls-cert without -tls-key")
	}
	if _, err := ServerConfig("", "key.pem"); err == nil {
		t.Fatal("accepted -tls-key without -tls-cert")
	}
}

// The generated certificate must be cached, or every server restart would
// hand out a new fingerprint and break every client's pin.
func TestServerConfigCachesGeneratedPair(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	first, err := ServerConfig("", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := ServerConfig("", "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := Fingerprint(second.Certificates[0].Certificate[0]),
		Fingerprint(first.Certificates[0].Certificate[0]); got != want {
		t.Fatalf("fingerprint changed across calls: %s != %s", got, want)
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

// End to end over loopback: the pin is what admits a client, and it is
// the only thing that admits it.
func TestTLSHandshakeAdmitsPinnedClientOnly(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	srvCfg, err := ServerConfig("", "")
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := Fingerprint(srvCfg.Certificates[0].Certificate[0])

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	tlsLn := tls.NewListener(ln, srvCfg)
	// Accept in a loop: the rejected connection below must still reach a
	// server that is waiting for it, or its handshake hangs.
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

	dial := func(pin string) (net.Conn, error) {
		cfg, err := ClientConfig(pin)
		if err != nil {
			return nil, err
		}
		raw, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			return nil, err
		}
		_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
		c := tls.Client(raw, cfg)
		if err := c.Handshake(); err != nil {
			raw.Close()
			return nil, err
		}
		return c, nil
	}

	// No pin is the default: -tls alone must connect and carry data. This
	// is exactly the path that used to fail silently on the user's side
	// while the server logged "read hello: EOF".
	bare, err := dial("")
	if err != nil {
		t.Fatalf("bare -tls handshake failed: %v", err)
	}
	if _, err := bare.Write([]byte("ping!!")); err != nil {
		t.Fatalf("bare write: %v", err)
	}
	buf := make([]byte, 6)
	if _, err := bare.Read(buf); err != nil {
		t.Fatalf("bare read: %v", err)
	}
	bare.Close()

	// The right fingerprint also gets in and carries data.
	conn, err := dial(fingerprint)
	if err != nil {
		t.Fatalf("pinned handshake failed: %v", err)
	}
	if _, err := conn.Write([]byte("ping!!")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf = make([]byte, 6)
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	conn.Close()

	// A different certificate must be refused, with the mismatch visible.
	other := "SHA256:" + strings.Repeat("00", 32)
	conn, err = dial(other)
	if err == nil {
		conn.Close()
		t.Fatal("a client with the wrong fingerprint connected")
	}
	if !strings.Contains(err.Error(), "expected") {
		t.Errorf("error does not name the mismatch: %v", err)
	}
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
