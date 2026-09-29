#!/usr/bin/env bash
# Delta-path integration test: run a real windowed client session, paint a
# second window onto the host mid-stream, and assert the client's canvas
# snapshot contains it at the exact position. This exercises the full
# update pipeline the keyframe tests skip:
#
#	host damage → RectUpdate (non-zero origin) → decode → Canvas.Composite
#	→ dirty-region blit → snapshot
#
# -refresh 1h disables the periodic keyframe ticker, so the mid-stream
# change can only arrive as a delta RectUpdate — a regression of the
# composite source-point bug (decoding 0-based images with the rect's
# canvas origin as the source point) drops or displaces the update and
# the region check fails.
#
# Two displays: the server captures :991 while the viewer renders on :992,
# so the viewer window never appears inside its own capture.
set -euo pipefail
cd "$(dirname "$0")/.."

PORT=17678
COLOR_A='#20c040'
COLOR_B='#e08040'
WIN_GEOM='200x150+400+300' # the mid-stream change, in screen coordinates
SNAP=$(mktemp /tmp/remmote-delta-XXXXXX.png)
LOG=$(mktemp /tmp/remmote-delta-XXXXXX.log)
CLOG=$(mktemp /tmp/remmote-delta-XXXXXX.client)
CODEC="${1:-hybrid}"
SCALE="${2:-1}"
UPSCALE="${3:-1}"
case "$SCALE" in 1|2|4) ;; *) echo "downscale must be 1, 2 or 4"; exit 2;; esac
case "$UPSCALE" in 1|2|4) ;; *) echo "upscale must be 1, 2 or 4"; exit 2;; esac
# The client's canvas is the wire size times -upscale, so a matching
# -upscale puts the mid-stream window back at its original geometry.
EXPECT_GEOM="$((200 * UPSCALE / SCALE))x$((150 * UPSCALE / SCALE))+$((400 * UPSCALE / SCALE))+$((300 * UPSCALE / SCALE))"

XVFB_A=""
XVFB_B=""
SRV_PID=""
FILL_A_PID=""
FILL_B_PID=""

cleanup() {
	for pid in "$SRV_PID" "$FILL_A_PID" "$FILL_B_PID" "${CLIENT_PID:-}" "$XVFB_A" "$XVFB_B"; do
		[ -n "$pid" ] && kill "$pid" 2>/dev/null || true
	done
	rm -f "$SNAP" "$LOG" "$CLOG"
}
trap cleanup EXIT

command -v Xvfb >/dev/null 2>&1 || { echo "SKIP: no Xvfb"; exit 77; }

SHM_BEFORE=$(ipcs -m 2>/dev/null | awk -v u="$USER" '$3==u' | wc -l)

echo "== building"
make -s build
go build -o /tmp/remmote-testfill ./internal/testfill

echo "== starting Xvfb :991 (host) and :992 (viewer)"
Xvfb :991 -screen 0 1280x800x24 >/dev/null 2>&1 &
XVFB_A=$!
Xvfb :992 -screen 0 1280x800x24 >/dev/null 2>&1 &
XVFB_B=$!
sleep 0.8

echo "== painting host screen $COLOR_A"
/tmp/remmote-testfill -display :991 -color "$COLOR_A" -hold 40s >/dev/null 2>&1 &
FILL_A_PID=$!
sleep 1.5

echo "== starting server ($CODEC, periodic keyframes off)"
./bin/remmote-server -display :991 -listen "127.0.0.1:$PORT" \
	-codec "$CODEC" -downscale "$SCALE" -refresh 1h -v >"$LOG" 2>&1 &
SRV_PID=$!
sleep 1

echo "== starting windowed client (snapshot after 6s, -upscale $UPSCALE)"
DISPLAY=:992 timeout 30 ./bin/remmote-client -server "127.0.0.1:$PORT" \
	-upscale "$UPSCALE" \
	-snapshot-after 6s -snapshot "$SNAP" -no-clipboard -fast-scale 2>"$CLOG" &
