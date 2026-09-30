#!/usr/bin/env bash
# Control API integration test: a daemon that shares nothing until a
# client says what to share, and a session that outlives every viewer.
#
# Covers the semantics the daemon exists for:
#   * -idle: nothing shared until a client posts a SessionSpec
#   * start + attach: the viewer's snapshot matches what was painted
#   * detach ≠ terminate: closing the viewer leaves the session running
#   * reattach: a second viewer gets the same session
#   * a running session is never clobbered (409) unless replace is asked
#   * replace stops the old session and starts the new spec
#   * launching an application is refused unless the daemon allows it
#   * terminate: the session is gone AND the service exits
#   * events (SSE) reports the session's state changes
set -euo pipefail
cd "$(dirname "$0")/.."

PORT=17760
COLOR='#20c040'
DISP=:994
SNAP=$(mktemp /tmp/remmote-ctl-XXXXXX.png)
LOG=$(mktemp /tmp/remmote-ctl-XXXXXX.log)
SPEC=$(mktemp -d)

XVFB_PID=""
SRV_PID=""
FILL_PID=""
cleanup() {
	for pid in "${SRV_PID:-}" "${FILL_PID:-}" "${XVFB_PID:-}"; do
		[ -n "$pid" ] && kill "$pid" 2>/dev/null || true
	done
	rm -f "$SNAP" "$LOG"
	rm -rf "$SPEC"
}
trap cleanup EXIT

command -v Xvfb >/dev/null 2>&1 || { echo "SKIP: no Xvfb"; exit 77; }

echo "== building"
make -s build

cat >"$SPEC/desktop.json" <<EOF
{
  "source": "desktop",
  "display": {"kind": "existing", "name": "$DISP"},
  "stream": {"codec": "jpeg", "quality": 80}
}
EOF
cat >"$SPEC/replaced.json" <<EOF
{
  "source": "desktop",
  "display": {"kind": "existing", "name": "$DISP"},
  "stream": {"codec": "zraw"}
}
EOF
cat >"$SPEC/app.json" <<EOF
{
  "source": "app",
  "display": {"kind": "existing", "name": "$DISP"},
  "app": {"command": "xcalc"}
}
EOF

ctl() { ./bin/remmote-ctl -server "127.0.0.1:$PORT" "$@"; }

echo "== starting Xvfb $DISP"
Xvfb $DISP -screen 0 1280x800x24 >/dev/null 2>&1 &
XVFB_PID=$!
sleep 0.8

echo "== painting $COLOR on $DISP"
go run ./internal/testfill -display "$DISP" -color "$COLOR" -hold 90s >/dev/null 2>&1 &
FILL_PID=$!
sleep 1.5

echo "== starting an idle daemon"
./bin/remmote-server -idle -listen "127.0.0.1:$PORT" -v >"$LOG" 2>&1 &
SRV_PID=$!
UP=""
for _ in $(seq 1 50); do
	ctl host >/dev/null 2>&1 && { UP=1; break; }
	sleep 0.2
done
[ -n "$UP" ] || { echo "FAIL: daemon never answered"; grep -v authority "$LOG" | tail -5; exit 1; }
ctl host | grep -q '"protoVersion": 4' || { echo "FAIL: host report lacks the protocol version"; exit 1; }

echo "== an idle daemon has no session"
if ctl session >/dev/null 2>&1; then
	echo "FAIL: an idle daemon reported a session"
	exit 1
fi

echo "== launching an application is refused by default"
if ctl start -spec "$SPEC/app.json" >"$SPEC/out" 2>&1; then
	echo "FAIL: a client was allowed to launch an application"
	cat "$SPEC/out"
	exit 1
fi
grep -q "does not allow launching" "$SPEC/out" || {
	echo "FAIL: the refusal did not name the fix"
	cat "$SPEC/out"
	exit 1
}

echo "== starting a desktop session over the API"
ctl start -spec "$SPEC/desktop.json" -wait
ctl session | grep -q '"state": "live"' || { echo "FAIL: the session is not live"; exit 1; }

echo "== viewer 1 attaches"
rm -f "$SNAP"
./bin/remmote-client -server "127.0.0.1:$PORT" -once -snapshot "$SNAP"
go run ./internal/testfill -check "$SNAP" -expect "$COLOR"

echo "== the session outlived its viewer (detach is not terminate)"
ctl session | grep -q '"state": "live"' || {
	echo "FAIL: closing the viewer ended the session"
	exit 1
}

echo "== viewer 2 reattaches to the same session"
rm -f "$SNAP"
./bin/remmote-client -server "127.0.0.1:$PORT" -once -snapshot "$SNAP"
go run ./internal/testfill -check "$SNAP" -expect "$COLOR"

echo "== a running session is not clobbered"
if ctl start -spec "$SPEC/replaced.json" >"$SPEC/out" 2>&1; then
	echo "FAIL: a second start replaced the session without being asked"
	exit 1
fi
grep -q "already running" "$SPEC/out" || { echo "FAIL: refusal did not say why"; cat "$SPEC/out"; exit 1; }

echo "== events (SSE) report the session's state"
ctl events >"$SPEC/events" 2>/dev/null &
EVENTS_PID=$!
sleep 0.5

echo "== replace: terminate and start mine"
ctl start -spec "$SPEC/replaced.json" -replace -wait
SESSION=$(ctl session)
echo "$SESSION" | grep -q '"state": "live"' || { echo "FAIL: the replacement is not live"; exit 1; }
echo "$SESSION" | grep -q '"codec": "zraw"' || {
	echo "FAIL: the replacement spec is not the one in force"
	echo "$SESSION"
	exit 1
}
kill "$EVENTS_PID" 2>/dev/null || true
grep -q '"state"' "$SPEC/events" || {
	echo "FAIL: the event stream carried no state change"
	cat "$SPEC/events"
	exit 1
}

echo "== terminate stops the session and the service"
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

echo "PASS: control API verified (idle, start, attach, detach, reattach, refuse, replace, events, terminate)"
