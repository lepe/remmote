#!/usr/bin/env bash
# remmote.sh — interactive launcher for remmote-server / remmote-client.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"

ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
BIN_DIR=${REMOTTE_BIN_DIR:-$ROOT/bin}
DEFAULT_PORT=7677
ROLE=''        # server | client (asked when empty)
PRINT_ONLY=0
ASSUME_YES=0
AUTO_AUTH=''   # cookie picked up from a start-* display, if any
CLIENT_CMD=()  # the connect command shown after the server's (server role)
CLIENT_NOTE=''
CREATED=()     # displays this session started ("backend:display")
KEEP_DISPLAYS=0

description() {
	cat <<'EOF'
remmote.sh — answer a few questions and run remmote-server or
remmote-client with the options you picked. It always prints the exact
command line it built before running it (with --print that is all it
does), so nothing runs that you have not seen. Questions show their
mnemonics — one letter is usually enough (`a` is App, case-insensitive)
— and ports never need a colon: `7677` or `192.168.1.10` will do.

Worth knowing before you start
  * remmote-server shares a screen *and hands over full keyboard and
    mouse control* of it to whoever can reach its port. Without -tls the
    stream is plain and unauthenticated: trusted LAN only, or tunnel it
    (ssh -L 7677:host:7677 user@host, then connect to localhost:7677).
  * -tls has three flavours here: 'encrypt' (encryption only — the
    server is not identified), 'secret' (one value on both sides, which
    also makes the server refuse everyone else), and on the client
    'fingerprint' (pin the server's certificate). The two sides must
    agree on the mode and on the value.
  * The display questions list what is there — ":0, :50, New" — and a
    display number can be typed with or without the colon. New, or a
    number that is not there yet, runs scripts/start-xvfb.sh (headless)
    or scripts/start-xephyr.sh (nested in a window on your desktop) for
    you. Both protect their display with a magic cookie; this script
    finds that cookie and exports it (and prints it with the command
    line, so it works when pasted elsewhere).
EOF
}

usage() {
	description
	cat <<'EOF'

Options:
  --server     go straight to the server questions
  --client     go straight to the client questions
  --print      build and print the command line, do not run it
  -y, --yes    accept the offered defaults for builds and run
  -h, --help   this text

Everything else is asked. Ctrl-C quits without running anything.
EOF
}

while [ $# -gt 0 ]; do
	case $1 in
		--server)  ROLE=server; shift ;;
		--client)  ROLE=client; shift ;;
		--print)   PRINT_ONLY=1; shift ;;
		-y|--yes)  ASSUME_YES=1; shift ;;
		-h|--help) usage; exit 0 ;;
		*)         usage >&2; die "unknown option '$1'" ;;
	esac
done

# ask_display <question> -> $REPLY = a display number. It lists what is
# their unambiguous beginnings.
ask_backend() {
	local a
	if [ "${ASSUME_YES:-0}" = 1 ]; then
		REPLY=xvfb
		return 0
	fi
	while :; do
		printf 'create with xvfb (v) or xephyr (e) [V/e]: '
		read -r a || die 'input ended before an answer arrived'
		if [ -z "$a" ]; then
			a=v
		fi
		case ${a,,} in
			v|xv*) REPLY=xvfb; return 0 ;;
			e|xe*) REPLY=xephyr; return 0 ;;
			*) printf '  v (xvfb, headless) or e (xephyr, nested in a window)\n' ;;
		esac
	done
}

# create_display <backend> <display> — run scripts/start-*.sh detached:
# it asks its own questions (screen size, window manager) and leaves the
# display running afterwards. The display is remembered: it is stopped
# again when the run ends (or the session aborts).
create_display() {
	local backend=$1 disp=$2 script="$SCRIPT_DIR/start-$1.sh"
	if [ ! -x "$script" ]; then
		die "$script is missing or not executable"
	fi
	say "creating display :$disp with $(basename "$script") — it asks the remaining questions"
	if ! "$script" --display "$disp" --detach; then
		die "display :$disp was not created — see the messages above"
	fi
	if [ ! -S "/tmp/.X11-unix/X$disp" ]; then
		die "display :$disp was not created — see the messages above"
	fi
	CREATED+=("$backend:$disp")
	say "display :$disp is up (stop it with: $(basename "$script") --display $disp --stop)"
}

# tidy_displays — stop the displays this session started (their window
# manager and cookie go with them). Idempotent.
tidy_displays() {
	local entry
	if [ "$KEEP_DISPLAYS" = 1 ] || [ "${#CREATED[@]}" -eq 0 ]; then
		return 0
	fi
	printf '\n'
	for entry in "${CREATED[@]}"; do
		say "stopping display :${entry##*:} (started by this session)"
		"$SCRIPT_DIR/start-${entry%%:*}.sh" --display "${entry##*:}" --stop >/dev/null 2>&1 || true
	done
	CREATED=()
}

