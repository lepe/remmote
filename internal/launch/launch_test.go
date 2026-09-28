package launch

import (
	"bytes"
	"log/slog"
	"strings"
	"syscall"
	"testing"
	"time"
)

func testLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, nil))
}

// groupGone waits for the process group to disappear after Done (the
// kernel tears the group down shortly after the leader is reaped).
func groupGone(pgid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if syscall.Kill(-pgid, 0) != nil {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

func TestStartEnvAndKill(t *testing.T) {
	var buf bytes.Buffer
	p, err := Start("sh -c 'echo DISPLAY=$DISPLAY; sleep 30'", ":995", testLogger(&buf))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Kill()
	select {
	case <-p.Done():
		t.Fatal("process exited too early")
	case <-time.After(300 * time.Millisecond):
	}
	if got := p.Pid(); got <= 0 {
		t.Fatalf("pid = %d", got)
	}
	// Wait for the echo to land in the log buffer.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "DISPLAY=:995") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(buf.String(), "DISPLAY=:995") {
		t.Fatalf("child DISPLAY not set; log: %q", buf.String())
	}

	p.Kill()
	select {
	case <-p.Done():
	case <-time.After(4 * time.Second):
		t.Fatal("process group not killed within grace")
	}
	if !groupGone(p.Pid(), 2*time.Second) {
		t.Error("process group still alive after Kill")
	}
}

func TestKillReapsGrandchildren(t *testing.T) {
	var buf bytes.Buffer
	p, err := Start("sh -c 'sleep 100 & sleep 100'", ":995", testLogger(&buf))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // let both sleeps spawn
	p.kill(500 * time.Millisecond)
	select {
	case <-p.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("shell not killed")
	}
	// The grandchildren were in the same process group.
	if !groupGone(p.Pid(), 2*time.Second) {
		t.Error("grandchildren survived the group kill")
	}
}

func TestExecBasename(t *testing.T) {
	cases := map[string]string{
		"xcalc":          "xcalc",
		"/usr/bin/xcalc": "xcalc",
		"sh -c 'foo'":    "sh",
		"":               "",
	}
	for in, want := range cases {
		if got := execBasename(in); got != want {
			t.Errorf("execBasename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitArgv(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"xcalc", []string{"xcalc"}},
		{"sh -c 'echo hi'", []string{"sh", "-c", "echo hi"}},
		{`xterm -e "top -o %CPU"`, []string{"xterm", "-e", "top -o %CPU"}},
		{"  a\tb  ", []string{"a", "b"}},
		{"", nil},
	}
	for _, tc := range cases {
		got := splitArgv(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("splitArgv(%q) = %q, want %q", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitArgv(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}
