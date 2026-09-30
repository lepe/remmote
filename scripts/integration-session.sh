#!/usr/bin/env bash
# Session lifecycle integration test: a daemon creates the display it
# shares, runs the application it is asked to run on it, and gives all of
# it back when the session is terminated.
#
#   * create: a fresh Xvfb display of the size asked, guarded by its own
#     cookie (which is also what the viewer's stream connects with)
#   * app:    a client-launched application on that display, allowed with
#     -allow-exec and refused without it
#   * attach: the viewer sees the application's window, painted on a
#     display that did not exist when the daemon started
#   * detach: the display and the application keep running without a
#     viewer
#   * replace: the old display and application are freed, not leaked
#   * terminate: the display is gone, the application is dead and the
#     service has stopped
set -euo pipefail
cd "$(dirname "$0")/.."

PORT=17770
COLOR='#20c040'
SNAP=$(mktemp /tmp/remmote-session-XXXXXX.png)
LOG=$(mktemp /tmp/remmote-session-XXXXXX.log)
SPEC=$(mktemp -d)
BIN=$(mktemp -d)

SRV_PID=""
cleanup() {
	if [ -n "${SRV_PID:-}" ]; then
		kill "$SRV_PID" 2>/dev/null || true
		wait "$SRV_PID" 2>/dev/null || true
	fi
	rm -f "$SNAP" "$LOG"
	rm -rf "$SPEC" "$BIN"
}
trap cleanup EXIT

# has <text> <pattern>: substring match without a pipe. A piped grep -q
# exits as soon as it matches, the writer dies on SIGPIPE, and pipefail
# turns that into a failure — a check that lies about its own result.
has() {
	case "$1" in
	*"$2"*) return 0 ;;
	*) return 1 ;;
	esac
}

command -v Xvfb >/dev/null 2>&1 || { echo "SKIP: no Xvfb"; exit 77; }

echo "== building"
make -s build
go build -o "$BIN/remmote-app" ./internal/testfill

APP="$BIN/remmote-app -size 200x150 -pos 40+30 -wm-class remmote-app:RemmoteApp -color $COLOR -hold 60s"
cat >"$SPEC/app.json" <<EOF
{
  "source": "app",
  "display": {"kind": "create", "create": {"server": "xvfb", "size": "800x600"}},
  "app": {"command": "$APP"},
  "stream": {"codec": "jpeg"}
}
EOF
cat >"$SPEC/desktop.json" <<EOF
{
  "source": "desktop",
  "display": {"kind": "create", "create": {"server": "xvfb", "size": "640x480"}}
}
EOF
cat >"$SPEC/badwm.json" <<EOF
{
  "source": "desktop",
  "display": {"kind": "create", "create": {"server": "xvfb", "wm": "no-such-window-manager"}}
}
EOF

ctl() { ./bin/remmote-ctl -server "127.0.0.1:$PORT" "$@"; }

# session_display: the number of the display a live session is sharing.
session_display() {
	ctl session | grep -oE '"display": ":[0-9]+"' | head -1 | grep -oE '[0-9]+' || true
}
# session_number: a numeric field of the session record (e.g. appPid).
session_number() {
	ctl session | grep -oE "\"$1\": [0-9]+" | head -1 | grep -oE '[0-9]+$' || true
}
# cookie_path: the cookie the daemon made for its display (unique per
# session, so the log is where its name is recorded).
cookie_path() {
	grep -oE 'auth=[^ ]+\.Xauthority' "$LOG" | tail -1 | cut -d= -f2 || true
}

echo "== starting an idle daemon (client launches allowed for remmote-app)"
./bin/remmote-server -idle -listen "127.0.0.1:$PORT" -allow-exec remmote-app -v >"$LOG" 2>&1 &
SRV_PID=$!
UP=""
for _ in $(seq 1 50); do
	ctl host >/dev/null 2>&1 && { UP=1; break; }
	sleep 0.2
done
[ -n "$UP" ] || { echo "FAIL: daemon never answered"; grep -v authority "$LOG" | tail -5; exit 1; }
HOST=$(ctl host)
has "$HOST" '"canCreate": true' || { echo "FAIL: host cannot create displays"; echo "$HOST"; exit 1; }
has "$HOST" '"allowExec": true' || { echo "FAIL: host does not report its exec allowance"; echo "$HOST"; exit 1; }

