#!/usr/bin/env bash
# remmote integration test: paint a known color on a throwaway display,
# stream it through remmote-server → remmote-client, and assert the
# snapshot the client saves matches the painted color.
#
# Display selection: Xvfb :990 if available, else an existing :50 the
# user owns, else SKIP (exit 77).
#
# NOTE on the :50 fallback: it is a live desktop. A screen locker
# engaging mid-test (or any window the user opens) covers the painted
# window and fails the color assertion. For deterministic runs install
# Xvfb: sudo apt install xvfb
set -euo pipefail
cd "$(dirname "$0")/.."

PORT=17677
COLOR='#20c040'
SNAP=$(mktemp /tmp/remmote-it-XXXXXX.png)
LOG=$(mktemp /tmp/remmote-it-XXXXXX.log)
CODEC="${1:-hybrid}"

XVFB_PID=""
SRV_PID=""
FILL_PID=""

cleanup() {
	for pid in "${SRV_PID:-}" "${FILL_PID:-}" "${XVFB_PID:-}"; do
		[ -n "$pid" ] && kill "$pid" 2>/dev/null || true
	done
	rm -f "$SNAP" "$LOG"
}
trap cleanup EXIT

pick_display() {
	if command -v Xvfb >/dev/null 2>&1; then
		Xvfb :990 -screen 0 1280x800x24 >/dev/null 2>&1 &
		XVFB_PID=$!
		sleep 0.7
		echo ":990"
	elif command -v xdpyinfo >/dev/null 2>&1 && DISPLAY=:50 xdpyinfo >/dev/null 2>&1; then
		echo ":50"
	else
		return 1
	fi
}

DISP=$(pick_display) || {
	echo "SKIP: no Xvfb and no accessible :50 — install xvfb for hermetic tests"
	exit 77
}
echo "== using display $DISP (codec: $CODEC)"

if [ "$DISP" = ":50" ]; then
	export XAUTHORITY="${XAUTHORITY:-$HOME/.Xauthority}"
fi

SHM_BEFORE=$(ipcs -m 2>/dev/null | awk -v u="$USER" '$3==u' | wc -l)

echo "== building"
if [ "$CODEC" = "webp" ]; then
	make -s build-webp
else
	make -s build
fi
SRV=bin/remmote-server
CLI=bin/remmote-client

echo "== painting $COLOR on $DISP"
go run ./internal/testfill -display "$DISP" -color "$COLOR" -hold 25s &
FILL_PID=$!
sleep 1.5

echo "== starting server ($CODEC)"
"$SRV" -display "$DISP" -listen "127.0.0.1:$PORT" -codec "$CODEC" -v >"$LOG" 2>&1 &
SRV_PID=$!
sleep 1

echo "== client snapshot"
"$CLI" -server "127.0.0.1:$PORT" -once -snapshot "$SNAP"

echo "== verifying snapshot"
go run ./internal/testfill -check "$SNAP" -expect "$COLOR"

kill "$SRV_PID" "$FILL_PID" 2>/dev/null || true
wait "$SRV_PID" 2>/dev/null || true
SRV_PID=""
FILL_PID=""

sleep 0.5
SHM_AFTER=$(ipcs -m 2>/dev/null | awk -v u="$USER" '$3==u' | wc -l)
if [ "$SHM_AFTER" -gt "$SHM_BEFORE" ]; then
	echo "FAIL: shared memory segments leaked ($SHM_BEFORE → $SHM_AFTER)"
	exit 1
fi

grep -q "client connected" "$LOG" || { echo "FAIL: no client-connect in server log"; exit 1; }

echo "PASS: server→client pipeline verified on $DISP (codec $CODEC, no SHM leaks)"
