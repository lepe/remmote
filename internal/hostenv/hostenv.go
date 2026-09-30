// Package hostenv owns the X displays remmote shares.
//
// Two kinds. A display that already exists is opened, with whatever
// cookie guards it — including one made by scripts/start-xvfb.sh or
// start-xephyr.sh, whose cookie is picked up from the runtime directory.
// A display the daemon creates for one session (Xvfb headless, or Xephyr
// nested in another session) is started here and destroyed when the
// session ends, window manager and cookie included.
//
// This is the display, cookie and window-manager bookkeeping that
// scripts/lib/common.sh does for the helper scripts, in Go: the same
// rules (a free display number, a real readiness probe rather than "the
// socket exists", a cookie in the runtime directory, whatever window
// manager is installed) with none of the prompting.
package hostenv

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lepe/remmote/internal/xconn"
)

// The display numbers a created display may take, and the window
// managers worth offering — the same list the helper scripts prefer.
const (
	firstDisplay = 88
	lastDisplay  = 999

	defaultSize = "1280x800"
)

// wmCandidates is the window managers to look for, best first.
var wmCandidates = []string{
	"openbox", "fluxbox", "marco", "mutter", "xfwm4", "i3", "awesome",
	"blackbox", "icewm", "jwm", "pekwm", "matchbox-window-manager", "twm",
}

// Display is an X display ready to share.
type Display struct {
	// Name is the display, e.g. ":88".
	Name string
	// Auth is the Xauthority file guarding it ("" when it needs none).
	Auth string

	server *process // the X server; nil when the display already existed
	wm     *process // the window manager it runs, if any
	log    *slog.Logger

	mu           sync.Mutex
	closeRestore func() // puts the environment back (Close)
}

// CreateOptions describes a display to create for a session.
type CreateOptions struct {
	Server      string // "xvfb" (headless) or "xephyr" (nested in another session)
	Size        string // "1280x800"; empty means the default
	WM          string // window manager to run inside; "", "none" or "no" runs none
	HostDisplay string // "xephyr": the session its window opens on
}

// Available reports whether this machine can create displays at all —
// i.e. has an X server that can make one.
func Available() bool {
	return findBin("Xvfb") != "" || findBin("Xephyr") != ""
}

// WMs lists the window managers installed here, best first.
func WMs() []string {
	var out []string
	for _, wm := range wmCandidates {
		if findBin(wm) != "" {
			out = append(out, wm)
		}
	}
	return out
}

// HasWM reports whether a window manager is installed here.
func HasWM(name string) bool {
	return findBin(strings.TrimSpace(name)) != ""
}

// Displays lists the display numbers that have a socket up, ascending.
// Whether they can be opened is a separate question (see Open).
func Displays() []string {
	matches, _ := filepath.Glob("/tmp/.X11-unix/X*")
	var out []string
	for _, m := range matches {
		if n := strings.TrimPrefix(filepath.Base(m), "X"); isNumber(n) {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return atoi(out[i]) < atoi(out[j]) })
	return out
}

// Open prepares a display that already exists. A display guarded by a
// cookie — one the helper scripts made, say — has its cookie picked up
// from the runtime directory; anything else is used as it is.
func Open(name string, log *slog.Logger) (*Display, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	name = normalizeName(name)
	if name == "" {
		return nil, fmt.Errorf("hostenv: no display named")
	}
	if !socketUp(name) {
		return nil, fmt.Errorf("hostenv: display %s does not exist (nothing is listening on %s)", name, socketPath(name))
	}
	d := &Display{Name: name, log: log}
	if probe(name, "") {
		return d, nil
	}
	// The display answers to someone else's cookie: look for the one its
	// maker left behind, the way scripts/lib/common.sh does.
	for _, dir := range []string{os.Getenv("XDG_RUNTIME_DIR"), os.TempDir()} {
		if dir == "" {
			continue
		}
		for _, kind := range []string{"xvfb", "xephyr"} {
			file := filepath.Join(dir, fmt.Sprintf("remmote-%s-%s.Xauthority", kind, displayNum(name)))
			if _, err := os.Stat(file); err != nil {
				continue
			}
			if probe(name, file) {
				d.Auth = file
				log.Info("display cookie found", "display", name, "auth", file)
				return d, nil
			}
		}
	}
	return nil, fmt.Errorf("hostenv: display %s is up but would not admit us: no working cookie (its XAUTHORITY, or one from start-xvfb.sh / start-xephyr.sh)", name)
}

