package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lepe/remmote/internal/auth"
	"github.com/lepe/remmote/internal/tlsutil"
)

// Control API paths.
const (
	PathHost      = "/api/v1/host"
	PathSession   = "/api/v1/session"
	PathEvents    = "/api/v1/events"
	PathAttach    = "/api/v1/attach"
	PathWindows   = "/api/v1/windows"
	PathPair      = "/api/v1/pair"
	PathPairCodes = "/api/v1/pair-codes"
	PathClients   = "/api/v1/clients"
	PathClient    = "/api/v1/clients/{name}" // daemon route; a client addresses one by name
)

// Client talks to one daemon's control API.
type Client struct {
	addr string // host:port
	base string // scheme://host:port
	cfg  *tls.Config
	hc   *http.Client
}

// NewClient builds a control client for addr (host:port). A nil TLS
// config means plain HTTP — a loopback development setup only.
func NewClient(addr string, cfg *tls.Config) *Client {
	scheme, tr := "http", &http.Transport{}
	if cfg != nil {
		scheme, tr.TLSClientConfig = "https", cfg
	}
	return &Client{addr: addr, base: scheme + "://" + addr, cfg: cfg,
		hc: &http.Client{Transport: tr}}
}

// NewClientIdentity builds a control client that authenticates as a
// paired device: its certificate says who it is, and the CA it carries
// says which daemon it is talking to.
func NewClientIdentity(addr string, id *auth.Identity) (*Client, error) {
	cfg, err := id.TLSConfig(addr)
	if err != nil {
		return nil, err
	}
	return NewClient(addr, cfg), nil
}

// NewClientPinned is NewClientIdentity with the server's certificate
// pinned to the fingerprint pairing learned, so the connection is not
// merely encrypted but checked against the one this device paired with.
func NewClientPinned(addr string, id *auth.Identity, fingerprint string) (*Client, error) {
	cfg, err := id.TLSConfig(addr)
	if err != nil {
		return nil, err
	}
	cfg, err = tlsutil.Pin(cfg, fingerprint)
	if err != nil {
		return nil, err
	}
	return NewClient(addr, cfg), nil
}

// Host asks the daemon what it can do.
func (c *Client) Host(ctx context.Context) (*HostInfo, error) {
	resp, err := c.do(ctx, http.MethodGet, PathHost, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var h HostInfo
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}
	return &h, nil
}

// Session reports the running session, or (nil, nil) when the daemon has
// none. A session that failed to start is returned with its state and
// error, not hidden.
func (c *Client) Session(ctx context.Context) (*SessionInfo, error) {
	resp, err := c.do(ctx, http.MethodGet, PathSession, nil)
	if err != nil {
		if e, ok := err.(*Error); ok && e.Status == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	defer resp.Body.Close()
	var s SessionInfo
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}
	return &s, nil
}

// Start creates the session described by spec. With replace it stops a
// running session first ("terminate and start mine"); without it, a
// running session is refused with Error.Status 409.
func (c *Client) Start(ctx context.Context, spec SessionSpec, replace bool) (*SessionInfo, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	path := PathSession
	if replace {
		path += "?replace=1"
	}
	resp, err := c.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var s SessionInfo
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}
	return &s, nil
}

// WindowInfo is one window on a daemon's display that a session with
// source "window" can share.
type WindowInfo struct {
	ID    string `json:"id"`              // hex window id, as WindowSpec.ID wants it
	Title string `json:"title"`           // WM_NAME, or the class when unnamed
	Class string `json:"class,omitempty"` // WM_CLASS res_class
}

// Windows lists the windows on one of the daemon's displays — the
// choices a source-"window" session has. Titles are whatever the
// window's WM_NAME says.
func (c *Client) Windows(ctx context.Context, display string) ([]WindowInfo, error) {
	q := url.Values{}
	q.Set("display", display)
	resp, err := c.do(ctx, http.MethodGet, PathWindows+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var list []WindowInfo
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}
	return list, nil
}