# created_note — where to stop the displays, when they are kept.
created_note() {
	local entry
	for entry in "${CREATED[@]}"; do
		note "display :${entry##*:} stays up — stop it with: scripts/start-${entry%%:*}.sh --display ${entry##*:} --stop"
	done
}

# ask_display <question> -> $REPLY = a display number. It lists what is
# there — ":0, :50, New" — and takes a display number (with or without
# the colon) or New. New, or a number that is not there yet, creates one:
# xvfb (v) or xephyr (e) first, then a number (free from :88), then
# scripts/start-*.sh runs and asks the rest.
ask_display() {
	local q=$1 list='' mnem='' a n
	list_displays
	local d
	for d in "${DISPLAYS[@]}"; do
		list+="${list:+, }:$d"
		mnem+="${mnem:+/}$d"
	done
	list+="${list:+, }New"
	mnem+="${mnem:+/}n"
	while :; do
		printf '%s (%s) [%s]: ' "$q" "$list" "$mnem"
		read -r a || die 'input ended before an answer arrived'
		if [ -z "$a" ]; then
			printf '  pick a display number, or n for New\n'
			continue
		fi
		case ${a,,} in
			n|new)
				ask_backend
				local backend=$REPLY
				first_free_display
				n=$REPLY
				if [ "${ASSUME_YES:-0}" != 1 ]; then
					ask_match 'display number for it' "$n" '^[0-9]+$' \
						'a display number like 88'
					n=$REPLY
					while [ -S "/tmp/.X11-unix/X$n" ]; do
						warn "display :$n is already in use"
						first_free_display
						ask_match 'display number for it' "$REPLY" '^[0-9]+$' \
							'a display number like 88'
						n=$REPLY
					done
				fi
				create_display "$backend" "$n"
				REPLY=$n
				return 0
				;;
		esac
		if [[ $a =~ ^:?([0-9]+)$ ]]; then
			n=${BASH_REMATCH[1]}
			if [ ! -S "/tmp/.X11-unix/X$n" ]; then
				# Not in the list: New, but with the number given.
				ask_backend
				create_display "$REPLY" "$n"
			fi
			REPLY=$n
			return 0
		fi
		printf '  pick one of: %s\n' "$list"
	done
}

# normalize_listen <value> -> $REPLY = "[:port | host:port]"; the colon
# is optional — "7677" or "127.0.0.1" are enough.
normalize_listen() {
	local v=$1
	if [[ $v =~ ^[0-9]+$ ]]; then
		REPLY=":$v"
	elif [[ $v =~ :[0-9]+$ ]]; then
		REPLY=$v
	else
		REPLY="$v:$DEFAULT_PORT"
	fi
}

# normalize_hostport <value> -> $REPLY = "host:port"; a bare port means
# localhost (the SSH-tunnel case), a bare host takes the default port.
normalize_hostport() {
	local v=$1
	if [[ $v =~ ^[0-9]+$ ]]; then
		REPLY="127.0.0.1:$v"
	elif [[ $v =~ :[0-9]+$ ]]; then
		REPLY=$v
	else
		REPLY="$v:$DEFAULT_PORT"
	fi
}

# ---- binaries ------------------------------------------------------------

# find_binary <server|client> -> $REPLY = path, '' when missing
find_binary() {
	local name="remmote-$1"
	if [ -x "$BIN_DIR/$name" ]; then
		REPLY="$BIN_DIR/$name"
	elif command -v "$name" >/dev/null 2>&1; then
		REPLY=$(command -v "$name")
	else
		REPLY=''
	fi
}

# ensure_binary <server|client> -> $REPLY = path; offers to run make build
ensure_binary() {
	find_binary "$1"
	if [ -n "$REPLY" ]; then
		return 0
	fi
	warn "remmote-$1 is not built (looked in $BIN_DIR and on PATH)"
	note "build it with: make -C $ROOT build"
	if [ "${ASSUME_YES:-0}" = 1 ] || ask_yn 'build it now with make build?' y; then
		command -v make >/dev/null 2>&1 || die 'make is required to build remmote'
		make -C "$ROOT" build
		find_binary "$1"
	fi
	if [ -z "$REPLY" ]; then
		die "remmote-$1 is still missing — run 'make build' and re-run this script"
	fi
}

# ask_secret_checked <question> <min-length> -> $REPLY
ask_secret_checked() {
	local q=$1 min=$2
	while :; do
		ask_secret "$q"
		if [ "${#REPLY}" -ge "$min" ]; then
			return 0
		fi
		printf '  use at least %d characters\n' "$min"
	done
}

