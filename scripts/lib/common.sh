# shellcheck shell=bash
# Shared helpers for remmote's interactive scripts: start-xvfb.sh,
# start-xephyr.sh and remmote.sh. Sourced, never executed.
#
# Everything here is prompt/detection plumbing: questions that keep
# asking until they get an answer, Debian (apt) installation offers for
# missing tools, window-manager discovery, display bookkeeping and the
# magic-cookie handling that keeps a created display private.

if [ -t 1 ]; then
	C_BOLD=$'\033[1m'
	C_DIM=$'\033[2m'
	C_RED=$'\033[31m'
	C_YEL=$'\033[33m'
	C_OFF=$'\033[0m'
else
	C_BOLD='' C_DIM='' C_RED='' C_YEL='' C_OFF=''
fi

say()  { printf '%s\n' "$*"; }
note() { printf '%s%s%s\n' "$C_DIM" "$*" "$C_OFF"; }
head1() { printf '\n%s%s%s\n' "$C_BOLD" "$*" "$C_OFF"; }
warn() { printf '%s!%s %s\n' "$C_YEL" "$C_OFF" "$*" >&2; }
die()  { printf '%serror:%s %s\n' "$C_RED" "$C_OFF" "$*" >&2; exit 1; }

# ask <question> [default] -> $REPLY. Without a default an empty answer
# is refused, for values the caller cannot sensibly guess.
ask() {
	local q=$1 def=${2-} a
	while :; do
		if [ -n "$def" ]; then
			printf '%s [%s]: ' "$q" "$def"
		else
			printf '%s: ' "$q"
		fi
		read -r a || die 'input ended before an answer arrived'
		if [ -z "$a" ]; then
			a=$def
		fi
		if [ -n "$a" ]; then
			REPLY=$a
			return 0
		fi
		printf '  an answer is required\n'
	done
}

# ask_yn <question> [y|n default] -> 0 = yes
ask_yn() {
	local q=$1 def=${2:-n} hint a
	while :; do
		case $def in
			y|Y) hint='[Y/n]' ;;
			*)   def=n; hint='[y/N]' ;;
		esac
		printf '%s %s: ' "$q" "$hint"
		read -r a || die 'input ended before an answer arrived'
		if [ -z "$a" ]; then
			a=$def
		fi
		case ${a,,} in
			y|yes) return 0 ;;
			n|no)  return 1 ;;
		esac
		printf '  please answer y or n\n'
	done
}

