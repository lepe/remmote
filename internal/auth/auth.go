// Package auth is the daemon's per-device identity: a small certificate
// authority, a pairing flow that turns a short code into a client
// certificate, and the roles and revocation that go with it.
//
// This is a different thing from -tls <secret>. A shared secret is one
// credential for everybody: whoever holds it is indistinguishable from
// anyone else who does, and losing it means changing it everywhere. Here
// each device is its own credential — a key that never leaves it, and a
// certificate this authority signed — so a device can be named in the
// logs, given a role, and revoked on its own.
//
// The roles are the point of it. Attaching to a session is one thing;
// asking the daemon to create displays and run programs on the host is
// another, and only a device paired as an operator should do that.
package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Roles, in increasing power. A device paired as view may watch; control
// may start and stop sessions; admin may pair and revoke other devices.
const (
	RoleView    = "view"
	RoleControl = "control"
	RoleAdmin   = "admin"
)

// Valid reports whether r is a known role.
func Valid(r string) bool {
	switch r {
	case RoleView, RoleControl, RoleAdmin:
		return true
	}
	return false
}

// AtLeast reports whether role has power over want (admin > control >
// view).
func AtLeast(role, want string) bool {
	rank := func(r string) int {
		switch r {
		case RoleAdmin:
			return 3
		case RoleControl:
			return 2
		case RoleView:
			return 1
		}
		return 0
	}
	return rank(role) >= rank(want)
}

const (
	caCertName   = "ca.pem"
	caKeyName    = "ca-key.pem"
	srvCertName  = "server.pem"
	srvKeyName   = "server-key.pem"
	clientsName  = "clients.json"
	certValidity = 365 * 24 * time.Hour
	codeTTL      = 10 * time.Minute
)

// Client is a paired device: what it is called, what it may do, and the
// serial its certificate carries (the handle revocation works on).
type Client struct {
	Name     string    `json:"name"`
	Role     string    `json:"role"`
	Serial   string    `json:"serial"`
	PairedAt time.Time `json:"pairedAt"`
	Revoked  bool      `json:"revoked,omitempty"`
}

// pairingCode is a short-lived, single-use invitation to a role.
type pairingCode struct {
	Code    string    `json:"code"`
	Role    string    `json:"role"`
	Expires time.Time `json:"expires"`
}

// Authority is the daemon's CA, its client roster and its outstanding
// pairing codes.
type Authority struct {
	dir  string
	cert *x509.Certificate
	key  *ecdsa.PrivateKey

	mu      sync.Mutex
	clients map[string]Client
	codes   map[string]pairingCode
	serials map[string]string // serial hex → client name (revocation lookups)
}

// Load reads the authority from dir, creating one on first use. The CA's
// private key is what makes this machine the one that admits devices:
// directory and files are owner-only.
func Load(dir string) (*Authority, error) {
	if dir == "" {
		return nil, fmt.Errorf("auth: no directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	a := &Authority{dir: dir, clients: map[string]Client{},
		codes: map[string]pairingCode{}, serials: map[string]string{}}

	certPEM, certErr := os.ReadFile(filepath.Join(dir, caCertName))
	keyPEM, keyErr := os.ReadFile(filepath.Join(dir, caKeyName))
	if certErr == nil && keyErr == nil {
		cert, key, err := parseKeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, err
		}
		a.cert, a.key = cert, key
	} else {
		if err := a.newCA(); err != nil {
			return nil, err
		}
	}
	if err := a.loadClients(); err != nil {
		return nil, err
	}
	return a, nil
}

// CACertPEM is the certificate a client verifies the daemon against (and
// is verified by).
func (a *Authority) CACertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.cert.Raw})
}

// Fingerprint is the CA certificate's public key fingerprint, in the same
// SHA256: form as tlsutil's — something a human can read aloud.
func (a *Authority) Fingerprint() string {
	sum := sha256.Sum256(a.cert.RawSubjectPublicKeyInfo)
	return "SHA256:" + hex.EncodeToString(sum[:])
}