# ---- server --------------------------------------------------------------

# build_client_hint <listen> <tls-mode> <tls-secret> <downscale> <clip>
# — CLIENT_CMD / CLIENT_NOTE: what the viewer runs to connect, mirroring
# every choice just made for the server (address, the matching -tls, the
# -upscale that fits -downscale, clipboard).
build_client_hint() {
	local listen=$1 mode=$2 secret=$3 downscale=$4 clip=$5
	local host=${listen%:*} port=${listen##*:} client_bin
	find_binary client
	client_bin=${REPLY:-remmote-client}
	CLIENT_NOTE=''
	case $host in
		''|0.0.0.0|'[::]')
			host=$(hostname -I 2>/dev/null | awk '{print $1}')
			if [ -z "$host" ]; then
				host=$(hostname 2>/dev/null)
			fi
			;;
		127.*|localhost|'[::1]')
			CLIENT_NOTE="the server listens on $host only — reach it through a tunnel first: ssh -L $port:localhost:$port user@$(hostname 2>/dev/null)"
			;;
	esac
	CLIENT_CMD=("$client_bin" -server "${host:-localhost}:$port")
	case $mode in
		encrypt) CLIENT_CMD+=(-tls) ;;
		secret)  CLIENT_CMD+=(-tls "$secret") ;;
	esac
	if [ "$downscale" != 1 ]; then
		CLIENT_CMD+=(-upscale "$downscale")
	fi
	if [ "$clip" = 0 ]; then
		CLIENT_CMD+=(-no-clipboard)
	fi
}

build_server_args() {
	local bin=$1 display mode
	local listen='' tls_mode=off tls_secret='' downscale=1 clip=1

	head1 'remmote-server — share this machine'
	ask_display 'X display to share'
	display=$REPLY
	use_display_auth "$display"

	ARGS=("$bin" -display ":$display")

	choose 'Share what?' desktop desktop app window
	mode=$REPLY
	case $mode in
		desktop)
			if ask_yn 'let viewers resize this screen? (-resize-desktop)' n; then
				ARGS+=(-resize-desktop)
			fi
			;;
		app)
			ask "command to run and share (only its windows are shown, e.g. 'xcalc')" ''
			ARGS+=(-exec "$REPLY")
			;;
		window)
			ask_match 'window id to share (hex, from xwininfo)' '' '^(0[xX])?[0-9a-fA-F]+$' \
				'a window id looks like 0x2c00005'
			ARGS+=(-window "$REPLY")
			;;
	esac
	if [ "$mode" != desktop ]; then
		if ask_yn 'maximize the shared window onto the host screen? (-maximize)' n; then
			ARGS+=(-maximize)
		fi
	fi

	ask_match 'listen address (port, or address:port)' "$DEFAULT_PORT" '^[^[:space:]]+$' \
		'a port like 7677, or address:port'
	normalize_listen "$REPLY"
	listen=$REPLY

	ARGS+=(-listen "$listen")

	if ask_yn 'extra options: codec, fps, quality, downscale, keyframes?' n; then
		choose 'encoder codec (webp needs a -tags webp build)' 'hybrid' hybrid zraw jpeg webp
		ARGS+=(-codec "$REPLY")
		ask_range 'maximum frames per second' 60 1 240
		ARGS+=(-fps "$REPLY")
		ask_range 'encoder quality (ZRAW ignores it)' 75 1 100
		ARGS+=(-quality "$REPLY")
		choose 'downscale the stream (less detail, less bandwidth)' 1 1 2 4
		downscale=$REPLY
		ARGS+=(-downscale "$downscale")
		ask_match 'keyframe interval (like 2s, 500ms, 1m)' '2s' '^[0-9]+([.][0-9]+)?(ms|s|m|h)$'
		ARGS+=(-refresh "$REPLY")
	fi

	if ! ask_yn 'clipboard sync between the two machines?' y; then
		clip=0
		ARGS+=(-no-clipboard)
	fi

	head1 'Encryption and authentication (-tls)'
	choose 'mode' off off encrypt secret
	tls_mode=$REPLY
	case $tls_mode in
		off)
			note 'plain and unauthenticated — trusted LAN or a tunnel only'
			;;
		encrypt)
			ARGS+=(-tls)
			note 'encrypted, but clients cannot tell this server from an impostor'
			;;
		secret)
			local secret
			if command -v openssl >/dev/null 2>&1 && ask_yn 'generate a strong secret with openssl?' y; then
				secret=$(openssl rand -hex 16)
				say "secret: $secret"
				note 'give the client exactly this value — it doubles as admission control'
			else
				ask_secret_checked 'shared secret (at least 8 characters)' 8
				secret=$REPLY
				note 'the client needs exactly this value'
			fi
			tls_secret=$secret
			ARGS+=(-tls "$tls_secret")
			;;
	esac

	build_client_hint "$listen" "$tls_mode" "$tls_secret" "$downscale" "$clip"
}