# choose <question> <default-or-empty> <option...> -> $REPLY (the chosen
# option word). The prompt lists the options as words with the shortest
# unambiguous prefix of each as mnemonic, the default's in capitals:
#
#   Share what? (Desktop, App, Window) [D/a/w]:
#   mode (Off, Encrypt, Secret) [O/e/s]:
#
# Typing is case-insensitive and only needs that prefix — or anything
# starting with a unique option's first letter: d, D, desk or Desktop all
# pick Desktop, and app, Ap or apple all pick App. An empty answer takes
# the default.
choose() {
	local q=$1 def=$2
	shift 2
	local opts=("$@") mnems=() list='' hint='' a low o ol pre n i j count hits
	for i in "${!opts[@]}"; do
		o=${opts[$i]}
		ol=${o,,}
		# shortest prefix that names this option alone
		pre=$ol
		for ((n = 1; n <= ${#ol}; n++)); do
			pre=${ol:0:n}
			count=0
			for j in "${!opts[@]}"; do
				if [[ ${opts[$j],,} == "$pre"* ]]; then
					count=$((count + 1))
				fi
			done
			if [ "$count" = 1 ]; then
				break
			fi
		done
		mnems[$i]=$pre
		list+="${list:+, }${o^}"
	done
	for i in "${!opts[@]}"; do
		pre=${mnems[$i]}
		if [ "${opts[$i]}" = "$def" ]; then
			pre=${pre^}
		fi
		hint+="${hint:+/}$pre"
	done

	while :; do
		printf '%s (%s) [%s]: ' "$q" "$list" "$hint"
		read -r a || die 'input ended before an answer arrived'
		if [ -z "$a" ]; then
			if [ -n "$def" ]; then
				REPLY=$def
				return 0
			fi
			printf '  an answer is required (one of: %s)\n' "$list"
			continue
		fi
		low=${a,,}
		hits=()
		for o in "${opts[@]}"; do
			if [[ ${o,,} == "$low"* ]]; then
				hits+=("$o")
			fi
		done
		if [ "${#hits[@]}" -eq 0 ]; then
			# Anything starting with a unique option's first letter
			# counts, so "apple" means App and "win" means Window.
			for o in "${opts[@]}"; do
				ol=${o,,}
				if [ "${ol:0:1}" = "${low:0:1}" ]; then
					hits+=("$o")
				fi
			done
		fi
		case ${#hits[@]} in
			1)
				REPLY=${hits[0]}
				return 0
				;;
			0)
				printf '  %s is not one of: %s\n' "$a" "$list"
				;;
			*)
				printf "  '%s' could be %s — use the mnemonics: %s\n" "$a" "${hits[*]}" "$hint"
				;;
		esac
	done
}

# ask_match <question> <default> <regex> [hint] -> $REPLY (keeps asking)
ask_match() {
	local q=$1 def=$2 re=$3 hint=${4:-'that does not look right'}
	while :; do
		ask "$q" "$def"
		if [[ $REPLY =~ $re ]]; then
			return 0
		fi
		printf '  %s\n' "$hint"
	done
}

# ask_range <question> <default> <min> <max> -> $REPLY (keeps asking)
ask_range() {
	local q=$1 def=$2 lo=$3 hi=$4
	while :; do
		ask_match "$q" "$def" '^[0-9]+$' 'a whole number, please'
		if [ "$REPLY" -ge "$lo" ] && [ "$REPLY" -le "$hi" ]; then
			return 0
		fi
		printf '  between %d and %d\n' "$lo" "$hi"
	done
}

# ask_secret <question> -> $REPLY (typed without echo on a terminal)
ask_secret() {
	local q=$1 a
	while :; do
		if [ -t 0 ]; then
			printf '%s: ' "$q"
			read -rs a || die 'input ended before an answer arrived'
			printf '\n'
		else
			ask "$q" ''
			a=$REPLY
		fi
		if [ -n "$a" ]; then
			REPLY=$a
			return 0
		fi
		printf '  an answer is required\n'
	done
}

# ---- missing tools -------------------------------------------------------

if [ "$(id -u)" = 0 ]; then
	RUN_AS_ROOT=''
else
	RUN_AS_ROOT='sudo'
fi

# install_pkg <debian-package> — apt only for now; other distributions get
# the package name and a friendly refusal.
install_pkg() {
	local pkg=$1
	command -v apt-get >/dev/null 2>&1 || die \
		"automatic installation covers Debian/Ubuntu (apt) for now — install '$pkg' with your package manager and re-run"
	if [ -n "$RUN_AS_ROOT" ]; then
		command -v sudo >/dev/null 2>&1 || die "sudo is required to install '$pkg'"
	fi
	say "installing '$pkg' with apt..."
	$RUN_AS_ROOT apt-get install -y "$pkg" || die "installing '$pkg' failed — install it yourself and re-run"
}

# ensure_command <command> <debian-package> <what-it-is-for>
# Detects <command>; when missing, says what it is for and offers the
# Debian package. --yes (ASSUME_YES=1) installs without asking.
ensure_command() {
	local cmd=$1 pkg=$2 what=$3
	if command -v "$cmd" >/dev/null 2>&1; then
		return 0
	fi
	warn "$cmd is not installed — $what"
	note "on Debian/Ubuntu the package is '$pkg'"
	if [ "${ASSUME_YES:-0}" = 1 ]; then
		install_pkg "$pkg"
	elif ask_yn "install '$pkg' now?" n; then
		install_pkg "$pkg"
	else
		die "$cmd is required — install '$pkg' and re-run"
	fi
	command -v "$cmd" >/dev/null 2>&1 || die "$cmd is still missing after installing '$pkg'"
}

# ---- window managers -----------------------------------------------------

WM_CANDIDATES=(openbox fluxbox marco mutter xfwm4 i3 awesome blackbox icewm jwm pekwm matchbox-window-manager twm)

# wm_package <command> -> $REPLY = the Debian package that provides it.
wm_package() {
	case $1 in
		i3) REPLY='i3-wm' ;;
		*)  REPLY=$1 ;;
	esac
}

# detect_wms -> WM_FOUND / WM_MISSING arrays.
detect_wms() {
	WM_FOUND=()
	WM_MISSING=()
	local wm
	for wm in "${WM_CANDIDATES[@]}"; do
		if command -v "$wm" >/dev/null 2>&1; then
			WM_FOUND+=("$wm")
		else
			WM_MISSING+=("$wm")
		fi
	done
}