// ServerCert is the daemon's own certificate, signed by the CA so that a
// paired client can verify who it is talking to. Generated once and kept.
func (a *Authority) ServerCert(hosts []string) (tls.Certificate, error) {
	certPath := filepath.Join(a.dir, srvCertName)
	keyPath := filepath.Join(a.dir, srvKeyName)
	if certPEM, err := os.ReadFile(certPath); err == nil {
		if keyPEM, err := os.ReadFile(keyPath); err == nil {
			if cert, err := tls.X509KeyPair(certPEM, keyPEM); err == nil {
				return cert, nil
			}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := randomSerial()
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "remmote-daemon"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(certValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	// Names and addresses are separate lists in a certificate, and a
	// client connecting by address verifies against the second one.
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM, err := marshalKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

// NewPairingCode mints a single-use invitation to a role. The code is
// the secret a new device types in — short, because a human does.
func (a *Authority) NewPairingCode(role string, ttl time.Duration) (string, error) {
	if !Valid(role) {
		return "", fmt.Errorf("auth: %q is not a role (want %s, %s or %s)",
			role, RoleView, RoleControl, RoleAdmin)
	}
	if ttl <= 0 {
		ttl = codeTTL
	}
	// Long enough to say aloud or type without guessing: 20 characters
	// from an alphabet without look-alikes (no O/0, no I/1), well past
	// what a short-lived code needs to survive a brute force.
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, c := range raw {
		b.WriteByte(alphabet[int(c)%len(alphabet)])
	}
	code := b.String()
	a.mu.Lock()
	a.codes[code] = pairingCode{Code: code, Role: role, Expires: time.Now().Add(ttl)}
	a.mu.Unlock()
	return code, nil
}

// Pair exchanges a pairing code for a client certificate. The CSR is
// generated by the device, so its private key never leaves it. role is
// what the device is being registered as; the code can only ever grant
// what it carries, so a device cannot pair itself into more.
func (a *Authority) Pair(code, name, role string, csrPEM []byte) (certPEM []byte, granted string, err error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, "/\\") {
		return nil, "", fmt.Errorf("auth: the device needs a name (letters, digits, spaces, - and _)")
	}
	a.mu.Lock()
	invite, ok := a.codes[strings.ToUpper(strings.TrimSpace(code))]
	if !ok {
		a.mu.Unlock()
		return nil, "", fmt.Errorf("auth: no such pairing code")
	}
	if time.Now().After(invite.Expires) {
		delete(a.codes, invite.Code)
		a.mu.Unlock()
		return nil, "", fmt.Errorf("auth: that pairing code is no longer valid")
	}
	a.mu.Unlock()

	block, _ := pem.Decode(csrPEM)
	if block == nil {
		return nil, "", fmt.Errorf("auth: the signing request is not PEM")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, "", fmt.Errorf("auth: the signing request: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, "", fmt.Errorf("auth: the signing request is not signed by its own key: %w", err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	// Re-check under the lock: two devices may race for one code.
	invite, ok = a.codes[strings.ToUpper(strings.TrimSpace(code))]
	if !ok || time.Now().After(invite.Expires) {
		return nil, "", fmt.Errorf("auth: that pairing code is no longer valid")
	}
	if existing, taken := a.clients[name]; taken && !existing.Revoked {
		return nil, "", fmt.Errorf("auth: a device named %q is already paired", name)
	}
	delete(a.codes, invite.Code) // single use
	switch {
	case len(a.clients) == 0:
		// The first device is the operator's own, and it is the one that
		// mints the codes every later device is paired with. Without an
		// administrator on the roster the host could never admit another
		// device, so this is the one pair the requested role does not cap.
		role = RoleAdmin
	case !Valid(role):
		role = invite.Role
	case !AtLeast(invite.Role, role):
		role = invite.Role // the code cannot grant what it does not carry
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   name,
			Organization: []string{"remmote", role},
		},
		NotBefore:   time.Now().Add(-time.Hour),
		NotAfter:    time.Now().Add(certValidity),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, csr.PublicKey, a.key)
	if err != nil {
		return nil, "", err
	}
	a.clients[name] = Client{
		Name: name, Role: role,
		Serial: serial.Text(16), PairedAt: time.Now(),
	}
	a.serials[serial.Text(16)] = name
	if err := a.saveClients(); err != nil {
		return nil, "", err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), role, nil
}

// Clients lists the paired devices, revoked ones included.
func (a *Authority) Clients() []Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Client, 0, len(a.clients))
	for _, c := range a.clients {
		out = append(out, c)
	}
	sortClients(out)
	return out
}

// Revoke withdraws one device's admission. Its certificate stays valid
// as a certificate — it is the roster that says no.
func (a *Authority) Revoke(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.clients[name]
	if !ok {
		return fmt.Errorf("auth: no device named %q", name)
	}
	c.Revoked = true
	a.clients[name] = c
	return a.saveClients()
}

// RoleOf reports the role of the device behind a peer certificate, and
// whether it is admitted at all.
func (a *Authority) RoleOf(cert *x509.Certificate) (string, bool) {
	if cert == nil {
		return "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	name := cert.Subject.CommonName
	c, ok := a.clients[name]
	if !ok || c.Serial != cert.SerialNumber.Text(16) {
		return "", false // never paired, or reissued and superseded
	}
	if c.Revoked {
		return "", false
	}
	return c.Role, true
}

// TLSConfig is the daemon's side of the link: a device may present a
// certificate this CA signed, and revocation is checked as it is
// verified — a revoked device is refused at the door, not later. A
// device with no certificate yet is not refused here: it can reach
// exactly one call (pairing), and the handlers turn everything else away.
func (a *Authority) TLSConfig(serverCert tls.Certificate) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(a.CACertPEM()) {
		return nil, fmt.Errorf("auth: could not read the CA certificate")
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    pool,
		ClientAuth:   tls.VerifyClientCertIfGiven,
		MinVersion:   tls.VersionTLS12,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return nil // not paired yet: the handlers decide what that means
			}
			cert, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			if _, ok := a.RoleOf(cert); !ok {
				return fmt.Errorf("auth: device %q is not paired, or has been revoked",
					cert.Subject.CommonName)
			}
			return nil
		},
	}
	return cfg, nil
}