// Terminate stops the running session. The daemon keeps running, ready
// for the next one: ending the sharing is not ending the service.
func (c *Client) Terminate(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodDelete, PathSession, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Events streams the session's state changes and log lines. The second
// result closes the stream early.
func (c *Client) Events(ctx context.Context) (<-chan Event, func(), error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+PathEvents, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, nil, decodeError(resp)
	}
	ch := make(chan Event)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		br := bufio.NewReader(resp.Body)
		for {
			line, err := br.ReadString('\n')
			if !strings.HasPrefix(line, "data: ") {
				if err != nil {
					return
				}
				continue
			}
			var ev Event
			if json.Unmarshal([]byte(strings.TrimSpace(line[6:])), &ev) == nil {
				select {
				case ch <- ev:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return ch, func() { resp.Body.Close() }, nil
}

// Pair exchanges a pairing code for this device's certificate. The key
// is what NewIdentity generated and what never leaves this machine; the
// result is the identity to save and present from then on.
func (c *Client) Pair(ctx context.Context, code, name, role string, keyPEM, csrPEM []byte) (*auth.Identity, error) {
	body, err := json.Marshal(PairRequest{Code: code, Name: name, Role: role, CSR: string(csrPEM)})
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, http.MethodPost, PathPair, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var pr PairResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}
	return &auth.Identity{Name: name, Role: pr.Role,
		Key: keyPEM, Cert: []byte(pr.Cert), CA: []byte(pr.CA)}, nil
}

// PairCode mints an invitation to a role (admin).
func (c *Client) PairCode(ctx context.Context, role, ttl string) (*PairCodeResponse, error) {
	body, err := json.Marshal(PairCodeRequest{Role: role, TTL: ttl})
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, http.MethodPost, PathPairCodes, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var pr PairCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}
	return &pr, nil
}

// Clients lists the paired devices (admin).
func (c *Client) Clients(ctx context.Context) ([]ClientInfo, error) {
	resp, err := c.do(ctx, http.MethodGet, PathClients, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var list []ClientInfo
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}
	return list, nil
}

// Revoke withdraws one device's admission (admin).
func (c *Client) Revoke(ctx context.Context, name string) error {
	resp, err := c.do(ctx, http.MethodDelete, PathClients+"/"+name, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Attach dials the daemon and upgrades the connection to the binary
// remmote stream. The returned conn is the stream: the caller speaks the
// usual ClientHello onwards on it. The connection is closed when ctx is
// done, which is what makes a viewer's disconnect leave the session
// running — the session lives on the daemon, not on this conn.
func (c *Client) Attach(ctx context.Context) (net.Conn, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return nil, err
	}
	// Tied to ctx for as long as the conn lives (no stop on return).
	_ = context.AfterFunc(ctx, func() { conn.Close() })
	if tc := tlsutil.TCP(conn); tc != nil {
		_ = tc.SetNoDelay(true)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if c.cfg != nil {
		tconn := tls.Client(conn, c.cfg)
		if err := tconn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("tls handshake with %s: %w", c.addr, err)
		}
		conn = tconn
	}
	up, err := Upgrade(conn, c.addr)
	if err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return up, nil
}

// Upgrade turns a plain connection into the binary remmote stream: it
// writes the attach request, checks for 101 Switching Protocols, and
// returns a conn whose reads continue exactly where the response ended —
// even if the response reader had already buffered stream bytes.
func Upgrade(conn net.Conn, host string) (net.Conn, error) {
	_, err := io.WriteString(conn,
		"POST "+PathAttach+" HTTP/1.1\r\n"+
			"Host: "+host+"\r\n"+
			"Connection: Upgrade\r\n"+
			"Upgrade: remmote\r\n\r\n")
	if err != nil {
		return nil, fmt.Errorf("attach request: %w", err)
	}
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("attach response: %w", err)
	}
	code := 0
	if _, err := fmt.Sscanf(status, "HTTP/1.1 %d", &code); err != nil {
		return nil, fmt.Errorf("attach response: %q", strings.TrimSpace(status))
	}
	if code != http.StatusSwitchingProtocols {
		rest, _ := io.ReadAll(io.LimitReader(br, 4096))
		return nil, upgradeError(code, append([]byte(status), rest...))
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("attach response: %w", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	return &bufferedConn{Conn: conn, r: br}, nil
}

// upgradeError turns a refused upgrade (its body is the daemon's
// message) into an Error with that message.
func upgradeError(code int, raw []byte) error {
	msg := ""
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		msg = strings.TrimSpace(string(raw[i+4:]))
	}
	if msg == "" {
		msg = http.StatusText(code)
	}
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(msg), &e) == nil && e.Error != "" {
		msg = e.Error
	}
	return &Error{Status: code, Message: msg}
}

// bufferedConn reads through a bufio.Reader that may already hold bytes
// taken from the underlying connection.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// do runs one control request and turns a non-2xx into an Error.
func (c *Client) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, decodeError(resp)
	}
	return resp, nil
}

// decodeError reads a daemon error body ("{"error": "..."}", or bare
// text) into an *Error.
func decodeError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(raw))
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error != "" {
		msg = e.Error
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return &Error{Status: resp.StatusCode, Message: msg}
}