# pick_wm <default> -> $REPLY = window manager command, or "none".
# Shows which candidates are installed; offers to install one that is
# not (same Debian-only install path as ensure_command).
pick_wm() {
	local def=${1:-none} name pkg
	detect_wms
	say "window managers installed: ${WM_FOUND[*]:-none}"
	note "not installed: ${WM_MISSING[*]:-}"
	while :; do
		ask "window manager to launch inside (or 'none')" "$def"
		name=$REPLY
		case ${name,,} in
			none|-|no|off)
				REPLY=none
				return 0
				;;
		esac
		if command -v "$name" >/dev/null 2>&1; then
			REPLY=$name
			return 0
		fi
		wm_package "$name"
		pkg=$REPLY
		warn "'$name' is not installed"
		note "on Debian/Ubuntu the package is '$pkg'"
		if [ "${ASSUME_YES:-0}" = 1 ]; then
			install_pkg "$pkg"
		elif ask_yn "install '$pkg' now?" n; then
			install_pkg "$pkg"
		else
			say "pick one of the installed ones above, or 'none'"
			continue
		fi
		command -v "$name" >/dev/null 2>&1 || die "'$name' is still missing after installing '$pkg'"
		REPLY=$name
		return 0
	done
}

# ---- displays ------------------------------------------------------------

# ---- displays ------------------------------------------------------------

# list_displays -> DISPLAYS: the X display numbers with a socket.
list_displays() {
	local f
	DISPLAYS=()
	for f in /tmp/.X11-unix/X*; do
		[ -e "$f" ] || continue
		f=${f##*/X}
		if [[ $f =~ ^[0-9]+$ ]]; then
			DISPLAYS+=("$f")
		fi
	done
	if [ "${#DISPLAYS[@]}" -gt 1 ]; then
		mapfile -t DISPLAYS < <(printf '%s\n' "${DISPLAYS[@]}" | sort -n)
	fi
}

# first_free_display -> $REPLY = the lowest free display number from 88.
first_free_display() {
	local n
	for n in $(seq 88 999); do
		if [ ! -S "/tmp/.X11-unix/X$n" ]; then
			REPLY=$n
			return 0
		fi
	done
	die 'no free display number between :88 and :999'
}

# can_open <display> [authfile] — can this display be reached (with that
# cookie file, or the current environment when empty)? True when xdpyinfo
# is missing and the question cannot be asked.
can_open() {
	local disp=$1 file=${2-}
	if ! command -v xdpyinfo >/dev/null 2>&1; then
		return 0
	fi
	if [ -n "$file" ]; then
		XAUTHORITY=$file xdpyinfo -display ":$disp" >/dev/null 2>&1
	else
		xdpyinfo -display ":$disp" >/dev/null 2>&1
	fi
}

# use_display_auth <display> — make a display openable before running
# against it. start-xvfb.sh / start-xephyr.sh protect their displays with
# a magic cookie and leave it next to their pid files; when the display
# needs it and the current environment has none, export that cookie (and
# say so, since the printed command line then needs the same prefix).
use_display_auth() {
	local disp=$1 dir file
	if [ ! -S "/tmp/.X11-unix/X$disp" ]; then
		die "display :$disp does not exist — create it with scripts/start-xvfb.sh or scripts/start-xephyr.sh"
	fi
	if can_open "$disp"; then
		return 0
	fi
	for dir in "${XDG_RUNTIME_DIR:-}" "${TMPDIR:-/tmp}"; do
		[ -n "$dir" ] || continue
		for file in "$dir/remmote-xvfb-$disp.Xauthority" "$dir/remmote-xephyr-$disp.Xauthority"; do
			if [ -f "$file" ] && can_open "$disp" "$file"; then
				export XAUTHORITY=$file
				AUTO_AUTH=$file
				say "display :$disp needs its cookie — using XAUTHORITY=$file"
				return 0
			fi
		done
	done
	die "display :$disp cannot be opened: no working XAUTHORITY found (start-xvfb.sh / start-xephyr.sh print the one they made, usually ${XDG_RUNTIME_DIR:-/tmp}/remmote-xvfb-$disp.Xauthority)"
}

# normalize_display <":88" | "88"> -> $REPLY = "88"
normalize_display() {
	local raw=${1-} v=${1-}
	v=${v#:}
	case $v in
		''|*[!0-9]*) die "display must be a number, like 88 or :88 (got '$raw')" ;;
	esac
	if [ "$v" -gt 999 ]; then
		die "display number out of range 0..999: '$raw'"
	fi
	REPLY=$v
}

# display_state <number> -> $REPLY = free | busy | stale
# A socket without a server answering is most likely stale — but a live
# server that uses a cookie also does not answer, so the message callers
# print has to allow for both.
display_state() {
	local n=$1 sock="/tmp/.X11-unix/X$1"
	if [ ! -S "$sock" ]; then
		REPLY=free
		return 0
	fi
	if command -v xdpyinfo >/dev/null 2>&1 && xdpyinfo -display ":$n" >/dev/null 2>&1; then
		REPLY=busy
	else
		REPLY=stale
	fi
}

# claim_display <number> — refuses a busy display, offers to clear a
# stale one.
claim_display() {
	local n=$1
	display_state "$n"
	case $REPLY in
		free)
			return 0
			;;
		busy)
			die "display :$n is already in use — stop it or choose another number"
			;;
		stale)
			warn "display :$n has a socket but no server answered (/tmp/.X11-unix/X$n)"
			note "it may be a dead server's stale socket, or a live one using a cookie"
			if [ "${ASSUME_YES:-0}" = 1 ] || ask_yn "remove the socket and continue?" n; then
				rm -f "/tmp/.X11-unix/X$n" || die "cannot remove /tmp/.X11-unix/X$n (not yours?)"
			else
				die "choose another display number"
			fi
			;;
	esac
}

