#!/usr/bin/env bash
# Viewer-driven resize, end to end on Xvfb:
#
#   1. -exec window mode over the wire: a protocol client asks the server
#      for a 640x480 surface and the canvas must announce exactly that
#      (the 32 px canvas grid is solved for, so it is exact).
#   2. -exec window mode through the real client: resizing the viewer's
#      own window must size the application's window (server log).
#   3. whole-desktop without -resize-desktop: the request is dropped with
#      exactly one warning, and the server keeps streaming.
#   4. whole-desktop with -resize-desktop on Xvfb: RANDR can never resize
#      an Xvfb screen, so the refusal must be graceful — logged once,
#      streaming continues, the screen stays 800x600.
set -euo pipefail
cd "$(dirname "$0")/.."

PORT=17731
HOSTDISP=:994
VIEWDISP=:995
LOG=$(mktemp /tmp/remmote-resize-XXXXXX.log)
CLOG=$(mktemp /tmp/remmote-resize-XXXXXX.client)
OUT=$(mktemp /tmp/remmote-resize-XXXXXX.out)
BIN=$(mktemp -d)

SRV_PID=""
CLI_PID=""
XVFB_PIDS=()
cleanup() {
	for pid in "${CLI_PID:-}" "${SRV_PID:-}"; do
		[ -n "$pid" ] && kill "$pid" 2>/dev/null || true
	done
	for pid in "${XVFB_PIDS[@]:-}"; do
		[ -n "$pid" ] || continue
		kill "$pid" 2>/dev/null || true
		# Reap before returning: a display number is only free again
		# once its X server is really gone, and the next script in the
		# suite takes the same number.
		wait "$pid" 2>/dev/null || true
	done
	rm -f "$LOG" "$CLOG" "$OUT"
	rm -rf "$BIN"
}
trap cleanup EXIT

command -v Xvfb >/dev/null 2>&1 || { echo "SKIP: no Xvfb"; exit 77; }

echo "== building"
make -s build
go build -o "$BIN/testresize" ./internal/testresize
go build -o "$BIN/remmote-testfill" ./internal/testfill

echo "== starting Xvfb $HOSTDISP (host) and $VIEWDISP (viewer), 800x600"
Xvfb "$HOSTDISP" -screen 0 800x600x24 >/dev/null 2>&1 &
XVFB_PIDS+=($!)
Xvfb "$VIEWDISP" -screen 0 800x600x24 >/dev/null 2>&1 &
XVFB_PIDS+=($!)
sleep 0.8
for pid in "${XVFB_PIDS[@]}"; do
	kill -0 "$pid" 2>/dev/null || {
		echo "FAIL: an Xvfb did not start (is the display number still in use?)"
		exit 1
	}
done

wait_listening() {
	for _ in $(seq 1 100); do
		grep -q "listening" "$LOG" && return 0
		sleep 0.2
	done
	echo "FAIL: server never listened"
	grep -v authority "$LOG" | tail -5
	return 1
}

wait_log() { # wait_log <pattern> <tries>
	local pat=$1 tries=${2:-25} i
	for i in $(seq 1 "$tries"); do
		grep -Eq "$pat" "$LOG" && return 0
		sleep 0.2
	done
	echo "FAIL: server log never matched: $pat"
	grep -v authority "$LOG" | tail -8
	return 1
}

echo "== case 1: window mode, protocol resize to 640x480"
# 200x150 at +40+30: the canvas grid solve turns 640x480 into a window of
# 632x450 at the same place, whose quantized bounding box is exactly 640x480.
./bin/remmote-server -display "$HOSTDISP" -listen "127.0.0.1:$PORT" \
	-exec "$BIN/remmote-testfill -size 200x150 -pos 40+30 -color '#20c040' -hold 60s" -v >"$LOG" 2>&1 &
SRV_PID=$!
wait_listening

"$BIN/testresize" -server "127.0.0.1:$PORT" -size 640x480 -expect resize \
	-wait 6s | tee "$OUT"
