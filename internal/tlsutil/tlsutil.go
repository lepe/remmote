// Package tlsutil builds the TLS configuration for the remmote link.
//
// remmote has no client authentication by design (see the README), so TLS
// here buys two things: confidentiality of the video and input stream, and
// — when the client pins the server's certificate — protection against a
// man in the middle. It does not turn the session into a private one for
// anyone who can reach the port, and the warning in the server log stays.
package tlsutil

import (
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
)

// Certificate lifetimes and the default location of a generated pair.
// The pair is persisted so the fingerprint survives restarts: a pin that
// changed on every server start would be useless.
const (
	certValidity = 365 * 24 * time.Hour
	certName     = "server-cert.pem"
	keyName      = "server-key.pem"
)

// Fingerprint returns the SHA-256 of a certificate in DER form, written
// "SHA256:" followed by lower-case hex — long enough to be unambiguous
// and short enough to paste from the server log.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return "SHA256:" + hex.EncodeToString(sum[:])
}

// ParseFingerprint normalises a fingerprint typed or pasted by a user:
// the SHA256: prefix and any colons are optional, the hex is
// case-insensitive. It returns it in canonical form.
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

// ServerConfig returns the server's TLS configuration. certFile and
// keyFile must be given together; when they are omitted a self-signed
// certificate is generated and cached under the user's config directory,
// so a fingerprint is stable across restarts. If no config directory is
// available the certificate is generated in memory for this run alone.
func ServerConfig(certFile, keyFile string) (*tls.Config, error) {
	if (certFile == "") != (keyFile == "") {
		return nil, fmt.Errorf("tls: -tls-cert and -tls-key must be given together")
	}
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
	cert, err := selfSigned()
	if err != nil {
		return nil, err
	}
	// Persist best effort: a missing or read-only config directory is not
	// an error, it just costs a new fingerprint at the next start.
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

// ClientConfig returns the client's TLS configuration. With no pin it
// encrypts the stream and accepts whatever certificate arrives — the
// simple path, which is what someone who just passed -tls wants. Give it
// a fingerprint and it verifies instead, and that is what lets a man in
// the middle be refused. A malformed pin is an error rather than a
// silent fall back to accepting everything.
func ClientConfig(pin string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true}
	if pin == "" {
		return cfg, nil
	}
	want, err := ParseFingerprint(pin)
	if err != nil {
		return nil, err
	}
	// Pinning replaces hostname checking: the leaf is self-signed and the
	// name is whatever the host happened to be when it generated it. The
	// chain is still handed over so this callback can inspect it.
	cfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("tls: server sent no certificate")
		}
		got := Fingerprint(rawCerts[0])
		if !strings.EqualFold(got, want) {
			return fmt.Errorf("tls: server certificate %s, expected %s — refusing (drop -tls-fingerprint to accept it)", got, want)
		}
		return nil
	}
	return cfg, nil
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

// selfSigned generates an ECDSA P-256 certificate for this host.
func selfSigned() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tls: generate key: %w", err)
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
