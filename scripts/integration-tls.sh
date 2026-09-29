#!/usr/bin/env bash
# TLS integration test: the same keyframe pipeline as integration.sh, run
# over an encrypted link. Asserts that trust is decided and enforced:
#
#   - a client with the server's fingerprint connects and captures,
#   - a client with a different fingerprint is refused,
#   - -tls alone connects (the simple path) and states that it is not
#     verifying the certificate,
#   - a plain client cannot read a TLS server at all (encryption is not
#     silently optional).
#
# The generated certificate is written under a throwaway XDG_CONFIG_HOME
# so the test never touches a real ~/.config/remmote.
set -euo pipefail
cd "$(dirname "$0")/.."

PORT=17740
COLOR='#20c040'
DISP=:990
SNAP=$(mktemp /tmp/remmote-tls-XXXXXX.png)
LOG=$(mktemp /tmp/remmote-tls-XXXXXX.log)
CLOG=$(mktemp /tmp/remmote-tls-XXXXXX.client)
CFG=$(mktemp -d /tmp/remmote-tls-cfg-XXXXXX)

XVFB_PID=""
SRV_PID=""
FILL_PID=""
cleanup() {
	for pid in "${SRV_PID:-}" "${FILL_PID:-}" "${XVFB_PID:-}"; do
		[ -n "$pid" ] && kill "$pid" 2>/dev/null || true
	done
	rm -f "$SNAP" "$LOG" "$CLOG"
	rm -rf "$CFG"
}
trap cleanup EXIT

command -v Xvfb >/dev/null 2>&1 || { echo "SKIP: no Xvfb"; exit 77; }

echo "== building"
make -s build

echo "== starting Xvfb $DISP"
Xvfb $DISP -screen 0 1280x800x24 >/dev/null 2>&1 &
XVFB_PID=$!
sleep 0.7

echo "== painting $COLOR on $DISP"
go run ./internal/testfill -display "$DISP" -color "$COLOR" -hold 60s >/dev/null 2>&1 &
FILL_PID=$!
sleep 1.5

echo "== starting server with -tls"
XDG_CONFIG_HOME="$CFG" ./bin/remmote-server -display "$DISP" \
	-listen "127.0.0.1:$PORT" -tls -v >"$LOG" 2>&1 &
SRV_PID=$!
for _ in $(seq 1 50); do
	grep -q "listening" "$LOG" && break
	sleep 0.2
done
grep -q "listening" "$LOG" || {
	echo "FAIL: server never listened"
	grep -v authority "$LOG" | tail -6
	exit 1
}

FINGERPRINT=$(grep -oE 'SHA256:[0-9a-f]+' "$LOG" | head -1)
if [ -z "$FINGERPRINT" ]; then
	echo "FAIL: no fingerprint in the server log"
	grep -v authority "$LOG" | tail -6
	exit 1
fi
grep -q "TLS: the stream is encrypted" "$LOG" || {
	echo "FAIL: server did not log its TLS banner"
	exit 1
}
echo "OK: fingerprint $FINGERPRINT"

# The certificate must be cached, or the fingerprint would change on the
# next start and every pin would break.
[ -f "$CFG/remmote/server-cert.pem" ] || {
	echo "FAIL: generated certificate was not cached"
	ls -la "$CFG" 2>/dev/null || true
	exit 1
}
echo "OK: certificate cached"

snapshot() { # $1 = case label, remaining args = client flags
	local label="$1"
	shift
	rm -f "$SNAP" "$CLOG"
	XDG_CONFIG_HOME="$CFG" DISPLAY=$DISP timeout 20 ./bin/remmote-client \
		-server "127.0.0.1:$PORT" -once -snapshot "$SNAP" "$@" >"$CLOG" 2>&1
	rc=$?
	echo "   client[$label] exit=$rc"
	return $rc
}

echo "== case 1: pinned fingerprint"
if ! snapshot pinned -tls -tls-fingerprint "$FINGERPRINT"; then
	echo "FAIL: pinned client did not connect"
	cat "$CLOG"
	exit 1
fi
[ -f "$SNAP" ] || { echo "FAIL: pinned client wrote no snapshot"; cat "$CLOG"; exit 1; }
grep -q "msg=connected.*tls=true" "$CLOG" || {
	echo "FAIL: client did not report tls=true"
	grep -v authority "$CLOG" | tail -4
	exit 1
}
go run ./internal/testfill -check "$SNAP" -expect "$COLOR" -min-match 90
echo "OK: snapshot over TLS matches $COLOR"

echo "== case 2: wrong fingerprint must be refused"
rm -f "$SNAP"
if snapshot wrong-pin -tls -tls-fingerprint "SHA256:$(printf '%064d' 0)"; then
	echo "FAIL: a client with the wrong fingerprint connected"
	cat "$CLOG"
	exit 1
fi
grep -q "expected" "$CLOG" || {
	echo "FAIL: refusal did not explain the mismatch"
	cat "$CLOG"
	exit 1
}
if [ -f "$SNAP" ]; then
	echo "FAIL: refused client still wrote a snapshot"
	exit 1
fi
echo "OK: refused, with the mismatch named"

echo "== case 3: -tls with no fingerprint (the simple path)"
if ! snapshot insecure -tls; then
	echo "FAIL: a plain -tls client did not connect"
	cat "$CLOG"
	exit 1
fi
grep -q "msg=connected.*tls=true" "$CLOG" || { echo "FAIL: not reported as TLS"; cat "$CLOG"; exit 1; }
grep -q "without verifying" "$CLOG" || {
	echo "FAIL: client did not state that it was not verifying the certificate"
	cat "$CLOG"
	exit 1
}
echo "OK: -tls alone connected, and said so"

echo "== case 4: a plain client cannot read a TLS server"
if snapshot plain; then
	echo "FAIL: an unencrypted client connected to a -tls server"
	cat "$CLOG"
	exit 1
fi
echo "OK: plain client refused"

echo "== case 5: an interactive client reports a failed connect"
# The path that produced the user's report: an interactive client looping
# in backoff while printing nothing, leaving only the server's
# "read hello: EOF" to go on.
rm -f "$CLOG"
XDG_CONFIG_HOME="$CFG" DISPLAY=$DISP timeout 6 ./bin/remmote-client \
	-server "127.0.0.1:$PORT" -tls -tls-fingerprint "SHA256:$(printf '%064d' 0)" \
	>"$CLOG" 2>&1 || true
grep -q "connect failed; retrying" "$CLOG" || {
	echo "FAIL: the interactive client did not report why it could not connect"
	grep -v authority "$CLOG" | tail -6
	exit 1
}
echo "OK: interactive client stated the failure"

kill "$SRV_PID" 2>/dev/null || true
wait "$SRV_PID" 2>/dev/null || true
SRV_PID=""

echo "PASS: TLS link verified (bare -tls, pinned, refused, plaintext, reported failure)"
