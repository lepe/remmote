#!/usr/bin/env bash
# start-xvfb.sh — create a headless X display with Xvfb for remmote.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"

XVFB_BIN=${XVFB_BIN:-Xvfb}
DISPLAY_NUM=''   # empty: ask (88 with --stop)
SIZE=''          # empty: ask
WM=''            # empty: ask
NO_AUTH=0
DETACH=0
STOP=0
ASSUME_YES=0

description() {
	cat <<'EOF'
start-xvfb.sh — start a fresh headless X display with Xvfb, ready for
remmote-server to share. No existing desktop session is needed.

What it does
  Creates a virtual X screen (default :88) with no physical output, runs
  a window manager on it if you ask for one, and keeps both alive while
  remmote-server captures and controls it: -exec <command> shares an
  application, without -exec it shares the whole virtual desktop.

Limitations (worth reading once)
  * The screen size is fixed when the display is created. Xvfb exposes
    exactly one RANDR mode — its -screen size — so remmote's
    -resize-desktop can never change it (verified: RANDR refuses), and
    the viewer letterboxes instead. Resizing a shared application's
    window (-exec / -window) works fine.
  * Nothing is visible locally: there is no output. Watch and drive the
    display with remmote-client, or point a VNC server at it.
  * No window manager unless you pick one below: application windows
    then have no decorations and sit exactly where the application puts
    them. remmote handles that, but some applications look and behave
    better with one.
  * The display carries a magic cookie (xauth) unless you pass
    --no-auth — then *any local user* can connect to it, and whoever
    reaches a remmote port has full keyboard and mouse control.
  * The X socket is local only (-nolisten tcp). What you expose (or
    tunnel) is remmote-server's port, never the display itself.
EOF
}

usage() {
	description
	cat <<'EOF'

Options:
  -d, --display N   X display number to create (default: ask, e.g. 88)
  -s, --size WxH    screen size, fixed for the display's life (default: ask)
  -w, --wm NAME     window manager to run inside, or 'none' (default: ask)
      --no-auth     no magic cookie: any local user may connect
  -b, --detach      start in the background and return
      --stop        stop that display (and its window manager) and exit
  -y, --yes         answer yes to installation questions
      --xvfb-bin P  Xvfb to use (default: Xvfb; env XVFB_BIN works too)
  -h, --help        this text

Run it without options to be asked everything. The display stays up
until Ctrl-C — or until `start-xvfb.sh --display N --stop` when it was
started with --detach.
EOF
}

while [ $# -gt 0 ]; do
	case $1 in
		-d|--display)   DISPLAY_NUM=${2:?missing value for $1}; shift 2 ;;
		-s|--size)      SIZE=${2:?missing value for $1}; shift 2 ;;
		-w|--wm)        WM=${2:?missing value for $1}; shift 2 ;;
		--no-auth)      NO_AUTH=1; shift ;;
		-b|--detach)    DETACH=1; shift ;;
		--stop)         STOP=1; shift ;;
		-y|--yes)       ASSUME_YES=1; shift ;;
		--xvfb-bin)     XVFB_BIN=${2:?missing value for $1}; shift 2 ;;
		-h|--help)      usage; exit 0 ;;
		*)              usage >&2; die "unknown option '$1'" ;;
	esac
done

runtime_dir
RUNDIR=$REPLY

if [ -n "$DISPLAY_NUM" ]; then
	normalize_display "$DISPLAY_NUM"
	DISPLAY_NUM=$REPLY
elif [ "$STOP" = 1 ]; then
	DISPLAY_NUM=88
fi

# set_paths — pid/log/cookie files for the display; called once the
# display number is known (again after the prompt, in interactive runs).
set_paths() {
	X_PIDFILE="$RUNDIR/remmote-xvfb-$DISPLAY_NUM.pid"
	WM_PIDFILE="$RUNDIR/remmote-xvfb-$DISPLAY_NUM.wm.pid"
	LOGFILE="$RUNDIR/remmote-xvfb-$DISPLAY_NUM.log"
	AUTHFILE="$RUNDIR/remmote-xvfb-$DISPLAY_NUM.Xauthority"
}

if [ "$STOP" = 1 ]; then
	set_paths
	stop_pid "$WM_PIDFILE"
	stop_pid "$X_PIDFILE"
	rm -f "$AUTHFILE"
	say "display :$DISPLAY_NUM stopped (if it was running)"
	exit 0
fi

head1 'What this script does'
description

# ---- dependencies --------------------------------------------------------

ensure_command "$XVFB_BIN" xvfb 'the headless X server this script starts'

# ---- questions -----------------------------------------------------------

