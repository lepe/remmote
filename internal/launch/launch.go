// Package launch spawns the shared application on the target display,
// finds the window it opens, and owns the process lifetime (kill the
// whole process group on shutdown).
package launch

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jezek/xgb/xproto"

	"github.com/lepe/remmote/internal/xconn"
	"github.com/lepe/remmote/internal/xwin"
)

// Proc is the spawned application process group.
type Proc struct {
	cmd  *exec.Cmd
	log  *slog.Logger
	mu   sync.Mutex
	done chan struct{}
	err  error
}

// Start runs the command line (quote-aware split: 'single' and "double"
// quotes group words) as a new process group on the given display. The
// child inherits the environment except DISPLAY.
func Start(cmdline, display string, log *slog.Logger) (*Proc, error) {
	argv := splitArgv(cmdline)
	if len(argv) == 0 {
		return nil, syscall.EINVAL
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "DISPLAY=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "DISPLAY="+xconn.DisplayString(display))
	cmd.Env = env

	p := &Proc{cmd: cmd, log: log, done: make(chan struct{})}
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go p.pipe(stdout, "out")
	go p.pipe(stderr, "err")
	go func() {
		p.mu.Lock()
		p.err = cmd.Wait()
		p.mu.Unlock()
		close(p.done)
	}()
	return p, nil
}

func (p *Proc) pipe(r io.Reader, tag string) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		p.log.Info("app", "stream", tag, "line", sc.Text())
	}
}

// Pid is the process group id (Setpgid makes them equal).
func (p *Proc) Pid() int { return p.cmd.Process.Pid }

// Done closes when the process exits.
func (p *Proc) Done() <-chan struct{} { return p.done }

// Err is the process exit error (after Done).
func (p *Proc) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// Kill terminates the whole process group: SIGTERM, then SIGKILL after
// grace. Idempotent.
func (p *Proc) Kill() {
	p.kill(2 * time.Second)
}

func (p *Proc) kill(grace time.Duration) {
	pgid := -p.Pid()
	if err := syscall.Kill(pgid, syscall.SIGTERM); err != nil {
		return // already gone
	}
	select {
	case <-p.done:
		// The leader can exit while descendants ignore SIGTERM.
		_ = syscall.Kill(pgid, syscall.SIGKILL)
	case <-time.After(grace):
		_ = syscall.Kill(pgid, syscall.SIGKILL)
		<-p.done // reap before the server itself exits
	}
}

// BootstrapResult reports the seed detection outcome.
type BootstrapResult struct {
	Proc  *Proc
	Win   xproto.Window // 0 when no window was found
	Class string        // res_class of the seed window ("" unknown)
}

// Bootstrap snapshots the existing windows, spawns the app, and polls
// for its window for up to 10 s. Detection prefers _NET_WM_PID matches,
// then WM_CLASS/WM_INSTANCE against the executable basename. On
// WM-managed displays candidates carry WM_STATE; on bare displays (Xvfb,
// no WM) the fallback considers new viewable direct children of root.
// The poll outlives an early process exit, because a single-instance app
// hands off to an existing process and its window appears afterwards.
// The returned result always carries the Proc (server policy: stay up
// even without a window); Win is 0 when nothing appeared.
func Bootstrap(ctx context.Context, xq *xwin.Client, cmdline, display string, log *slog.Logger) (*BootstrapResult, error) {
	known := make(map[xproto.Window]bool)
	for _, w := range xq.FindClientWindows() {
		known[w] = true
	}
	for _, w := range xq.Stack() {
		known[w] = true
	}

	proc, err := Start(cmdline, display, log)
	if err != nil {
		return nil, err
	}
	res := &BootstrapResult{Proc: proc}

	base := execBasename(cmdline)
	pid := uint32(proc.Pid())
	deadline := time.Now().Add(10 * time.Second)
	var guess xproto.Window
	var guessClass string
	guessAt := time.Time{}
	// A single-instance application (most KDE/GNOME apps — konsole, kate,
	// dolphin) hands the request to an already-running process and exits
	// at once, so its window appears only *after* proc.Done(). Returning
	// there left the server serving a permanent 1×1 canvas with nothing
	// but a warning to explain it. Keep scanning to the deadline instead:
	// the class heuristic can still find the window, and a genuine launch
	// failure ends exactly as before, just after the full wait.
	procExited := false
	for time.Now().Before(deadline) {
		if procExited {
			select {
			case <-ctx.Done():
				return res, nil
			case <-time.After(150 * time.Millisecond):
			}
		} else {
			select {
			case <-ctx.Done():
				return res, nil
			case <-proc.Done():
				procExited = true
				log.Warn("application exited before opening a window; still scanning for one",
					"pid", pid, "err", proc.Err())
			case <-time.After(150 * time.Millisecond):
			}
		}

		candidates := xq.FindClientWindows()
		if len(candidates) == 0 {
			// No WM (bare Xvfb): fall back to new viewable root children.
			candidates = xq.Stack()
		}
		for _, w := range candidates {
			if known[w] {
				continue
			}
			if wpid, ok := xq.PropUint32(w, "_NET_WM_PID"); ok && wpid == pid {
				_, class, _ := xq.WMClass(w)
				res.Win, res.Class = w, class
				return res, nil
			}
			if guess == 0 {
				instance, class, _ := xq.WMClass(w)
				if base != "" && (strings.EqualFold(class, base) || strings.EqualFold(instance, base)) {
					guess, guessClass, guessAt = w, class, time.Now()
				}
			}
		}
		// A class guess only wins after 4 s — a PID-tagged window is
		// strictly better evidence.
		if guess != 0 && time.Since(guessAt) > 4*time.Second {
			res.Win, res.Class = guess, guessClass
			return res, nil
		}
	}
	if guess != 0 {
		res.Win, res.Class = guess, guessClass
	}
	return res, nil
}

func execBasename(cmdline string) string {
	fields := splitArgv(cmdline)
	if len(fields) == 0 {
		return ""
	}
	base := fields[0]
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	return base
}

// splitArgv splits a command line into words, honoring single and double
// quotes for grouping (no escape sequences).
func splitArgv(s string) []string {
	var args []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	flush := func() {
		if cur.Len() > 0 {
			args = append(args, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '\'' && !inDouble:
			inSingle = !inSingle
		case r == '"' && !inSingle:
			inDouble = !inDouble
		case (r == ' ' || r == '\t') && !inSingle && !inDouble:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return args
}