// Create starts a fresh display and returns it ready to share — and to be
// destroyed, with everything on it, when the session ends.
func Create(ctx context.Context, opts CreateOptions, log *slog.Logger) (*Display, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	server := strings.ToLower(strings.TrimSpace(opts.Server))
	switch server {
	case "xvfb", "xephyr":
	case "":
		server = "xvfb"
	default:
		return nil, fmt.Errorf("hostenv: cannot create a display with %q (want xvfb or xephyr)", opts.Server)
	}
	size := strings.TrimSpace(opts.Size)
	if size == "" {
		size = defaultSize
	}
	if !isSize(size) {
		return nil, fmt.Errorf("hostenv: size must look like 1280x800 (got %q)", opts.Size)
	}

	bin := findBin(map[string]string{"xvfb": "Xvfb", "xephyr": "Xephyr"}[server])
	if bin == "" {
		return nil, fmt.Errorf("hostenv: %s is not installed (Debian: apt install %s)", server, map[string]string{"xvfb": "xvfb", "xephyr": "xserver-xephyr"}[server])
	}
	wm := strings.TrimSpace(opts.WM)
	if wm != "" && !isNoneWord(wm) {
		if findBin(wm) == "" {
			return nil, fmt.Errorf("hostenv: window manager %q is not installed", wm)
		}
	} else {
		wm = ""
	}

	num, err := firstFree()
	if err != nil {
		return nil, err
	}
	name := ":" + num

	// A fresh cookie for a fresh display: the server is started with it
	// and it is what clients get in XAUTHORITY. The name carries a random
	// token — a display number can be free again while the session that
	// last used it is still shutting down, and one session's teardown
	// must never pull the cookie out from under another.
	authFile := filepath.Join(runtimeDir(),
		fmt.Sprintf("remmote-%s-%s-%s.Xauthority", server, num, token()))
	if err := writeAuthority(authFile, num); err != nil {
		return nil, err
	}

	d := &Display{Name: name, Auth: authFile, log: log}
	argv := []string{name}
	env := os.Environ()
	switch server {
	case "xvfb":
		argv = append(argv, "-screen", "0", size+"x24", "-nolisten", "tcp", "-auth", authFile)
	case "xephyr":
		host := normalizeName(opts.HostDisplay)
		if host == "" {
			host = os.Getenv("DISPLAY")
		}
		if host == "" {
			_ = os.Remove(authFile)
			return nil, fmt.Errorf("hostenv: xephyr needs a host session to open its window in (set hostDisplay)")
		}
		// Xephyr authenticates to the host session with the ordinary
		// environment; -auth and the cookie belong to the *nested*
		// display it serves.
		env = withDisplay(env, host)
		argv = append(argv, "-screen", size, "-resizeable", "-no-host-grab",
			"-nolisten", "tcp", "-auth", authFile)
	}

	p, err := start(server, env, log, append([]string{bin}, argv...)...)
	if err != nil {
		_ = os.Remove(authFile)
		return nil, fmt.Errorf("hostenv: start %s: %w", bin, err)
	}
	d.server = p

	if err := waitReady(ctx, name, authFile, p, log); err != nil {
		_ = d.Close()
		return nil, err
	}

	if wm != "" {
		wenv := withDisplay(os.Environ(), name)
		if wp, err := start("wm", withAuth(wenv, authFile), log, wm); err != nil {
			log.Warn("window manager did not start (continuing without one)", "wm", wm, "err", err)
		} else {
			d.wm = wp
			log.Info("window manager started", "wm", wm, "display", name)
		}
	}

	log.Info("display created", "display", name, "server", server, "size", size, "auth", authFile)
	return d, nil
}

