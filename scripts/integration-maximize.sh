#!/usr/bin/env bash
# -maximize integration test: with -exec, the shared window must be
# maximized onto the host screen once it becomes viewable. Covers both
# paths in xwin.Maximize — the EWMH request under a window manager, and
# the plain ConfigureWindow fallback on a bare Xvfb (no WM to honour it).
#
# The window is only 200x150 to start with, so a snapshot that fills the
# 800x600 screen is proof the maximize actually happened.
set -euo pipefail
cd "$(dirname "$0")/.."

PORT=17730
DISP=:991
BIN=$(mktemp -d)
LOG=$(mktemp /tmp/remmote-max-XXXXXX.log)
SNAP=$(mktemp /tmp/remmote-max-XXXXXX.png)

XVFB_PID=""
SRV_PID=""
WM_PID=""
cleanup() {
	for pid in "${SRV_PID:-}" "${WM_PID:-}" "${XVFB_PID:-}"; do
		[ -n "$pid" ] && kill "$pid" 2>/dev/null || true
	done
	pkill -x remmote-app 2>/dev/null || true
	rm -f "$LOG" "$SNAP"
	rm -rf "$BIN"
}
trap cleanup EXIT

command -v Xvfb >/dev/null 2>&1 || { echo "SKIP: no Xvfb"; exit 77; }

echo "== building"
make -s build
go build -o "$BIN/remmote-app" ./internal/testfill

echo "== starting Xvfb $DISP (800x600)"
Xvfb $DISP -screen 0 800x600x24 >/dev/null 2>&1 &
XVFB_PID=$!
sleep 0.8

run_case() {
	local name="$1"
	local mode="$2"
	rm -f "$LOG" "$SNAP"
	PORT=$((PORT + 1))
	./bin/remmote-server -display $DISP -listen "127.0.0.1:$PORT" -maximize \
		-exec "$BIN/remmote-app -size 200x150 -pos 40+30 \
			-wm-class remmote-app:RemmoteApp -color '#20c040' -hold 40s" \
		-v >"$LOG" 2>&1 &
	SRV_PID=$!
	for _ in $(seq 1 100); do
		grep -q "listening" "$LOG" && break
		sleep 0.2
	done
	grep -q "listening" "$LOG" || {
		echo "FAIL [$name]: server never listened"
		grep -v authority "$LOG" | tail -6
		exit 1
	}
	# Asserting the path matters: without a WM the fallback resize would
	# pass this test even if the EWMH request were broken.
	grep -q "maximized window.*mode=$mode" "$LOG" || {
		echo "FAIL [$name]: expected the $mode maximize path"
		grep -v authority "$LOG" | tail -8
		exit 1
	}

	DISPLAY=$DISP timeout 20 ./bin/remmote-client -server "127.0.0.1:$PORT" \
		-once -snapshot "$SNAP" >/dev/null 2>&1
	[ -f "$SNAP" ] || { echo "FAIL [$name]: no snapshot written"; exit 1; }

	go run ./internal/testfill -check "$SNAP" -expect '#20c040' -min-match 85
	python3 - "$SNAP" <<'PY'
import struct, sys
d = open(sys.argv[1], 'rb').read()
w, h = struct.unpack('>II', d[16:24])
# The screen is 800x600; a maximized client window is the screen minus at
# most the WM's decorations, and the canvas is quantized outward, so the
# result is never smaller than 95% of the screen.
if w < 760 or h < 570:
    print(f"FAIL: snapshot {w}x{h}, want >= 760x570 (800x600 screen maximized)")
    sys.exit(1)
print(f"OK: canvas {w}x{h} fills the 800x600 screen")
PY

	kill "$SRV_PID" 2>/dev/null || true
	wait "$SRV_PID" 2>/dev/null || true
	SRV_PID=""
	pkill -x remmote-app 2>/dev/null || true
	sleep 0.5
	echo "OK [$name]"
}

echo "== case 1: bare Xvfb — ConfigureWindow fallback"
run_case "no window manager" "resize"

if command -v openbox >/dev/null 2>&1; then
	echo "== case 2: under openbox — EWMH maximize"
	DISPLAY=$DISP openbox >/dev/null 2>&1 &
	WM_PID=$!
	sleep 1.5
	run_case "window manager" "ewmh"
else
	echo "SKIP: no openbox, EWMH path not covered"
fi

echo "PASS: -maximize fills the host screen"
