// Package tlsutil builds the TLS configuration for the remmote link.
//
// The -tls flag takes an optional value, and every form means one thing:
//
//	-tls              generate (and cache) a certificate, print its fingerprint
//	-tls auto         the same, spelled out
//	-tls <secret>     derive the certificate from a shared secret, so both
//	                  peers can reach the same fingerprint without
//	                  exchanging a log line — and, because a client must
//	                  prove it holds the matching private key, a server
//	                  started this way admits only clients using that
//	                  same secret
//	-tls SHA256:...   assert the certificate, against one supplied with
//	                  -tls-cert on the server, pinned on the client
//
// remmote has no client authentication unless a shared secret is in use (see
// the README), so TLS buys confidentiality of the video and input stream
// plus — when the client checks the certificate, or the server was given a
// secret — protection against a man in the middle. It does not turn the
// session into a private one for anyone who can reach the port when no
// secret is used, and the warning in the server log says which case you are
// in.
package tlsutil

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/scrypt"
)

// Certificate lifetime, the salt that scopes the key derivation to
// remmote, and the default location of a generated pair. The pair is
// persisted so the fingerprint survives restarts: a pin that changed on
// every server start would be useless.
const (
	certValidity = 365 * 24 * time.Hour
	secretSalt   = "remmote/tls/v1"
	certName     = "server-cert.pem"
	keyName      = "server-key.pem"
)

// scrypt parameters: 32 MiB and roughly 60 ms per derivation. Both peers
// pay this once per handshake so that a captured exchange does not hand
// an attacker a fast oracle for guessing a human-chosen secret.
const (
	scryptN = 1 << 15
	scryptR = 8
	scryptP = 1
)

// Fingerprint returns the SHA-256 of a certificate's *public key*
// (SubjectPublicKeyInfo), written "SHA256:" followed by lower-case hex.
//
// It is the key and not the certificate that has to match: an attacker
// must hold the corresponding private key to complete a handshake at all,
// and — unlike hashing the whole certificate — this stays constant for a
// given key. That is what lets the shared-secret mode compare a
// certificate it derived itself with one the server generated on the
// other side of the network.
func Fingerprint(der []byte) (string, error) {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return "", fmt.Errorf("tls: parse certificate: %w", err)
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return "SHA256:" + hex.EncodeToString(sum[:]), nil
}

// ParseFingerprint normalises a fingerprint typed or pasted by a user: the
// SHA256: prefix and any colons are optional, the hex is case-insensitive.
// It returns it in canonical form.
func ParseFingerprint(s string) (string, error) {
	v := strings.TrimSpace(s)
	if len(v) >= 7 && strings.EqualFold(v[:7], "SHA256:") {
		v = v[7:]
	}
	v = strings.ToLower(strings.ReplaceAll(v, ":", ""))
	if len(v) != sha256.Size*2 {
		return "", fmt.Errorf("tls: fingerprint must be %d hex digits, got %d", sha256.Size*2, len(v))
	}
	if _, err := hex.DecodeString(v); err != nil {
		return "", fmt.Errorf("tls: fingerprint is not hex: %w", err)
	}
	return "SHA256:" + v, nil
}

// looksLikeFingerprint is the shape test that decides whether a -tls value
// names a certificate or is a shared secret. A value carrying the SHA256:
// prefix is always a fingerprint, even a malformed one, so a typo reports
// itself instead of being quietly reinterpreted as a secret.
func looksLikeFingerprint(s string) bool {
	v := strings.TrimSpace(s)
	if len(v) >= 7 && strings.EqualFold(v[:7], "SHA256:") {
		return true
	}
	v = strings.ReplaceAll(v, ":", "")
	if len(v) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}

// isDefaultMode reports whether a -tls value means "TLS, no further
// instruction" — the value the flag carries when only -tls was given.
func isDefaultMode(v string) bool {
	switch v {
	case "", "auto", "on", "true", "1":
		return true
	}
	return false
}

// On reports whether a -tls flag value turns TLS on. Both "" (the flag was
// never given) and "off" are off, which lets the flag default to the
// spelled-out word and still read as an off switch.
func On(v string) bool { return v != "" && v != "off" }