# ---- client --------------------------------------------------------------

build_client_args() {
	local bin=$1

	head1 'remmote-client — view and control a shared screen'
	ask_match 'server to connect to (host, host:port, or just its port)' '' '^[^[:space:]]+$' \
		'a host name or address like 192.168.1.10 — the port defaults to 7677'
	normalize_hostport "$REPLY"
	ARGS=("$bin" -server "$REPLY")

	head1 'Encryption (-tls) — must match the server'
	choose 'mode' off off encrypt secret fingerprint
	case $REPLY in
		off)
			note 'plain and unauthenticated — trusted LAN or a tunnel only'
			;;
		encrypt)
			ARGS+=(-tls)
			note 'encrypted, but the server is not identified'
			;;
		secret)
			ask_secret_checked 'shared secret (the server was started with)' 8
			ARGS+=(-tls "$REPLY")
			;;
		fingerprint)
			ask_match 'server certificate fingerprint' '' '^SHA256:[A-Za-z0-9+/=_-]+$' \
				"it looks like SHA256:48bc… — the server logs it when it starts with -tls"
			ARGS+=(-tls "$REPLY")
			;;
	esac

	note '(the viewer window opens there — pick a display you can see)'
	ask_display 'X display for the viewer window'
	use_display_auth "$REPLY"
	ARGS+=(-display ":$REPLY")

	if ask_yn 'extra options: upscale, fast scaling, quality?' n; then
		choose 'magnify a downscaled stream back to host resolution' 1 1 2 4
		if [ "$REPLY" != 1 ]; then
			ARGS+=(-upscale "$REPLY")
		fi
		if ask_yn 'fast (nearest-neighbour) scaling? cheaper CPU, rougher edges' n; then
			ARGS+=(-fast-scale)
		fi
		ask_range 'quality override (0 = keep the server default)' 0 0 100
		if [ "$REPLY" -gt 0 ]; then
			ARGS+=(-quality "$REPLY")
		fi
	fi

	if ! ask_yn 'clipboard sync between the two machines?' y; then
		ARGS+=(-no-clipboard)
	fi
}

# ---- go ------------------------------------------------------------------

# Displays this session started are stopped when it ends: after the run,
# or when the session is aborted (Ctrl-C). A deliberate --print or "not
# run" keeps them — the printed command still needs one to exist.
trap 'exit 130' INT
trap 'exit 143' TERM
trap tidy_displays EXIT

if [ -z "$ROLE" ]; then
	head1 'What should this machine do?'
	choose 'role' '' server client
	ROLE=$REPLY
fi

ensure_binary "$ROLE"
BIN=$REPLY

case $ROLE in
	server) build_server_args "$BIN" ;;
	client) build_client_args "$BIN" ;;
esac

head1 'Command'
if [ -n "${AUTO_AUTH:-}" ]; then
	# The exported cookie is part of the recipe: show it, so the line
	# works when pasted into another terminal too.
	printf 'XAUTHORITY=%s ' "$AUTO_AUTH"
fi
print_cmd "${ARGS[@]}"

if [ "$ROLE" = server ] && [ "${#CLIENT_CMD[@]}" -gt 0 ]; then
	say ''
	say 'Connect from the client (run this there):'
	client_line=$(print_cmd "${CLIENT_CMD[@]}")
	print_box 'client' "${client_line% }"
	if [ -n "$CLIENT_NOTE" ]; then
		note "$CLIENT_NOTE"
	fi
fi

# run_and_tidy — run the command as a child (Ctrl-C reaches it as usual)
# and stop the displays this session started once it is gone.
run_and_tidy() {
	local status=0 pid
	say 'running — Ctrl-C stops it (and the displays this session started)'
	"${ARGS[@]}" &
	pid=$!
	trap 'kill -INT "$pid" 2>/dev/null' INT TERM
	while kill -0 "$pid" 2>/dev/null; do
		wait "$pid" || status=$?
	done
	trap - INT TERM
	exit "$status" # the EXIT trap stops the displays
}

if [ "$PRINT_ONLY" = 1 ]; then
	KEEP_DISPLAYS=1
	created_note
	exit 0
fi
if [ "${ASSUME_YES:-0}" = 1 ] || ask_yn 'run it now?' y; then
	if [ "${#CREATED[@]}" -eq 0 ]; then
		say 'running — Ctrl-C stops it'
		exec "${ARGS[@]}"
	fi
	run_and_tidy
fi
say 'not started'
KEEP_DISPLAYS=1
created_note