echo "== a window manager that is not installed is refused by name"
if ctl start -spec "$SPEC/badwm.json" >"$SPEC/out" 2>&1; then
	echo "FAIL: an unknown window manager was accepted"
	exit 1
fi
grep -q "not installed" "$SPEC/out" || { echo "FAIL: the refusal did not name the fix"; cat "$SPEC/out"; exit 1; }

echo "== an application on a display the daemon creates"
ctl start -spec "$SPEC/app.json" -wait || {
	echo "FAIL: the session did not come up"
	ctl session || true
	echo "--- daemon log:"
	grep -v authority "$LOG" | tail -40
	exit 1
}
SESSION=$(ctl session)
has "$SESSION" '"state": "live"' || { echo "FAIL: the session is not live"; echo "$SESSION"; exit 1; }
NUM=$(session_display)
[ -n "$NUM" ] || { echo "FAIL: the session does not name its display"; echo "$SESSION"; exit 1; }
echo "   display :$NUM created"
[ -S "/tmp/.X11-unix/X$NUM" ] || { echo "FAIL: the created display is not up"; exit 1; }
AUTH=$(cookie_path)
if [ -z "$AUTH" ] || [ ! -e "$AUTH" ]; then
	echo "FAIL: no cookie was made for :$NUM"
	exit 1
fi
APP_PID=$(session_number appPid)
[ -n "$APP_PID" ] || { echo "FAIL: the session does not report its application"; echo "$SESSION"; exit 1; }
kill -0 "$APP_PID" 2>/dev/null || { echo "FAIL: the reported application is not running"; exit 1; }

echo "== viewer attaches (and the cookie is what makes that possible)"
./bin/remmote-client -server "127.0.0.1:$PORT" -once -snapshot "$SNAP"
go run ./internal/testfill -check "$SNAP" -expect "$COLOR" -min-match 60

echo "== detach: display and application outlive the viewer"
kill -0 "$APP_PID" 2>/dev/null || { echo "FAIL: the application died with the viewer"; exit 1; }
[ -S "/tmp/.X11-unix/X$NUM" ] || { echo "FAIL: the display died with the viewer"; exit 1; }

echo "== replace frees the display and the application it was using"
ctl start -spec "$SPEC/desktop.json" -replace -wait || {
	echo "FAIL: the replacement did not come up"
	ctl session || true
	exit 1
}
SESSION=$(ctl session)
has "$SESSION" '"state": "live"' || { echo "FAIL: the replacement is not live"; echo "$SESSION"; exit 1; }
NEWNUM=$(session_display)
echo "   display :$NEWNUM created"
if kill -0 "$APP_PID" 2>/dev/null; then
	echo "FAIL: the old application survived its session"
	exit 1
fi
grep -q "msg=\"display stopped\" display=:$NUM" "$LOG" || {
	echo "FAIL: the old display :$NUM was not stopped"
	grep -v authority "$LOG" | tail -6
	exit 1
}
if [ ! -S "/tmp/.X11-unix/X$NEWNUM" ]; then
	echo "FAIL: the replacement's display is not up"
	exit 1
fi

echo "== the created display is the size that was asked for"
has "$SESSION" '"width": 640' || { echo "FAIL: 640x480 was not honoured"; echo "$SESSION"; exit 1; }
has "$SESSION" '"height": 480' || { echo "FAIL: 640x480 was not honoured"; echo "$SESSION"; exit 1; }

echo "== terminate: display gone, service gone"
ctl terminate
for _ in $(seq 1 50); do
	kill -0 "$SRV_PID" 2>/dev/null || break
	sleep 0.2
done
if kill -0 "$SRV_PID" 2>/dev/null; then
	echo "FAIL: the daemon kept running after terminate"
	exit 1
fi
SRV_PID=""
if [ -S "/tmp/.X11-unix/X$NEWNUM" ]; then
	echo "FAIL: the created display :$NEWNUM leaked"
	exit 1
fi
NEWAUTH=$(cookie_path)
if [ -n "$NEWAUTH" ] && [ -e "$NEWAUTH" ]; then
	echo "FAIL: the cookie for :$NEWNUM leaked ($NEWAUTH)"
	exit 1
fi

echo "PASS: session lifecycle verified (create, app, attach, detach, replace, terminate)"