# ---- processes and runtime files -----------------------------------------

# runtime_dir -> $REPLY = per-user directory for pid/log/cookie files.
runtime_dir() {
	local d="${XDG_RUNTIME_DIR:-}"
	if [ -n "$d" ] && [ -d "$d" ] && [ -w "$d" ]; then
		REPLY=$d
	else
		REPLY="${TMPDIR:-/tmp}"
	fi
}

# spawn <logfile> <command...> -> $REPLY = pid of a detached session leader
spawn() {
	local log=$1
	shift
	setsid "$@" >>"$log" 2>&1 &
	REPLY=$!
}

# stop_pid <pidfile> — TERM, a brief wait, then KILL. Idempotent.
stop_pid() {
	local file=$1 pid i
	[ -f "$file" ] || return 0
	pid=$(cat "$file" 2>/dev/null) || pid=''
	rm -f "$file"
	[ -n "$pid" ] || return 0
	kill -0 "$pid" 2>/dev/null || return 0
	kill "$pid" 2>/dev/null || true
	for i in 1 2 3 4 5 6 7 8 9 10; do
		kill -0 "$pid" 2>/dev/null || return 0
		sleep 0.2
	done
	kill -9 "$pid" 2>/dev/null || true
}

# ---- X cookies -----------------------------------------------------------

# make_cookie <authfile> <":display"> — writes a fresh magic cookie so the
# display is private to whoever holds the file. 1 when xauth is missing.
make_cookie() {
	local file=$1 disp=$2 cookie
	command -v xauth >/dev/null 2>&1 || return 1
	( umask 077 && : >"$file" ) || return 1
	if command -v mcookie >/dev/null 2>&1; then
		cookie=$(mcookie)
	else
		cookie=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
	fi
	xauth -f "$file" add "$disp" . "$cookie" >/dev/null 2>&1 || return 1
	return 0
}

# print_box <title> <line...> — the lines inside a labelled box:
#
#   ┌─ client (on the viewer) ─────────────────────────────────────┐
#   │ remmote-client -server 192.168.1.10:7677 -tls d4b3…          │
#   └──────────────────────────────────────────────────────────────┘
print_box() {
	local title=$1 line w
	shift
	w=${#title}
	for line in "$@"; do
		if [ "${#line}" -gt "$w" ]; then
			w=${#line}
		fi
	done
	# Note: no `tr` here — it is byte-based and mangles UTF-8 rules.
	local bar='' pad='' i
	for ((i = 0; i < w + 2; i++)); do
		bar+='─'
	done
	for ((i = 0; i < w - ${#title} + 1; i++)); do
		pad+='─'
	done
	printf '┌─%s%s┐\n' "$title" "$pad"
	for line in "$@"; do
		printf '│ %-*s │\n' "$w" "$line"
	done
	printf '└%s┘\n' "$bar"
}

# print_cmd <command...> — the exact command line, shell-quoted.
print_cmd() {
	local out='' a
	for a in "$@"; do
		out+=$(printf '%q ' "$a")
	done
	printf '%s\n' "$out"
}
