// Package server is the network front of remmote: it owns the listen
// address, the TLS setup and one handler per connection, and delegates
// the sharing itself — capture, encoding, input, viewers — to one
// internal/stream.Stream.
package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/lepe/remmote/internal/stream"
	"github.com/lepe/remmote/internal/tlsutil"
	"github.com/lepe/remmote/internal/xconn"
)

// Options configures the server: where and how to listen, plus the
// stream to share.
type Options struct {
	Stream      stream.Options
	ListenAddr  string
	TLS         bool   // encrypt the stream with TLS (still no client authentication)
	TLSValue    string // the -tls argument: default mode, a shared secret, or a fingerprint
	TLSCertFile string // PEM certificate for -tls ("" = generate and cache one)
	TLSKeyFile  string // PEM private key for -tls ("" = generate and cache one)
}

// Server is the running screen-sharing host: a listener in front of one
// stream.
type Server struct {
	opts Options
	log  *slog.Logger
	st   *stream.Stream
}

// New wires the X stack together. The returned server must be Run.
func New(opts Options, log *slog.Logger) (*Server, error) {
	return NewContext(context.Background(), opts, log)
}

// NewContext also permits cancellation while waiting for a launched app.
func NewContext(ctx context.Context, opts Options, log *slog.Logger) (*Server, error) {
	st, err := stream.NewContext(ctx, opts.Stream, log)
	if err != nil {
		return nil, err
	}
	return &Server{opts: opts, log: log, st: st}, nil
}

// DumpFrame captures the whole source once and writes it as PNG — the
// bring-up and diagnostics path (-dump-frame).
func (s *Server) DumpFrame(path string) error { return s.st.DumpFrame(path) }

// TestInject moves the host pointer 50 px right and back — the input
// bring-up path (-test-inject).
func (s *Server) TestInject() { s.st.TestInject() }

// Run serves until ctx is canceled, then tears everything down. It
// blocks.
func (s *Server) Run(ctx context.Context) error {
	// The stream owns X, the launched app and the capture source: they
	// must be released on every exit path, including a listener that
	// never came up.
	defer s.st.Close()

	ln, err := net.Listen("tcp", s.opts.ListenAddr)
	if err != nil {
		return fmt.Errorf("server: listen %s: %w", s.opts.ListenAddr, err)
	}
	fingerprint := ""
	clientAuth := false
	if s.opts.TLS {
		cfg, tlsErr := tlsutil.ServerConfig(s.opts.TLSCertFile, s.opts.TLSKeyFile, s.opts.TLSValue)
		if tlsErr != nil {
			_ = ln.Close()
			return tlsErr
		}
		fp, fpErr := tlsutil.Fingerprint(cfg.Certificates[0].Certificate[0])
		if fpErr != nil {
			_ = ln.Close()
			return fpErr
		}
		fingerprint = fp
		// Only the shared-secret mode asks clients for a certificate; say
		// which of the two you are in, since only one of them is a check.
		clientAuth = cfg.ClientAuth != tls.NoClientCert
		ln = tls.NewListener(ln, cfg)
	}
	switch {
	case !s.opts.TLS:
		s.log.Warn("NO AUTHENTICATION, NO ENCRYPTION: anyone who can reach this port gains full control of this machine",
			"listen", s.opts.ListenAddr, "display", xconn.DisplayString(s.opts.Stream.Display))
	case clientAuth:
		s.log.Warn("TLS: encrypted, and clients are authenticated — only a peer holding the shared secret can connect, and anyone with it gains full control of this machine",
			"listen", s.opts.ListenAddr, "display", xconn.DisplayString(s.opts.Stream.Display),
			"fingerprint", fingerprint)
	default:
		// Kept as loud as the plaintext banner: encryption alone does not
		// make this machine private to anyone who can reach the port.
		s.log.Warn("TLS: the stream is encrypted, but there is no client authentication — anyone who completes the handshake gains full control of this machine",
			"listen", s.opts.ListenAddr, "display", xconn.DisplayString(s.opts.Stream.Display),
			"fingerprint", fingerprint)
	}
	r := s.st.ScreenRect()
	s.log.Info("listening", "addr", ln.Addr().String(),
		"source", fmt.Sprintf("%dx%d", r.Dx(), r.Dy()),
		"codec", s.st.CodecName(), "fps", s.st.FPS())

	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		_ = s.st.Run(ctx)
	}()

	var handlers sync.WaitGroup
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed (shutdown)
			}
			if ctx.Err() != nil {
				conn.Close()
				return
			}
			handlers.Add(1)
			go func() { defer handlers.Done(); s.st.Attach(ctx, conn) }()
		}
	}()

	<-ctx.Done()
	_ = ln.Close() // wake Accept before waiting for connection handlers
	<-acceptDone
	handlers.Wait() // no viewer may inject input after this point
	<-streamDone    // no capture may touch X/SHM after this point
	return nil
}