// NormalizeArgs reshapes os.Args for the optional-value -tls flag.
//
// The Go flag package knows "-flag" and "-flag=value", but not "-flag value"
// unless the flag is a plain string. A bare "-tls" therefore fails with
// "flag needs an argument", and the tempting workaround (a flag.Value with
// IsBoolFlag) is worse: it silently drops the value *and every flag after
// it*, because parsing stops at the first non-flag argument. Callers pass
// this to flag.CommandLine.Parse instead of calling flag.Parse.
func NormalizeArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a != "-tls" && a != "--tls" {
			out = append(out, a)
			continue
		}
		// A following token that does not look like a flag is the value;
		// otherwise -tls means "on" and the remaining arguments still parse.
		// A value that itself starts with "-" is not consumed — pass
		// -tls=-value for those.
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			out = append(out, "-tls="+args[i+1])
			i++
			continue
		}
		out = append(out, "-tls=auto")
	}
	return out
}

// ServerConfig returns the server's TLS configuration for a -tls value.
// certFile and keyFile must be given together, and the value decides what
// is served:
//
//	""/auto/on  the supplied pair, else a generated one cached so the
//	            fingerprint survives restarts (in memory if there is no
//	            usable config directory)
//	shared      a certificate derived from the secret; mutually exclusive
//	            with certFile/keyFile
//	fingerprint certFile/keyFile, and startup fails unless they really are
//	            the named certificate
func ServerConfig(certFile, keyFile, value string) (*tls.Config, error) {
	if (certFile == "") != (keyFile == "") {
		return nil, fmt.Errorf("tls: -tls-cert and -tls-key must be given together")
	}
	switch {
	case isDefaultMode(value):
		return defaultServerConfig(certFile, keyFile)

	case looksLikeFingerprint(value):
		want, err := ParseFingerprint(value)
		if err != nil {
			return nil, err
		}
		if certFile == "" {
			return nil, fmt.Errorf("tls: -tls %s names a certificate, but none was supplied — "+
				"pass -tls-cert/-tls-key to serve it, or a shared secret to derive it", want)
		}
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("tls: load %s: %w", certFile, err)
		}
		got, err := Fingerprint(cert.Certificate[0])
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(got, want) {
			return nil, fmt.Errorf("tls: %s serves %s, want %s — refusing to start", certFile, got, want)
		}
		return serverTLS(cert), nil

	default: // shared secret
		if certFile != "" {
			return nil, fmt.Errorf("tls: a shared secret derives the certificate, so it cannot be combined with -tls-cert/-tls-key")
		}
		cert, err := derivedCertificate(value)
		if err != nil {
			return nil, err
		}
		want, err := Fingerprint(cert.Certificate[0])
		if err != nil {
			return nil, err
		}
		cfg := serverTLS(cert)
		// The secret authenticates the *client* too, which is what makes it
		// worth using: a peer must present a certificate derived from the same
		// secret. The certificate alone would prove nothing — its public key
		// is public — so the fingerprint is checked here, and possession of
		// the matching private key is what TLS itself verifies during the
		// handshake. Without the secret, a client cannot get in.
		cfg.ClientAuth = tls.RequireAnyClientCert
		cfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("tls: no client certificate presented — clients must pass the same -tls secret as the server")
			}
			got, err := Fingerprint(rawCerts[0])
			if err != nil {
				return err
			}
			if !strings.EqualFold(got, want) {
				return fmt.Errorf("tls: client certificate %s, expected %s — the client's -tls secret does not match the server's", got, want)
			}
			return nil
		}
		return cfg, nil
	}
}

// defaultServerConfig loads the supplied pair, or generates and caches one.
func defaultServerConfig(certFile, keyFile string) (*tls.Config, error) {
	if certFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("tls: load %s: %w", certFile, err)
		}
		return serverTLS(cert), nil
	}
	if cert, ok := loadCached(); ok {
		return serverTLS(cert), nil
	}
	cert, err := newCertificate(nil)
	if err != nil {
		return nil, err
	}
	// Persist best effort: a missing or read-only config directory is not an
	// error, it just costs a new fingerprint at the next start.
	if cachedCert, cachedKey, pathErr := cachePath(); pathErr == nil {
		_ = os.MkdirAll(filepath.Dir(cachedCert), 0o755)
		_ = writeKeyPair(cachedCert, cachedKey, cert)
	}
	return serverTLS(cert), nil
}

