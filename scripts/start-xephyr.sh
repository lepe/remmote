#!/usr/bin/env bash
# start-xephyr.sh — start a nested X display (Xephyr) for remmote.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"

XEPHYR_BIN=${XEPHYR_BIN:-Xephyr}
DISPLAY_NUM=''   # empty: ask (88 with --stop)
SIZE=''          # empty: ask
WM=''            # empty: ask
HOST_DISPLAY=''  # the X session to draw the window on
RESIZABLE=1
HOST_GRAB=1
NO_AUTH=0
DETACH=0
STOP=0
ASSUME_YES=0

description() {
	cat <<'EOF'
start-xephyr.sh — start a nested X display with Xephyr, ready for
remmote-server to share. Xephyr is a real X server that draws its whole
screen into a window on your current desktop, so you can watch the
shared display locally as well as through remmote.

What it does
  Opens a window (default 1280x800) that is display :88 itself, runs a
  window manager inside it if you ask for one, and keeps both alive
  while remmote-server captures and controls it. remmote injects
  keystrokes and mouse events into the nested display only — your own
  desktop is never touched.

Limitations (worth reading once)
  * It needs an existing X session to draw into (that is the point of
    nesting) — it is not a headless server, so it asks which session the
    window should open on, listing the displays that are up. For a
    machine with no desktop at all, use start-xvfb.sh instead.
  * The nested screen is that window: with the default --resizeable it
    follows when you resize the window, and remmote's -resize-desktop
    changes it too (through Xephyr's RandR — which resizes the window
    along with the screen). It is imperfect: Xephyr advertises a fixed
    list of mode sizes (1600x1200 … 160x160) and forgets even that list
    when its window is resized, so remmote falls back to the screen's
    modes and, failing those, asks Xephyr to create an exact-size mode.
    Where none of that works the viewer letterboxes and the server logs
    why.
  * Rendering goes through your existing X server (fast SHM XImages, no
    GPU acceleration), so heavy graphics is slower than a real session.
  * While the pointer is over the window, keyboard and mouse are grabbed
    by the nested display like a VM window; --no-host-grab keeps them
    with your desktop.
  * The nested display carries a magic cookie (xauth) unless you pass
    --no-auth, in which case *any local user* can connect to it — and
    whoever reaches a remmote port has full keyboard and mouse control.
EOF
}

usage() {
	description
	cat <<'EOF'

Options:
  -d, --display N     nested X display number (default: ask, e.g. 88)
  -s, --size WxH      window/screen size (default: ask, e.g. 1280x800)
  -w, --wm NAME       window manager to run inside, or 'none' (default: ask)
      --host-display D  host X session to draw the window on (default: ask;
                    $DISPLAY is the offered one)
      --fixed-size    keep the window at -screen size (default: --resizeable)
      --no-host-grab  do not grab keyboard/mouse while the pointer is over it
      --no-auth       no magic cookie on the nested display
  -b, --detach        start in the background and return
      --stop          stop the nested display (and its window manager)
  -y, --yes           answer yes to installation questions
      --xephyr-bin P  Xephyr to use (default: Xephyr; env XEPHYR_BIN works too)
  -h, --help          this text

Run it without options to be asked everything. Closing the window (or
Ctrl-C) stops the nested display; use --detach / --stop to run it in the
background like start-xvfb.sh.
EOF
}

while [ $# -gt 0 ]; do
	case $1 in
		-d|--display)     DISPLAY_NUM=${2:?missing value for $1}; shift 2 ;;
		-s|--size)        SIZE=${2:?missing value for $1}; shift 2 ;;
		-w|--wm)          WM=${2:?missing value for $1}; shift 2 ;;
		--host-display)   HOST_DISPLAY=${2:?missing value for $1}; shift 2 ;;
		--fixed-size)     RESIZABLE=0; shift ;;
		--resizeable)     RESIZABLE=1; shift ;;
		--no-host-grab)   HOST_GRAB=0; shift ;;
		--no-auth)        NO_AUTH=1; shift ;;
		-b|--detach)      DETACH=1; shift ;;
		--stop)           STOP=1; shift ;;
		-y|--yes)         ASSUME_YES=1; shift ;;
		--xephyr-bin)     XEPHYR_BIN=${2:?missing value for $1}; shift 2 ;;
		-h|--help)        usage; exit 0 ;;
		*)                usage >&2; die "unknown option '$1'" ;;
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
	X_PIDFILE="$RUNDIR/remmote-xephyr-$DISPLAY_NUM.pid"
	WM_PIDFILE="$RUNDIR/remmote-xephyr-$DISPLAY_NUM.wm.pid"
	LOGFILE="$RUNDIR/remmote-xephyr-$DISPLAY_NUM.log"
	AUTHFILE="$RUNDIR/remmote-xephyr-$DISPLAY_NUM.Xauthority"
}