CLIENT_PID=$!

sleep 3 # client connected (~1s) + keyframe composited

echo "== mid-stream change: painting $COLOR_B window at $WIN_GEOM"
/tmp/remmote-testfill -display :991 -windows "$COLOR_B@$WIN_GEOM" -hold 20s >/dev/null 2>&1 &
FILL_B_PID=$!
sleep 1 # damage-driven delta lands within ~50ms

# Input path: warp the pointer over the viewer's window on :992 and
# assert it arrives on the host. The client maps window → host through
# the letterbox fit: the viewer window is capped at 80% of the local
# screen, so a 1280x800 host is shown in a 1024x640 window and the
# mapping is 1280/1024 by 800/640 (integer division, as the client does).
#
# With -upscale the canvas is the host size, so the window is sized from
# that instead. The pointer is then divided by -upscale on the way out
# and the server multiplies by -downscale, so the net factor is
# SCALE/UPSCALE — with a matching pair the pointer lands on the same
# host pixel as an unscaled session.
STREAM_W=$((1280 / SCALE))
STREAM_H=$((800 / SCALE))
CANVAS_W=$((STREAM_W * UPSCALE))
CANVAS_H=$((STREAM_H * UPSCALE))
VIEW_W=$((CANVAS_W < 1024 ? CANVAS_W : 1024))
VIEW_H=$((CANVAS_H < 640 ? CANVAS_H : 640))
IN_X=$((VIEW_W * 3 / 5))
IN_Y=$((VIEW_H * 3 / 5))
EXP_X=$((IN_X * CANVAS_W / VIEW_W / UPSCALE * SCALE))
EXP_Y=$((IN_Y * CANVAS_H / VIEW_H / UPSCALE * SCALE))
echo "== input: warping the viewer pointer to $IN_X,$IN_Y (expect host $EXP_X,$EXP_Y)"
/tmp/remmote-testfill -display :992 -warp "$IN_X,$IN_Y"
HOST_PTR=""
for _ in $(seq 1 50); do
	HOST_PTR=$(/tmp/remmote-testfill -display :991 -pointer 2>/dev/null | sed -n 's/^PTR://p')
	[ "$HOST_PTR" = "$EXP_X,$EXP_Y" ] && break
	sleep 0.1
done
if [ "$HOST_PTR" != "$EXP_X,$EXP_Y" ]; then
	echo "FAIL: pointer input did not reach the host (got '$HOST_PTR', want '$EXP_X,$EXP_Y')"
	cat "$CLOG"
	exit 1
fi
echo "OK: host pointer at $HOST_PTR (input reached XTEST)"

wait "$CLIENT_PID"
grep -q "snapshot written" "$CLOG" || {
	echo "FAIL: client did not write a snapshot"
	cat "$CLOG"
	exit 1
}

echo "== verifying: canvas is $CANVAS_W x $CANVAS_H and the delta lands at the exact position"
go run ./internal/testfill -check "$SNAP" -expect "$COLOR_A" -min-match 85 \
	-expect-size "${CANVAS_W}x${CANVAS_H}" \
	-expect-region "$EXPECT_GEOM:$COLOR_B"

grep -q "client connected" "$LOG" || {
	echo "FAIL: no client-connect in server log"
	exit 1
}

kill "$SRV_PID" "$FILL_A_PID" "$FILL_B_PID" 2>/dev/null || true
wait "$SRV_PID" 2>/dev/null || true
SRV_PID=""
sleep 0.5
SHM_AFTER=$(ipcs -m 2>/dev/null | awk -v u="$USER" '$3==u' | wc -l)
if [ "$SHM_AFTER" -gt "$SHM_BEFORE" ]; then
	echo "FAIL: shared memory segments leaked ($SHM_BEFORE → $SHM_AFTER)"
	exit 1
fi

echo "PASS: delta pipeline verified (mid-stream update at $WIN_GEOM + input round trip, codec $CODEC, downscale $SCALE, upscale $UPSCALE)"