func serverTLS(cert tls.Certificate) *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
}

// loadCached reads a previously generated pair, reporting whether it was
// found and usable.
func loadCached() (tls.Certificate, bool) {
	certFile, keyFile, err := cachePath()
	if err != nil {
		return tls.Certificate{}, false
	}
	if _, err := os.Stat(certFile); err != nil {
		return tls.Certificate{}, false
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, false
	}
	return cert, true
}

// ClientConfig returns the client's TLS configuration for a -tls value.
//
// The default mode encrypts and accepts whatever certificate arrives — the
// simple path, and the one a client that only passed -tls takes.
//
// A fingerprint makes the client verify the server against that value.
//
// A shared secret does both at once: it is what the server expects to see
// from us (it requires a certificate derived from the same secret), and it
// is where the fingerprint we check on the server comes from.
func ClientConfig(value string) (*tls.Config, error) {
	// Accept-but-don't-verify, and warn about it at the call site.
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true}
	if !On(value) || isDefaultMode(value) {
		return cfg, nil
	}
	var want, how string
	if looksLikeFingerprint(value) {
		v, err := ParseFingerprint(value)
		if err != nil {
			return nil, err
		}
		want, how = v, "the fingerprint you passed"
	} else {
		// The shared secret works both ways: it is what the server expects to
		// see from us, and it is where the fingerprint we expect to see from
		// the server comes from — one derivation serves both checks.
		cert, err := derivedCertificate(value)
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{cert}
		want, err = Fingerprint(cert.Certificate[0])
		if err != nil {
			return nil, err
		}
		how = "derived from your shared secret"
	}
	// Pinning replaces hostname checking: the leaf is self-signed and its
	// name is whatever the host happened to be. The chain is still handed
	// over so this callback can inspect it.
	cfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("tls: server sent no certificate")
		}
		got, err := Fingerprint(rawCerts[0])
		if err != nil {
			return err
		}
		if !strings.EqualFold(got, want) {
			return fmt.Errorf("tls: server certificate %s, expected %s (%s) — refusing (drop -tls <value> to accept it)",
				got, want, how)
		}
		return nil
	}
	return cfg, nil
}

// derivedCertificate builds the certificate for a shared secret. Only the
// public key has to be a function of the secret — the fingerprint covers
// the key, not the serial number, validity or subject — so the rest is
// generated as usual.
func derivedCertificate(secret string) (tls.Certificate, error) {
	key, err := keyFromSecret(secret)
	if err != nil {
		return tls.Certificate{}, err
	}
	return newCertificate(key)
}

// keyFromSecret stretches the secret into a secp256r1 key. scrypt makes
// guessing expensive; the seed is then mapped onto the curve's scalar
// range so the result is always a valid key rather than an occasionally
// rejected one.
func keyFromSecret(secret string) (*ecdsa.PrivateKey, error) {
	if secret == "" {
		return nil, fmt.Errorf("tls: empty shared secret")
	}
	seed, err := scrypt.Key([]byte(secret), []byte(secretSalt), scryptN, scryptR, scryptP, 32)
	if err != nil {
		return nil, fmt.Errorf("tls: derive key: %w", err)
	}
	order := elliptic.P256().Params().N
	d := new(big.Int).SetBytes(seed)
	d.Mod(d, new(big.Int).Sub(order, big.NewInt(1)))
	d.Add(d, big.NewInt(1))
	priv, err := ecdhP256(d)
	if err != nil {
		return nil, err
	}
	return priv, nil
}

// ecdhP256 builds an ECDSA key from a scalar in [1, N-1], going through
// crypto/ecdh so the point is validated on the curve rather than derived by
// hand.
func ecdhP256(d *big.Int) (*ecdsa.PrivateKey, error) {
	ecdhKey, err := ecdh.P256().NewPrivateKey(d.FillBytes(make([]byte, 32)))
	if err != nil {
		return nil, fmt.Errorf("tls: derive key: %w", err)
	}
	point := ecdhKey.PublicKey().Bytes()
	if len(point) != 65 {
		return nil, fmt.Errorf("tls: unexpected public key length %d", len(point))
	}
	return &ecdsa.PrivateKey{
		D: new(big.Int).SetBytes(ecdhKey.Bytes()),
		PublicKey: ecdsa.PublicKey{
			Curve: elliptic.P256(),
			X:     new(big.Int).SetBytes(point[1:33]),
			Y:     new(big.Int).SetBytes(point[33:65]),
		},
	}, nil
}

