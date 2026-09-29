#!/usr/bin/env bash
# TLS integration test: the same keyframe pipeline as integration.sh, run
# over an encrypted link. Asserts that trust is decided and enforced:
#
#   - a client given the server's fingerprint connects and captures,
#   - a client given a different fingerprint is refused,
#   - -tls alone connects (the simple path) and states it is not verifying,
#   - a shared secret typed on both sides verifies without any log copying,
#   - a secret server admits only clients presenting a certificate derived
#     from the same secret, and both sides say why it was refused,
#   - a plain client cannot read a TLS server at all (encryption is not
#     silently optional),
#   - an interactive client that cannot connect says so instead of retrying
#     in silence.
#
# The generated certificate is written under a throwaway XDG_CONFIG_HOME
# so the test never touches a real ~/.config/remmote.
set -euo pipefail
cd "$(dirname "$0")/.."

PORT=17740
COLOR='#20c040'
DISP=:990
SECRET='integration shared word'
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

# start_server <args...>: (re)starts the server on a fresh port.
start_server() {
	if [ -n "$SRV_PID" ]; then
		kill "$SRV_PID" 2>/dev/null || true
		wait "$SRV_PID" 2>/dev/null || true
		SRV_PID=""
	fi
	PORT=$((PORT + 1))
	rm -f "$LOG"
	XDG_CONFIG_HOME="$CFG" ./bin/remmote-server -display "$DISP" \
		-listen "127.0.0.1:$PORT" "$@" -v >"$LOG" 2>&1 &
	SRV_PID=$!
	for _ in $(seq 1 60); do
		grep -q "listening" "$LOG" && break
		sleep 0.2
	done
	grep -q "listening" "$LOG" || {
		echo "FAIL: server never listened ($*)"
		grep -v authority "$LOG" | tail -6
		exit 1
	}
}

echo "== building"
make -s build

echo "== starting Xvfb $DISP"
Xvfb $DISP -screen 0 1280x800x24 >/dev/null 2>&1 &
XVFB_PID=$!
sleep 0.7

echo "== painting $COLOR on $DISP"
go run ./internal/testfill -display "$DISP" -color "$COLOR" -hold 90s >/dev/null 2>&1 &
FILL_PID=$!
sleep 1.5

echo "== starting server with -tls"
start_server -tls

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

echo "== case 1: fingerprint as the -tls value"
if ! snapshot pinned -tls "$FINGERPRINT"; then
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
if snapshot wrong-pin -tls "SHA256:$(printf '%064d' 0)"; then
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

echo "== case 3: -tls with no value (the simple path)"
if ! snapshot bare -tls; then
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
# The path that produced a user-visible bug: an interactive client looping
# in backoff while printing nothing, leaving only the server's
# "read hello: EOF" to go on.
rm -f "$CLOG"
XDG_CONFIG_HOME="$CFG" DISPLAY=$DISP timeout 6 ./bin/remmote-client \
	-server "127.0.0.1:$PORT" -tls "SHA256:$(printf '%064d' 0)" \
	>"$CLOG" 2>&1 || true
grep -q "connect failed; retrying" "$CLOG" || {
	echo "FAIL: the interactive client did not report why it could not connect"
	grep -v authority "$CLOG" | tail -6
	exit 1
}
echo "OK: interactive client stated the failure"

echo "== case 6: shared secret on both sides (no log to copy)"
start_server -tls "$SECRET"
if ! snapshot shared-secret -tls "$SECRET"; then
	echo "FAIL: the shared secret was refused"
	cat "$CLOG"
	exit 1
fi
grep -q "msg=connected.*tls=true" "$CLOG" || { echo "FAIL: not reported as TLS"; cat "$CLOG"; exit 1; }
# Verification must actually have happened: the client never read the
# server's log, so the only way it knows the certificate is right is the
# secret. If it were merely encrypting, it would have warned.
if grep -q "without verifying" "$CLOG"; then
	echo "FAIL: shared-secret client did not verify the certificate"
	cat "$CLOG"
	exit 1
fi
if grep -q "expected" "$CLOG"; then
	echo "FAIL: shared-secret client reported a mismatch"
	cat "$CLOG"
	exit 1
fi
go run ./internal/testfill -check "$SNAP" -expect "$COLOR" -min-match 90
echo "OK: shared secret verified, no fingerprint exchanged"

echo "== case 7: a different shared secret is refused"
rm -f "$SNAP"
if snapshot wrong-secret -tls "not the secret"; then
	echo "FAIL: a client with the wrong secret connected"
	cat "$CLOG"
	exit 1
fi
grep -q "shared secret" "$CLOG" || {
	echo "FAIL: refusal did not say the expectation came from the shared secret"
	cat "$CLOG"
	exit 1
}
echo "OK: wrong secret refused"

echo "== case 8: a secret server admits nobody without the secret"
# The secret has to gate access, not only identify the server — and the
# client has to be told what to do about it, not just that it was refused.
rm -f "$SNAP" "$CLOG"
if snapshot no-secret -tls; then
	echo "FAIL: a client without the secret connected to a server that requires it"
	cat "$CLOG"
	exit 1
fi
grep -qi "shared secret" "$CLOG" || {
	echo "FAIL: the refusal did not name the fix (pass the same -tls value)"
	cat "$CLOG"
	exit 1
}
grep -q "didn't provide a certificate" "$LOG" || {
	echo "FAIL: the server did not say why it refused the client"
	grep -v authority "$LOG" | tail -5
	exit 1
}
echo "OK: denied, and both sides say why"

kill "$SRV_PID" 2>/dev/null || true
wait "$SRV_PID" 2>/dev/null || true
SRV_PID=""

echo "PASS: TLS link verified (fingerprint, refused, bare, plaintext, reported failure, shared secret, admission)"