if [ "$STOP" = 1 ]; then
	set_paths
	stop_pid "$WM_PIDFILE"
	stop_pid "$X_PIDFILE"
	rm -f "$AUTHFILE"
	say "nested display :$DISPLAY_NUM stopped (if it was running)"
	exit 0
fi

head1 'What this script does'
description

# ---- dependencies --------------------------------------------------------

ensure_command "$XEPHYR_BIN" xserver-xephyr 'the nested X server this script starts'

# ---- questions -----------------------------------------------------------

# ask_host_display -> $REPLY = ":N" of an existing display. Xephyr is
# nested: it draws its window into another X session, so it asks which
# one, listing what is up. (No "New" here — the host has to exist.)
ask_host_display() {
	local list='' mnem='' a n d
	list_displays
	if [ "${#DISPLAYS[@]}" -eq 0 ]; then
		die "Xephyr draws into an existing X session and none is running — log into a desktop first, or use start-xvfb.sh for a headless display"
	fi
	for d in "${DISPLAYS[@]}"; do
		list+="${list:+, }:$d"
		mnem+="${mnem:+/}$d"
	done
	while :; do
		if [ -n "${DISPLAY:-}" ] && [ -S "/tmp/.X11-unix/X${DISPLAY#:}" ]; then
			note "(Enter takes your own session, $DISPLAY)"
		fi
		printf 'host display (the window opens here) (%s) [%s]: ' "$list" "$mnem"
		read -r a || die 'input ended before an answer arrived'
		if [ -z "$a" ] && [ -n "${DISPLAY:-}" ]; then
			a=${DISPLAY#:}
		fi
		if [[ $a =~ ^:?([0-9]+)$ ]]; then
			n=${BASH_REMATCH[1]}
			if [ ! -S "/tmp/.X11-unix/X$n" ]; then
				printf '  display :%s is not running — pick one of: %s\n' "$n" "$list"
				continue
			fi
			REPLY=":$n"
			return 0
		fi
		printf '  pick one of: %s\n' "$list"
	done
}

if [ -n "$HOST_DISPLAY" ]; then
	# --host-display: use it without asking (it still has to open).
	normalize_display "$HOST_DISPLAY"
	HOST_DISPLAY=":$REPLY"
elif [ "${ASSUME_YES:-0}" = 1 ]; then
	if [ -n "${DISPLAY:-}" ] && [ -S "/tmp/.X11-unix/X${DISPLAY#:}" ]; then
		HOST_DISPLAY=$DISPLAY
	else
		list_displays
		if [ "${#DISPLAYS[@]}" -eq 0 ]; then
			die "Xephyr draws into an existing X session and none is running — use start-xvfb.sh for a headless display"
		fi
		HOST_DISPLAY=":${DISPLAYS[0]}"
	fi
else
	head1 'Host session'
	ask_host_display
	HOST_DISPLAY=$REPLY
fi
use_display_auth "${HOST_DISPLAY#:}"

if [ -z "$DISPLAY_NUM" ]; then
	head1 'Nested display'
	while :; do
		ask_match 'nested X display number to create' '88' '^:?[0-9]+$' 'a display number looks like 88 or :88'
		normalize_display "$REPLY"
		DISPLAY_NUM=$REPLY
		if [ ":$DISPLAY_NUM" = "$HOST_DISPLAY" ]; then
			warn "display :$DISPLAY_NUM is your own desktop — pick another number"
			continue
		fi
		display_state "$DISPLAY_NUM"
		if [ "$REPLY" = free ]; then
			break
		fi
		claim_display "$DISPLAY_NUM"
	done
else
	if [ ":$DISPLAY_NUM" = "$HOST_DISPLAY" ]; then
		die "display :$DISPLAY_NUM is the host display ($HOST_DISPLAY) — pick another number"
	fi
	claim_display "$DISPLAY_NUM"
fi
set_paths

if [ -z "$SIZE" ]; then
	head1 'Window and screen'
	ask 'window/screen size (the window is this screen)' 1280x800
	SIZE=$REPLY
fi
if ! [[ $SIZE =~ ^[0-9]+x[0-9]+$ ]]; then
	die "size must look like 1280x800 (got '$SIZE')"
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
		warn 'xauth is not installed — the nested display will not need a cookie'
	fi
else
	rm -f "$AUTHFILE"
	AUTHFILE=''
fi

# ---- start ---------------------------------------------------------------

head1 "Starting nested display :$DISPLAY_NUM on $HOST_DISPLAY"
ARGS=(":$DISPLAY_NUM" -screen "$SIZE" -nolisten tcp)
if [ "$RESIZABLE" = 1 ]; then
	ARGS+=(-resizeable)
fi
if [ "$HOST_GRAB" = 0 ]; then
	ARGS+=(-no-host-grab)
fi
ARGS+=("${AUTH_ARGS[@]+"${AUTH_ARGS[@]}"}")

# Xephyr authenticates to the host session with the ordinary environment;
# -auth and the cookie belong to the *nested* display it serves.
spawn "$LOGFILE" env DISPLAY="$HOST_DISPLAY" "$XEPHYR_BIN" "${ARGS[@]}"
X_PID=$REPLY
echo "$X_PID" >"$X_PIDFILE"

for _ in $(seq 1 50); do
	if [ -S "/tmp/.X11-unix/X$DISPLAY_NUM" ]; then
		break
	fi
	kill -0 "$X_PID" 2>/dev/null || break
	sleep 0.1
done
if ! kill -0 "$X_PID" 2>/dev/null; then
	rm -f "$X_PIDFILE"
	die "$XEPHYR_BIN exited at once — see $LOGFILE"
fi
if [ ! -S "/tmp/.X11-unix/X$DISPLAY_NUM" ]; then
	stop_pid "$X_PIDFILE"
	die "nested display :$DISPLAY_NUM did not come up — see $LOGFILE"
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
FITSIZE=$SIZE
if [ "$RESIZABLE" = 1 ]; then
	FITSIZE="resizable (starts at $SIZE)"
fi

say "nested display :$DISPLAY_NUM is up in a window on $HOST_DISPLAY: $FITSIZE, window manager: $WM_NAME"
note "log: $LOGFILE"
note "auth: $AUTH_NOTE"
cat <<EOF

Share it (from this machine):
  ${PREFIX}DISPLAY=:$DISPLAY_NUM ./bin/remmote-server -listen :7677 -exec '<command to run>'
  ${PREFIX}DISPLAY=:$DISPLAY_NUM ./bin/remmote-server -listen :7677
  ./bin/remmote-client -server 127.0.0.1:7677

Or answer the questions instead: ./scripts/remmote.sh (it picks up
this cookie on its own)
(Reminder: resizing the window on $HOST_DISPLAY resizes the nested
screen, and so does remmote's -resize-desktop.)
EOF

if [ "$DETACH" = 1 ]; then
	say "Stop it with: $0 --display $DISPLAY_NUM --stop"
	exit 0
fi

cleanup() {
	say ''
	say "stopping nested display :$DISPLAY_NUM"
	stop_pid "${WM_PIDFILE:-}"
	stop_pid "${X_PIDFILE:-}"
	rm -f "${AUTHFILE:-}"
}
trap 'cleanup; exit 130' INT TERM
say "Stop it with Ctrl-C (or just close the window)."
wait "$X_PID" 2>/dev/null || true
cleanup