// Activate installs the display's cookie in this process's environment,
// so every X client remmote starts here — capture, the clipboard watcher,
// a launched application — finds it. One session per daemon is what makes
// this sound; Close puts the environment back.
func (d *Display) Activate() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Auth == "" {
		return
	}
	d.closeRestore = swapAuth(d.Auth)
}

// Close stops what the display runs (its window manager, its X server),
// removes the cookie it made and restores the environment. A display that
// already existed is left running: remmote did not start it.
func (d *Display) Close() error {
	d.mu.Lock()
	restore := d.closeRestore
	d.closeRestore = nil
	d.mu.Unlock()
	if restore != nil {
		restore()
	}
	if d.wm != nil {
		d.wm.stop()
		d.wm = nil
	}
	if d.server != nil {
		d.server.stop()
		d.server = nil
	}
	if d.Auth != "" {
		_ = os.Remove(d.Auth)
		d.log.Info("display stopped", "display", d.Name)
	}
	return nil
}

// waitReady waits for the display to answer with its cookie: a server
// that is still starting — or one guarding a different cookie — must not
// look ready because its socket exists.
func waitReady(ctx context.Context, name, auth string, p *process, log *slog.Logger) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !p.alive() {
			return fmt.Errorf("hostenv: the X server for %s exited at once", name)
		}
		if socketUp(name) && probe(name, auth) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("hostenv: display %s did not come up", name)
}

// firstFree takes the lowest free display number in the range.
func firstFree() (string, error) {
	for n := firstDisplay; n <= lastDisplay; n++ {
		if !socketUp(":" + strconv.Itoa(n)) {
			return strconv.Itoa(n), nil
		}
	}
	return "", fmt.Errorf("hostenv: no free display number between :%d and :%d", firstDisplay, lastDisplay)
}

// probe reports whether a display answers — with this cookie when one is
// given. A real connection, not just a socket: that is the difference
// between "ready" and "still starting".
func probe(name, auth string) bool {
	restore := swapAuth(auth)
	defer restore()
	c, err := xconn.Dial(name)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// swapAuth installs auth as XAUTHORITY ("" leaves the environment alone)
// and returns the func that puts it back.
func swapAuth(auth string) func() {
	prev, had := os.LookupEnv("XAUTHORITY")
	if auth != "" {
		os.Setenv("XAUTHORITY", auth)
	}
	return func() {
		if had {
			os.Setenv("XAUTHORITY", prev)
		} else {
			os.Unsetenv("XAUTHORITY")
		}
	}
}

// token is a short random name component, so a cookie file can never be
// confused with — or removed by — another session's teardown.
func token() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// writeAuthority writes an Xauthority file granting access to a display:
// one FamilyWild entry with a fresh MIT-MAGIC-COOKIE-1, which both the X
// server (as -auth) and clients (as XAUTHORITY) accept. The format is
// X11/Xauth.h's: family u16, then u16-length-prefixed address, display
// number, protocol name and cookie, all big-endian.
func writeAuthority(path, num string) error {
	cookie := make([]byte, 16)
	if _, err := rand.Read(cookie); err != nil {
		return fmt.Errorf("hostenv: cookie: %w", err)
	}
	var b bytes.Buffer
	field := func(s string) {
		_ = binary.Write(&b, binary.BigEndian, uint16(len(s)))
		b.WriteString(s)
	}
	_ = binary.Write(&b, binary.BigEndian, uint16(0xffff)) // FamilyWild: any address
	field("")                                              // address
	field(num)                                             // display number, as xauth records it
	field("MIT-MAGIC-COOKIE-1")
	_ = binary.Write(&b, binary.BigEndian, uint16(len(cookie)))
	b.Write(cookie)
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		return fmt.Errorf("hostenv: write %s: %w", path, err)
	}
	return nil
}