// TCP returns the *net.TCPConn underneath c, if there is one, looking
// through a TLS wrapper too. Socket options (NoDelay, buffer sizes) are
// what they were written for; without this they would silently stop
// applying the moment TLS was switched on.
func TCP(c net.Conn) *net.TCPConn {
	if t, ok := c.(*tls.Conn); ok {
		c = t.NetConn()
	}
	tc, _ := c.(*net.TCPConn)
	return tc
}

// cachePath locates the default certificate and key paths, returning an
// error when there is no usable config directory.
func cachePath() (string, string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", "", err
	}
	dir = filepath.Join(dir, "remmote")
	return filepath.Join(dir, certName), filepath.Join(dir, keyName), nil
}

// newCertificate builds a self-signed certificate for key, generating one
// when key is nil (the -tls default path).
func newCertificate(key *ecdsa.PrivateKey) (tls.Certificate, error) {
	if key == nil {
		generated, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("tls: generate key: %w", err)
		}
		key = generated
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tls: serial: %w", err)
	}
	host, _ := os.Hostname()
	dns := []string{"localhost"}
	if host != "" {
		dns = append(dns, host)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"remmote"}, CommonName: host},
		NotBefore:             time.Now().Add(-time.Hour), // tolerate clock skew
		NotAfter:              time.Now().Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dns,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tls: create certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tls: parse certificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// writeKeyPair caches the pair so the next start reports the same
// fingerprint. Failures are the caller's to ignore: the run continues with
// an in-memory certificate.
func writeKeyPair(certFile, keyFile string, cert tls.Certificate) error {
	key, ok := cert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return fmt.Errorf("tls: key is %T, want *ecdsa.PrivateKey", cert.PrivateKey)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certFile, certPEM, 0o644); err != nil {
		return err
	}
	return os.WriteFile(keyFile, keyPEM, 0o600)
}

// Pin turns a client configuration into one that also insists on a
// particular server certificate, the way a -tls SHA256:... value does.
// It is how a client that was handed no fingerprint can still end up
// checking one: pairing learns the certificate, and Pin remembers it.
//
// cfg is modified in place and returned, so a caller can pin a config it
// already built — the paired device's own certificate and trust anchor
// stay exactly as they were.
func Pin(cfg *tls.Config, fingerprint string) (*tls.Config, error) {
	want, err := ParseFingerprint(fingerprint)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		cfg = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	// The chain may or may not verify on its own — a certificate signed by
	// the authority does, a bare -tls one does not — so the pin is checked
	// on its own terms and the library's own verdict is left out of it.
	verify := cfg.VerifyPeerCertificate
	cfg.InsecureSkipVerify = true
	cfg.VerifyPeerCertificate = func(rawCerts [][]byte, chains [][]*x509.Certificate) error {
		if verify != nil {
			if err := verify(rawCerts, chains); err != nil {
				return err
			}
		}
		if len(rawCerts) == 0 {
			return fmt.Errorf("tls: the server sent no certificate")
		}
		got, err := Fingerprint(rawCerts[0])
		if err != nil {
			return err
		}
		if !strings.EqualFold(got, want) {
			return fmt.Errorf("tls: the server's certificate is %s, but this device paired with %s — refusing", got, want)
		}
		return nil
	}
	return cfg, nil
}

// ServerFingerprint reports the fingerprint of the certificate a server
// presents at addr, without trusting it: the connection is made, the
// handshake is completed, and only then is the leaf looked at. That is
// the "whoever answers first" reading, which is what pairing does — the
// pairing code is what makes the answer the right one, and the
// fingerprint it produces is what every later connection is pinned to.
func ServerFingerprint(addr string) (string, error) {
	conn, err := tls.Dial("tcp", addr, &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // nolint:gosec // reading a fingerprint is the point
	})
	if err != nil {
		return "", err
	}
	defer conn.Close()
	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return "", fmt.Errorf("tls: %s presented no certificate", addr)
	}
	return Fingerprint(state.PeerCertificates[0].Raw)
}
