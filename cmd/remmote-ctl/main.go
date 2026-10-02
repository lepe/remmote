// remmote-ctl: drive a remmote daemon from the terminal. It is the
// control API made scriptable — see what the daemon offers, start or
// replace the session, follow it as it comes up, and terminate it — and
// what the integration tests use before any GUI exists.
//
//	remmote-ctl [-server host:port] [-tls value] <command> [flags]
//
//	remmote-ctl host                      what this daemon can do
//	remmote-ctl session                   the session it is running
//	remmote-ctl start -spec spec.json     start one (-wait to follow it,
//	                                      -replace to stop the running one first)
//	remmote-ctl events [-follow]          state changes and log lines
//	remmote-ctl terminate                 stop the running session (the daemon keeps running)
//
// The spec is JSON — the same shape as a saved connection profile — and
// may come from a file or, with -spec -, from stdin.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lepe/remmote/internal/api"
	"github.com/lepe/remmote/internal/auth"
	"github.com/lepe/remmote/internal/tlsutil"
)

func main() {
	global := flag.NewFlagSet("remmote-ctl", flag.ExitOnError)
	server := global.String("server", "127.0.0.1:7677", "daemon address host:port")
	useTLS := global.String("tls", "off", "encryption: 'auto' (or no value), the daemon's shared secret, or SHA256:… to pin its certificate")
	identity := global.String("identity", "", "authenticate as this paired device (its credential lives in ~/.config/remmote/credentials/<name>)")
	global.Usage = usage
	// -tls takes an optional value; reshape the arguments first.
	global.Parse(tlsutil.NormalizeArgs(os.Args[1:]))

	args := global.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	cmd, rest := args[0], args[1:]

	sub := flag.NewFlagSet(cmd, flag.ExitOnError)
	specPath := sub.String("spec", "", "session spec as JSON: a file path, or '-' for stdin")
	replace := sub.Bool("replace", false, "stop the running session first (terminate and start mine)")
	waitLive := sub.Bool("wait", false, "follow the session until it is live (or has failed)")
	follow := sub.Bool("follow", false, "stay attached until interrupted")
	pairCode := sub.String("code", "", "the pairing code the operator passed on (pair)")
	pairName := sub.String("name", "", "what to call this device (pair)")
	pairRole := sub.String("role", "control", "role to pair the device into: view, control or admin (never more than the code carries)")
	pairTTL := sub.String("ttl", "", "how long the pairing code lives, e.g. 10m (pair-code)")
	sub.Usage = usage
	sub.Parse(rest)
	if sub.NArg() != 0 {
		usage()
		os.Exit(2)
	}

	// Pairing is how a device gets its credential, and it only exists
	// where TLS does: speak TLS whatever -tls says — auto when empty,
	// since a device with nothing to verify yet is exactly the point of
	// the call. The certificate that comes back is what is checked from
	// then on.
	if cmd == "pair" && !tlsutil.On(*useTLS) {
		*useTLS = "auto"
	}
	c, err := clientFor(*server, *useTLS, *identity)
	if err != nil {
		fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "host":
		h, err := c.Host(ctx)
		if err != nil {
			fatal(err)
		}
		printJSON(h)

	case "session":
		s, err := c.Session(ctx)
		if err != nil {
			fatal(err)
		}
		if s == nil {
			fmt.Fprintln(os.Stderr, "no session is running on "+*server)
			os.Exit(1)
		}
		printJSON(s)

	case "start":
		spec, err := readSpec(*specPath)
		if err != nil {
			fatal(err)
		}
		s, err := c.Start(ctx, spec, *replace)
		if err != nil {
			fatal(err)
		}
		printJSON(s)
		if !*waitLive {
			return
		}
		// Subscribe after the request: a new subscriber is told the
		// session's current state, so nothing can be missed — and the
		// state of the session that was just replaced cannot be mistaken
		// for this one's.
		events, unsub, err := c.Events(ctx)
		if err != nil {
			fatal(err)
		}
		defer unsub()
		if code := waitSession(ctx, events, c); code != 0 {
			os.Exit(code)
		}

	case "events":
		events, unsub, err := c.Events(ctx)
		if err != nil {
			fatal(err)
		}
		defer unsub()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					return
				}
				printJSON(ev)
				if !*follow && (ev.State == api.StateLive || ev.State == api.StateLost) {
					return
				}
			}
		}

	case "terminate":
		if err := c.Terminate(ctx); err != nil {
			fatal(err)
		}
		fmt.Println("terminated: the session is gone; the daemon keeps running")

	case "pair":
		if *pairName == "" || *pairCode == "" {
			fatal(fmt.Errorf("pair needs -name <device> and -code <pairing code>"))
		}
		// The key is generated here and never sent: the daemon signs a
		// request for the matching public key.
		keyPEM, csrPEM, err := auth.NewIdentity(*pairName)
		if err != nil {
			fatal(err)
		}
		id, err := c.Pair(ctx, *pairCode, *pairName, *pairRole, keyPEM, csrPEM)
		if err != nil {
			fatal(err)
		}
		dir := auth.DefaultIdentityDir(*pairName)
		if err := id.Save(dir); err != nil {
			fatal(err)
		}
		fmt.Printf("paired %q as %s\n", *pairName, id.Role)
		fmt.Println("credential:", dir)

	case "pair-code":
		resp, err := c.PairCode(ctx, *pairRole, *pairTTL)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("pairing code for %s: %s\n", resp.Role, resp.Code)
		fmt.Println("expires:", resp.Expires.Format(time.RFC3339))

	case "clients":
		list, err := c.Clients(ctx)
		if err != nil {
			fatal(err)
		}
		if len(list) == 0 {
			fmt.Println("no devices are paired")
			return
		}
		for _, cl := range list {
			state := "paired"
			if cl.Revoked {
				state = "REVOKED"
			}
			fmt.Printf("%-16s %-8s %s  %s\n", cl.Name, cl.Role, state,
				cl.PairedAt.Format("2006-01-02 15:04"))
		}

	case "revoke":
		if *pairName == "" {
			fatal(fmt.Errorf("revoke needs -name <device>"))
		}
		if err := c.Revoke(ctx, *pairName); err != nil {
			fatal(err)
		}
		fmt.Printf("revoked %q: it can no longer connect\n", *pairName)

	default:
		fmt.Fprintf(os.Stderr, "remmote-ctl: unknown command %q\n", cmd)
		usage()
		os.Exit(2)
	}
}