// isNoneWord reports whether a window-manager choice means "run none".
func isNoneWord(wm string) bool {
	switch strings.ToLower(strings.TrimSpace(wm)) {
	case "", "none", "no", "off", "-":
		return true
	}
	return false
}

// process is a display's X server or window manager: its own process
// group, so stopping it takes the whole group with it.
type process struct {
	cmd  *exec.Cmd
	tag  string
	done chan struct{}
}

// start runs argv as a new process group, logging its output.
func start(tag string, env []string, log *slog.Logger, argv ...string) (*process, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("hostenv: nothing to start")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = env
	out, _ := cmd.StdoutPipe()
	errOut, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &process{cmd: cmd, tag: tag, done: make(chan struct{})}
	go p.pipe(out, log)
	go p.pipe(errOut, log)
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

func (p *process) pipe(r io.Reader, log *slog.Logger) {
	buf := make([]byte, 4096)
	var partial []byte
	for {
		n, err := r.Read(buf)
		partial = append(partial, buf[:n]...)
		for {
			i := bytes.IndexByte(partial, '\n')
			if i < 0 {
				break
			}
			log.Info(p.tag, "line", strings.TrimRight(string(partial[:i]), "\r"))
			partial = partial[i+1:]
		}
		if err != nil {
			if len(partial) > 0 {
				log.Info(p.tag, "line", string(partial))
			}
			return
		}
	}
}

// alive reports whether the process is still running.
func (p *process) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// stop terminates the process group and reaps it: politely first, then
// by force — what scripts/lib/common.sh's stop_pid does.
func (p *process) stop() {
	if p.cmd.Process == nil {
		return
	}
	pgid := -p.cmd.Process.Pid
	_ = syscall.Kill(pgid, syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
		_ = syscall.Kill(pgid, syscall.SIGKILL)
		<-p.done
	}
}

// runtimeDir is where display cookies and pid files go: the user's
// runtime directory when it is usable, the temporary directory otherwise.
func runtimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return d
		}
	}
	return os.TempDir()
}

// socketPath is where a display's unix socket lives.
func socketPath(name string) string {
	return "/tmp/.X11-unix/X" + displayNum(name)
}

// socketUp reports whether a display's socket exists.
func socketUp(name string) bool {
	st, err := os.Stat(socketPath(name))
	return err == nil && st.Mode()&os.ModeSocket != 0
}

// normalizeName turns "88", ":88" or "unix:88" into ":88"; "" stays "".
func normalizeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if i := strings.IndexByte(name, ':'); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.IndexByte(name, '.'); i >= 0 { // drop the screen
		name = name[:i]
	}
	if !isNumber(name) {
		return ""
	}
	return ":" + name
}

// displayNum is a display name's number ("88").
func displayNum(name string) string {
	return strings.TrimPrefix(normalizeName(name), ":")
}

// withDisplay returns env with DISPLAY set to name.
func withDisplay(env []string, name string) []string {
	return setEnv(env, "DISPLAY", name)
}

// withAuth returns env with XAUTHORITY set to auth (no-op when empty).
func withAuth(env []string, auth string) []string {
	if auth == "" {
		return env
	}
	return setEnv(env, "XAUTHORITY", auth)
}

// setEnv returns env with key=value replacing any earlier entry.
func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}
	return append(out, prefix+value)
}

// findBin looks a command up in PATH.
func findBin(name string) string {
	if name == "" {
		return ""
	}
	path, _ := exec.LookPath(name)
	return path
}

func isNumber(s string) bool {
	if s == "" {
		return false
	}
	_, err := strconv.Atoi(s)
	return err == nil
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// isSize reports whether s looks like "1280x800".
func isSize(s string) bool {
	w, h, ok := strings.Cut(strings.ToLower(s), "x")
	if !ok || !isNumber(strings.TrimSpace(w)) || !isNumber(strings.TrimSpace(h)) {
		return false
	}
	return atoi(strings.TrimSpace(w)) >= 160 && atoi(strings.TrimSpace(h)) >= 160
}
