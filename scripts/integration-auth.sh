#!/usr/bin/env bash
# Paired-device authentication integration test: a daemon that only
# admits devices it has paired with — each named, roled and revocable on
# its own, with a key that never left its machine.
#
#   * pairing: a code becomes a device credential, and the daemon's CA is
#     what the device verifies it against from then on
#   * unpaired: everything except pairing is refused
#   * roles: a view device may watch, but may not start or terminate
#   * revocation: one device loses its admission, the others keep theirs
#   * the stream: a paired device attaches with its own credential
#   * the door: a listener that is not loopback-only will not run without
#     TLS unless -insecure says so
set -euo pipefail
cd "$(dirname "$0")/.."

PORT=17780
COLOR='#20c040'
DISP=:998
SNAP=$(mktemp /tmp/remmote-auth-XXXXXX.png)
LOG=$(mktemp /tmp/remmote-auth-XXXXXX.log)
SPEC=$(mktemp -d)
TMP=$(mktemp -d)
export XDG_CONFIG_HOME="$TMP/cfg"

XVFB_PID=""
SRV_PID=""
FILL_PID=""
cleanup() {
	for pid in "${SRV_PID:-}" "${FILL_PID:-}" "${XVFB_PID:-}"; do
		[ -n "$pid" ] || continue
		kill "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
	done
	rm -f "$SNAP" "$LOG"
	rm -rf "$SPEC" "$TMP"
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

cat >"$SPEC/desktop.json" <<EOF
{
  "source": "desktop",
  "display": {"kind": "existing", "name": "$DISP"},
  "stream": {"codec": "jpeg"}
}
EOF

# PAIRC talks to the daemon before this device has a credential (the
# pairing code is the secret); IDC talks as a paired device.
PAIRC() { ./bin/remmote-ctl -server "127.0.0.1:$PORT" -tls auto "$@"; }
IDC() { local dev="$1"; shift; ./bin/remmote-ctl -server "127.0.0.1:$PORT" -identity "$dev" "$@"; }

echo "== starting Xvfb $DISP"
Xvfb $DISP -screen 0 800x600x24 >/dev/null 2>&1 &
XVFB_PID=$!
sleep 0.8
kill -0 "$XVFB_PID" 2>/dev/null || { echo "FAIL: Xvfb did not start"; exit 1; }

echo "== painting $COLOR on $DISP"
go run ./internal/testfill -display "$DISP" -color "$COLOR" -hold 90s >/dev/null 2>&1 &
FILL_PID=$!
sleep 1.5

echo "== starting a daemon that only admits paired devices"
./bin/remmote-server -idle -listen "127.0.0.1:$PORT" -auth -auth-dir "$TMP/auth" -v >"$LOG" 2>&1 &
SRV_PID=$!
CODE=""
for _ in $(seq 1 50); do
	[ -s "$TMP/auth/pairing-code" ] && CODE=$(cat "$TMP/auth/pairing-code") && break
	sleep 0.2
done
[ -n "$CODE" ] || { echo "FAIL: no pairing code was offered"; grep -v authority "$LOG" | tail -6; exit 1; }

echo "== an unpaired device is refused everything but pairing"
if PAIRC host >"$SPEC/out" 2>&1; then
	echo "FAIL: an unpaired device got host information"
	cat "$SPEC/out"
	exit 1
fi
grep -q "paired" "$SPEC/out" || { echo "FAIL: the refusal did not name the fix"; cat "$SPEC/out"; exit 1; }

echo "== pairing the first device"
PAIRC pair -name operator -code "$CODE" | tee "$SPEC/pair"
grep -q 'as admin' "$SPEC/pair" || { echo "FAIL: the first pair is not the administrator"; exit 1; }

echo "== a wrong code is refused"
if PAIRC pair -name impostor -code "00000000" >"$SPEC/out" 2>&1; then
	echo "FAIL: a made-up pairing code was accepted"
	exit 1
fi

echo "== pairing a second device as view only"
CODE2=$(IDC operator pair-code -role view | grep -oE '[A-F0-9]{8}' | head -1 || true)
[ -n "$CODE2" ] || { echo "FAIL: no view pairing code came back"; exit 1; }
PAIRC pair -name viewer -code "$CODE2" | grep -q 'as view' || {
	echo "FAIL: the second device is not paired as view"
	exit 1
}

echo "== a view device may not start a session"
if IDC viewer start -spec "$SPEC/desktop.json" >"$SPEC/out" 2>&1; then
	echo "FAIL: a view-only device started a session"
	exit 1
fi
grep -q "control" "$SPEC/out" || { echo "FAIL: the refusal did not name the role it needs"; cat "$SPEC/out"; exit 1; }

echo "== the operator starts a session"
IDC operator start -spec "$SPEC/desktop.json" -wait || {
	echo "FAIL: the operator could not start a session"
	IDC operator session || true
	exit 1
}

echo "== the view device sees it"
has "$(IDC viewer session)" '"state": "live"' || {
	echo "FAIL: a view-only device cannot see the session"
	exit 1
}

echo "== the view device may not terminate it"
if IDC viewer terminate >"$SPEC/out" 2>&1; then
	echo "FAIL: a view-only device terminated the service"
	exit 1
fi
grep -q "control" "$SPEC/out" || { echo "FAIL: the refusal did not name the role it needs"; cat "$SPEC/out"; exit 1; }

echo "== the roster lists both devices"
CLIENTS=$(IDC operator clients)
has "$CLIENTS" operator || { echo "FAIL: the operator is not on the roster"; echo "$CLIENTS"; exit 1; }
has "$CLIENTS" viewer || { echo "FAIL: the viewer is not on the roster"; echo "$CLIENTS"; exit 1; }

echo "== revoking one device, keeping the other"
IDC operator revoke -name viewer | grep -q revoked || { echo "FAIL: revocation said nothing"; exit 1; }
if IDC viewer session >"$SPEC/out" 2>&1; then
	echo "FAIL: a revoked device is still admitted"
	cat "$SPEC/out"
	exit 1
fi
has "$(IDC operator session)" '"state": "live"' || {
	echo "FAIL: revoking one device affected another"
	exit 1
}

echo "== the stream attaches with the device's own credential"
rm -f "$SNAP"
./bin/remmote-client -server "127.0.0.1:$PORT" -identity operator -once -snapshot "$SNAP"
go run ./internal/testfill -check "$SNAP" -expect "$COLOR"

echo "== a revoked device cannot attach to the stream either"
if ./bin/remmote-client -server "127.0.0.1:$PORT" -identity viewer -once -snapshot "$SNAP" >/dev/null 2>&1; then
	echo "FAIL: a revoked device streamed the session"
	exit 1
fi

echo "== a listener off this machine will not run without TLS"
if ./bin/remmote-server -idle -listen "0.0.0.0:$((PORT + 1))" >"$SPEC/out" 2>&1; then
	echo "FAIL: an unencrypted listener came up on every interface"
	exit 1
fi
grep -q "refusing to serve" "$SPEC/out" || { echo "FAIL: the refusal did not say why"; cat "$SPEC/out"; exit 1; }

echo "== terminate: the operator may"
IDC operator terminate
for _ in $(seq 1 50); do
	kill -0 "$SRV_PID" 2>/dev/null || break
	sleep 0.2
done
if kill -0 "$SRV_PID" 2>/dev/null; then
	echo "FAIL: the daemon kept running after terminate"
	exit 1
fi
SRV_PID=""

echo "PASS: paired-device admission verified (pair, refuse, roles, revoke, stream, door)"