// Identity is a paired device's credential: its own key and the
// certificate the authority signed for it.
type Identity struct {
	Name string
	Role string
	Key  []byte // PEM private key
	Cert []byte // PEM certificate
	CA   []byte // PEM CA certificate (what it verifies the daemon against)
}

// NewIdentity makes a device's key and the signing request to pair with.
// The private key is generated here and never sent anywhere.
func NewIdentity(name string) (keyPEM, csrPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: name},
	}, key)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err = marshalKey(key)
	if err != nil {
		return nil, nil, err
	}
	return keyPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}), nil
}

// TLSConfig is the device's side of the link: its certificate, and the
// CA it verifies the daemon against — so a paired device knows *which*
// daemon it is talking to without trusting whatever arrives. The server
// name is taken from the address it dials.
func (i *Identity) TLSConfig(serverAddr string) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(i.Cert, i.Key)
	if err != nil {
		return nil, fmt.Errorf("auth: identity %q: %w", i.Name, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(i.CA) {
		return nil, fmt.Errorf("auth: identity %q: could not read the CA certificate", i.Name)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   hostOf(serverAddr),
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// hostOf is the host part of an address ("host:port" or "host").
func hostOf(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// PairingFiles are the names an identity is stored under in a directory.
const (
	identityKey  = "client-key.pem"
	identityCert = "client.pem"
	identityCA   = "ca.pem"
)

// DefaultIdentityDir is where this machine keeps one device's
// credential: <config>/credentials/<name>.
func DefaultIdentityDir(name string) string {
	return filepath.Join(ConfigHome(), "credentials", name)
}

// ConfigHome is where this user keeps configuration: XDG_CONFIG_HOME
// when it is set, ~/.config otherwise.
func ConfigHome() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "remmote")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "remmote")
	}
	return filepath.Join(os.TempDir(), "remmote")
}

// Save writes the identity into dir (owner-only), and LoadIdentity reads
// one back.
func (i *Identity) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for name, data := range map[string][]byte{
		identityKey: i.Key, identityCert: i.Cert, identityCA: i.CA,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// LoadIdentity reads a saved identity from dir.
func LoadIdentity(dir string) (*Identity, error) {
	read := func(name string) ([]byte, error) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("auth: identity in %s: %w", dir, err)
		}
		return b, nil
	}
	key, err := read(identityKey)
	if err != nil {
		return nil, err
	}
	cert, err := read(identityCert)
	if err != nil {
		return nil, err
	}
	ca, err := read(identityCA)
	if err != nil {
		return nil, err
	}
	id := &Identity{Key: key, Cert: cert, CA: ca}
	// The name and role are the certificate's; the directory is only
	// where it lives.
	if block, _ := pem.Decode(cert); block != nil {
		if c, err := x509.ParseCertificate(block.Bytes); err == nil {
			id.Name = c.Subject.CommonName
			id.Role = RoleFrom(c)
		}
	}
	return id, nil
}

// RoleFrom reads a device's role out of its certificate. The role rides
// in the subject's organization next to the marker "remmote"; which of
// the two comes back first is the encoding's business, not ours.
func RoleFrom(cert *x509.Certificate) string {
	if cert == nil {
		return ""
	}
	for _, org := range cert.Subject.Organization {
		if Valid(org) {
			return org
		}
	}
	return ""
}

// --- internals ---

// newCA makes this machine's authority: a self-signed certificate that
// is allowed to sign others.
func (a *Authority) newCA() error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := randomSerial()
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "remmote authority"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM, err := marshalKey(key)
	if err != nil {
		return err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	a.cert, a.key = cert, key
	if err := os.WriteFile(filepath.Join(a.dir, caCertName), certPEM, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(a.dir, caKeyName), keyPEM, 0o600)
}

// loadClients reads the roster and builds the revocation index.
func (a *Authority) loadClients() error {
	raw, err := os.ReadFile(filepath.Join(a.dir, clientsName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var list []Client
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("auth: %s: %w", clientsName, err)
	}
	for _, c := range list {
		a.clients[c.Name] = c
		a.serials[c.Serial] = c.Name
	}
	return nil
}

// saveClients writes the roster back (atomic, owner-only).
func (a *Authority) saveClients() error {
	list := make([]Client, 0, len(a.clients))
	for _, c := range a.clients {
		list = append(list, c)
	}
	sortClients(list)
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(a.dir, clientsName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// parseKeyPair reads a PEM certificate and ECDSA key.
func parseKeyPair(certPEM, keyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, nil, fmt.Errorf("auth: the CA certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("auth: the CA certificate: %w", err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("auth: the CA key is not PEM")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("auth: the CA key: %w", err)
	}
	return cert, key, nil
}

// marshalKey writes an ECDSA private key as PEM.
func marshalKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

// randomSerial is a certificate serial nobody else has.
func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
}

// sortClients orders the roster by name, for stable listings and files.
func sortClients(list []Client) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].Name < list[j-1].Name; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}
