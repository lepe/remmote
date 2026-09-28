#!/usr/bin/env bash
# Window-mode integration test: paint two colored windows on Xvfb, share
# one of them via -window, and assert the client snapshot is exactly the
# composite canvas (union bbox, quantized to 32px) with both windows
# visible. Then kill the painter and assert the server stays up serving
# the retained last frame.
set -euo pipefail
cd "$(dirname "$0")/.."

PORT=17710
DISP=:993
SNAP=$(mktemp /tmp/remmote-win-XXXXXX.png)
LOG=$(mktemp /tmp/remmote-win-XXXXXX.log)

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

command -v Xvfb >/dev/null 2>&1 || { echo "SKIP: no Xvfb"; exit 77; }

SHM_BEFORE=$(ipcs -m 2>/dev/null | awk -v u="$USER" '$3==u' | wc -l)

echo "== building"
make -s build
go build -o /tmp/remmote-testfill ./internal/testfill

echo "== starting Xvfb $DISP"
Xvfb $DISP -screen 0 800x600x24 >/dev/null 2>&1 &
XVFB_PID=$!
sleep 0.8

echo "== painting two windows"
# Window 1: (50,60) 300x200 green. Window 2: (250,180) 200x150 orange.
# Union: (50,60)-(450,330) → quantized canvas (32,32)-(480,352) = 448x320.
/tmp/remmote-testfill -display $DISP -windows "#20c040@300x200+50+60,#e08040@200x150+250+180" -hold 40s > /tmp/remmote-win-fill.log 2>&1 &
FILL_PID=$!
sleep 1
WIN=$(grep -o 'WIN:0x[0-9a-f]*' /tmp/remmote-win-fill.log | head -1 | cut -d: -f2)
[ -n "$WIN" ] || { echo "FAIL: no window id from testfill"; cat /tmp/remmote-win-fill.log; exit 1; }
echo "seed window: $WIN"

echo "== starting server in -window mode"
./bin/remmote-server -display $DISP -listen "127.0.0.1:$PORT" -window "$WIN" -v >"$LOG" 2>&1 &
SRV_PID=$!
sleep 1.5

echo "== client snapshot #1"
./bin/remmote-client -server "127.0.0.1:$PORT" -once -snapshot "$SNAP" 2>/dev/null
python3 - "$SNAP" <<'EOF'
import struct, sys, zlib
d = open(sys.argv[1], 'rb').read()
w, h = struct.unpack('>II', d[16:24])
if (w, h) != (448, 320):
    print(f"FAIL: snapshot {w}x{h}, want 448x320 (quantized union bbox)")
    sys.exit(1)
print(f"OK: canvas {w}x{h}")
EOF
go run ./internal/testfill -check "$SNAP" -expect '#20c040' -min-match 20
go run ./internal/testfill -check "$SNAP" -expect '#e08040' -min-match 5 \
	-expect-region "100x100+40+50:#20c040,80x90+320+160:#e08040"

grep -q "tracking window" "$LOG" || { echo "FAIL: server did not track the seed window"; grep -v authority "$LOG" | tail -5; exit 1; }

echo "== killing painter; server must stay up"
kill "$FILL_PID" 2>/dev/null || true
FILL_PID=""
sleep 3.5

SNAP2=$(mktemp /tmp/remmote-win-XXXXXX.png)
./bin/remmote-client -server "127.0.0.1:$PORT" -once -snapshot "$SNAP2" 2>/dev/null
python3 - "$SNAP2" <<'EOF'
import struct, sys
d = open(sys.argv[1], 'rb').read()
w, h = struct.unpack('>II', d[16:24])
if (w, h) != (448, 320):
    print(f"FAIL: retained frame {w}x{h}, want 448x320")
    sys.exit(1)
print("OK: last frame retained after app exit")
EOF
rm -f "$SNAP2"
kill -0 "$SRV_PID" || { echo "FAIL: server exited after app exit"; exit 1; }
echo "OK: server stayed up"

kill "$SRV_PID" 2>/dev/null || true
wait "$SRV_PID" 2>/dev/null || true
SRV_PID=""
sleep 0.5
SHM_AFTER=$(ipcs -m 2>/dev/null | awk -v u="$USER" '$3==u' | wc -l)
if [ "$SHM_AFTER" -gt "$SHM_BEFORE" ]; then
	echo "FAIL: SHM segments leaked ($SHM_BEFORE → $SHM_AFTER)"
	exit 1
fi

echo "PASS: window-mode sharing verified on $DISP"