// waitSession follows events until the session is live (0) or has failed
// (1) — and a failure is reported with the daemon's own reason, so
// nobody has to go and read its log.
func waitSession(ctx context.Context, events <-chan api.Event, c *api.Client) int {
	for {
		select {
		case <-ctx.Done():
			return 1
		case ev, ok := <-events:
			if !ok {
				return 1
			}
			switch ev.State {
			case api.StateLive:
				return 0
			case api.StateLost:
				if s, err := c.Session(ctx); err == nil && s != nil && s.Error != "" {
					fmt.Fprintln(os.Stderr, "remmote-ctl: the session failed:", s.Error)
				}
				return 1
			}
		}
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: remmote-ctl [-server host:port] [-tls value] [-identity name] <command> [flags]")
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  host | session | events [-follow] | terminate")
	fmt.Fprintln(os.Stderr, "  start [-spec file|-] [-wait] [-replace]")
	fmt.Fprintln(os.Stderr, "  pair -name <device> -code <code>")
	fmt.Fprintln(os.Stderr, "  pair-code -role view|control|admin [-ttl 10m]   (admin)")
	fmt.Fprintln(os.Stderr, "  clients | revoke -name <device>                 (admin)")
}

// readSpec reads a SessionSpec from a JSON file, or stdin with "-".
func readSpec(path string) (api.SessionSpec, error) {
	var spec api.SessionSpec
	var raw []byte
	var err error
	if path == "" || path == "-" {
		raw, err = io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return spec, err
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return spec, fmt.Errorf("bad session spec: %w", err)
	}
	return spec, nil
}

// clientFor builds the control client: as a paired device when an
// identity is named, and with -tls otherwise (plain on loopback).
func clientFor(server, tlsValue, identity string) (*api.Client, error) {
	if identity != "" {
		id, err := auth.LoadIdentity(auth.DefaultIdentityDir(identity))
		if err != nil {
			return nil, err
		}
		return api.NewClientIdentity(server, id)
	}
	cfg, err := clientTLS(tlsValue)
	if err != nil {
		return nil, err
	}
	return api.NewClient(server, cfg), nil
}

// clientTLS turns a -tls value into a client config (nil = plaintext).
func clientTLS(value string) (*tls.Config, error) {
	if !tlsutil.On(value) {
		return nil, nil
	}
	return tlsutil.ClientConfig(value)
}

func printJSON(v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fatal(err)
	}
	fmt.Println(string(b))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "remmote-ctl:", err)
	os.Exit(1)
}