grep -q '^RESIZE 640 480$' "$OUT" || {
	echo "FAIL: canvas is not exactly 640x480 (want RESIZE 640 480)"
	exit 1
}

echo "== case 2: window mode, the real client resizing its own window"
DISPLAY=$VIEWDISP ./bin/remmote-client -server "127.0.0.1:$PORT" \
	-no-clipboard >"$CLOG" 2>&1 &
CLI_PID=$!
"$BIN/testresize" -display "$VIEWDISP" -resize-window 'remmote — ' -size 700x500
# 700x500 → grid 704x512 → window 696x482 at +40+30 (see windowSizeForCanvas).
wait_log 'resized window for viewer.*width=696 height=482'
kill "$CLI_PID" 2>/dev/null || true
wait "$CLI_PID" 2>/dev/null || true
CLI_PID=""
kill "$SRV_PID" 2>/dev/null || true
wait "$SRV_PID" 2>/dev/null || true
SRV_PID=""

echo "== case 3: whole-desktop without -resize-desktop"
: >"$LOG"
./bin/remmote-server -display "$HOSTDISP" -listen "127.0.0.1:$PORT" -v >"$LOG" 2>&1 &
SRV_PID=$!
wait_listening
"$BIN/testresize" -server "127.0.0.1:$PORT" -size 640x480 -expect none \
	-keyframe -wait 3s
wait_log 'start the server with -resize-desktop'
[ "$(grep -c 'start the server with -resize-desktop' "$LOG")" = 1 ] || {
	echo "FAIL: the -resize-desktop warning was not logged exactly once"
	exit 1
}
kill "$SRV_PID" 2>/dev/null || true
wait "$SRV_PID" 2>/dev/null || true
SRV_PID=""

echo "== case 4: whole-desktop with -resize-desktop on Xvfb (RANDR refuses)"
: >"$LOG"
./bin/remmote-server -display "$HOSTDISP" -listen "127.0.0.1:$PORT" \
	-resize-desktop -v >"$LOG" 2>&1 &
SRV_PID=$!
wait_listening
"$BIN/testresize" -server "127.0.0.1:$PORT" -size 640x480 -expect none \
	-keyframe -wait 3s
wait_log 'refused to resize'
ROOT=$(DISPLAY=$HOSTDISP xwininfo -root 2>/dev/null | awk '/Width:/{w=$2} /Height:/{h=$2} END{print w"x"h}')
[ "$ROOT" = "800x600" ] || {
	echo "FAIL: Xvfb screen changed to $ROOT despite the refusal"
	exit 1
}
kill "$SRV_PID" 2>/dev/null || true
wait "$SRV_PID" 2>/dev/null || true
SRV_PID=""

echo "== case 5: window mode under a window manager (EWMH path)"
DISPLAY=$HOSTDISP openbox >/dev/null 2>&1 &
WM_PID=$!
sleep 0.5
: >"$LOG"
./bin/remmote-server -display "$HOSTDISP" -listen "127.0.0.1:$PORT" \
	-exec "$BIN/remmote-testfill -size 200x150 -pos 40+30 -color '#20c040' -hold 60s" -v >"$LOG" 2>&1 &
SRV_PID=$!
wait_listening
# openbox moves the window wherever it likes — the canvas solve is
# position-independent, and the request stays small enough to fit
# anywhere on an 800x600 screen (a centred window at x≈300 could not
# grow to 640 wide without being cut off, which the server would clamp).
# The size is a multiple of the 32 px canvas grid, so it must land exact.
"$BIN/testresize" -server "127.0.0.1:$PORT" -size 384x288 -expect resize \
	-wait 6s | tee "$OUT"
grep -q '^RESIZE 384 288$' "$OUT" || {
	echo "FAIL: EWMH resize did not land on exactly 384x288"
	exit 1
}
wait_log 'resized window for viewer.*mode=ewmh'
kill "$SRV_PID" 2>/dev/null || true
wait "$SRV_PID" 2>/dev/null || true
SRV_PID=""
kill "$WM_PID" 2>/dev/null || true

echo "PASS: viewer resize drives the window, and the desktop policy holds"