if [ -z "$DISPLAY_NUM" ]; then
	head1 'Display'
	while :; do
		ask_match 'X display number to create' '88' '^:?[0-9]+$' 'a display number looks like 88 or :88'
		normalize_display "$REPLY"
		DISPLAY_NUM=$REPLY
		display_state "$DISPLAY_NUM"
		if [ "$REPLY" = free ]; then
			break
		fi
		claim_display "$DISPLAY_NUM"
	done
else
	claim_display "$DISPLAY_NUM"
fi
set_paths

if [ -z "$SIZE" ]; then
	head1 'Screen'
	ask 'screen size (fixed for the life of the display)' 1920x1080
	SIZE=$REPLY
fi
if ! [[ $SIZE =~ ^[0-9]+x[0-9]+$ ]]; then
	die "size must look like 1920x1080 (got '$SIZE')"
fi

if [ -z "$WM" ]; then
	head1 'Window manager'
	pick_wm none
	WM=$REPLY
elif [ "$WM" != none ]; then
	command -v "$WM" >/dev/null 2>&1 || die "window manager '$WM' is not installed (see --wm none)"
fi

AUTH_ARGS=()
AUTH_NOTE='no cookie (--no-auth): any local user may connect'
if [ "$NO_AUTH" = 0 ]; then
	if make_cookie "$AUTHFILE" ":$DISPLAY_NUM"; then
		AUTH_ARGS=(-auth "$AUTHFILE")
		AUTH_NOTE="cookie: $AUTHFILE (pass it as XAUTHORITY)"
	else
		AUTH_NOTE='no cookie: xauth is not installed, so any local user may connect'
		rm -f "$AUTHFILE"
		AUTHFILE=''
		warn 'xauth is not installed — the display will not need a cookie'
	fi
else
	rm -f "$AUTHFILE"
	AUTHFILE=''
fi

# ---- start ---------------------------------------------------------------

head1 "Starting display :$DISPLAY_NUM"
ARGS=(":$DISPLAY_NUM" -screen 0 "${SIZE}x24" -nolisten tcp "${AUTH_ARGS[@]+"${AUTH_ARGS[@]}"}")
spawn "$LOGFILE" "$XVFB_BIN" "${ARGS[@]}"
X_PID=$REPLY
echo "$X_PID" >"$X_PIDFILE"

# The socket appears within a moment; anything longer is a startup error.
for _ in $(seq 1 50); do
	if [ -S "/tmp/.X11-unix/X$DISPLAY_NUM" ]; then
		break
	fi
	kill -0 "$X_PID" 2>/dev/null || break
	sleep 0.1
done
if ! kill -0 "$X_PID" 2>/dev/null; then
	rm -f "$X_PIDFILE"
	die "$XVFB_BIN exited at once — see $LOGFILE"
fi
if [ ! -S "/tmp/.X11-unix/X$DISPLAY_NUM" ]; then
	stop_pid "$X_PIDFILE"
	die "display :$DISPLAY_NUM did not come up — see $LOGFILE"
fi

WM_NAME=none
WM_PID=''
if [ "$WM" != none ]; then
	XENV=(DISPLAY=":$DISPLAY_NUM")
	if [ -n "$AUTHFILE" ]; then
		XENV+=("XAUTHORITY=$AUTHFILE")
	fi
	spawn "$LOGFILE" env "${XENV[@]}" "$WM"
	WM_PID=$REPLY
	echo "$WM_PID" >"$WM_PIDFILE"
	WM_NAME=$WM
fi

PREFIX=''
if [ -n "$AUTHFILE" ]; then
	PREFIX="XAUTHORITY=$AUTHFILE "
fi

say "display :$DISPLAY_NUM is up: $XVFB_BIN ${SIZE}x24, window manager: $WM_NAME"
note "log: $LOGFILE"
note "auth: $AUTH_NOTE"
cat <<EOF

Share it (from this machine):
  ${PREFIX}DISPLAY=:$DISPLAY_NUM ./bin/remmote-server -listen :7677 -exec '<command to run>'
  ${PREFIX}DISPLAY=:$DISPLAY_NUM ./bin/remmote-server -listen :7677
  ./bin/remmote-client -server 127.0.0.1:7677

Or answer the questions instead: ./scripts/remmote.sh (it picks up
this cookie on its own)
(Reminder: the screen is fixed at ${SIZE} — -resize-desktop cannot change
it here, while -exec / -window resizes work.)
EOF

if [ "$DETACH" = 1 ]; then
	say "Stop it with: $0 --display $DISPLAY_NUM --stop"
	exit 0
fi

cleanup() {
	say ''
	say "stopping display :$DISPLAY_NUM"
	stop_pid "${WM_PIDFILE:-}"
	stop_pid "${X_PIDFILE:-}"
	rm -f "${AUTHFILE:-}"
}
trap 'cleanup; exit 130' INT TERM
say "Stop it with Ctrl-C."
wait "$X_PID" 2>/dev/null || true
cleanup
