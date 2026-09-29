#!/usr/bin/env bash
# -exec integration test: the spawned command exits at once while its
# window appears afterwards — exactly what a single-instance application
# (konsole, kate, dolphin) does when it hands off to an already-running
# process over D-Bus. The server must keep looking for the window instead
# of giving up on the early exit and serving a permanent 1x1 canvas.
#
# A launcher named "remmote-app" starts the real window in the background
# and exits; the target paints a WM_CLASS whose instance matches the
# launcher's basename, which is the only signal left once the launched
# process is gone. Regression guard for launch.Bootstrap returning Win=0
# the moment proc.Done() fires.
set -euo pipefail
cd "$(dirname "$0")/.."

PORT=17720
DISP=:991
SNAP=$(mktemp /tmp/remmote-exec-XXXXXX.png)
LOG=$(mktemp /tmp/remmote-exec-XXXXXX.log)
CLOG=$(mktemp /tmp/remmote-exec-XXXXXX.client)
BIN=$(mktemp -d)

XVFB_PID=""
SRV_PID=""
cleanup() {
	for pid in "${SRV_PID:-}" "${XVFB_PID:-}"; do
		[ -n "$pid" ] && kill "$pid" 2>/dev/null || true
	done
	rm -f "$SNAP" "$LOG" "$CLOG"
	rm -rf "$BIN"
}
trap cleanup EXIT

command -v Xvfb >/dev/null 2>&1 || { echo "SKIP: no Xvfb"; exit 77; }

echo "== building"
make -s build
go build -o "$BIN/remmote-app-target" ./internal/testfill

# 200x150 at +40+30 → quantized canvas (32,32)-(256,192) = 224x160.
cat > "$BIN/remmote-app" <<EOF
#!/bin/sh
"$BIN/remmote-app-target" -size 200x150 -pos 40+30 \\
	-wm-class remmote-app:RemmoteApp -color '#20c040' -hold 30s >/dev/null 2>&1 &
exit 0
EOF
chmod +x "$BIN/remmote-app"

echo "== starting Xvfb $DISP"
Xvfb $DISP -screen 0 800x600x24 >/dev/null 2>&1 &
XVFB_PID=$!
sleep 0.8

echo "== starting server (-exec launcher that exits immediately)"
./bin/remmote-server -display $DISP -listen "127.0.0.1:$PORT" \
	-exec "$BIN/remmote-app" -v >"$LOG" 2>&1 &
SRV_PID=$!

# The window is only findable through the class heuristic, which needs the
# guess to persist, so allow the full window-search budget.
LISTENED=""
for _ in $(seq 1 100); do
	grep -q "listening" "$LOG" && { LISTENED=1; break; }
	sleep 0.2
done
if [ -z "$LISTENED" ]; then
	echo "FAIL: server never listened"
	grep -v authority "$LOG" | tail -5
	exit 1
fi
SOURCE=$(grep -oE 'source=[0-9]+x[0-9]+' "$LOG" | tail -1)
echo "server listening ($SOURCE)"

if [ "$SOURCE" = "source=1x1" ]; then
	echo "FAIL: server captured no window after the launch process exited ($SOURCE)"
	grep -v authority "$LOG" | tail -5
	exit 1
fi

echo "== client snapshot"
DISPLAY=$DISP timeout 20 ./bin/remmote-client -server "127.0.0.1:$PORT" \
	-once -snapshot "$SNAP" >"$CLOG" 2>&1
[ -f "$SNAP" ] || { echo "FAIL: no snapshot"; cat "$CLOG"; exit 1; }

echo "== verifying: the launched window was captured"
# Canvas is window rect (40,30)-(240,180) quantized outward to the 32 px
# grid: (32,0)-(256,192) = 224x192, so the window sits at canvas (8,30).
go run ./internal/testfill -check "$SNAP" -expect '#20c040' -min-match 60 \
	-expect-size 224x192 \
	-expect-region "200x150+8+30:#20c040"

kill "$SRV_PID" 2>/dev/null || true
wait "$SRV_PID" 2>/dev/null || true
SRV_PID=""
pkill -f "$BIN/remmote-app-target" 2>/dev/null || true

echo "PASS: -exec captured the window of a launcher that exited immediately"
