# Interactive helper scripts (xvfb / xephyr / remmote)

**Goal:** three interactive scripts under `scripts/` — create a display to
share (headless with Xvfb, or nested in a window with Xephyr) and launch
remmote-server/-client by answering questions instead of assembling flags.

## Tasks

- [x] 1. `scripts/lib/common.sh`: prompts (`ask`/`ask_yn`/`choose`/`ask_secret`), `say`/`warn`/`die`, Debian/apt install offer, WM detection + selection, display-number parse/free/stale check, cookie helper → `bash -n`
- [x] 2. `scripts/start-xvfb.sh`: banner + limitations, Xvfb detect/install (`--xvfb-bin` override), display/size/WM prompts, cookie auth or `--no-auth`, start / `--detach` / `--stop`, remmote usage hints → created :85/:86, verified with `xdpyinfo` + `remmote-server -dump-frame`
- [x] 3. `scripts/start-xephyr.sh`: same skeleton; Xephyr facts (`-screen`, `-resizeable`, `-host-cursor`, `-no-host-grab`), needs a host `$DISPLAY`, limitations (nested screen follows the window; `-resize-desktop` cannot reach it) → detection + install-prompt paths tested (Xephyr install needs a sudo password here), full flow smoke-tested through a Xvfb-backed `Xephyr` stub
- [x] 4. `scripts/remmote.sh`: interactive launcher — role question, binary discovery (+ build offer), per-role questions (`-exec`/`-window`/`-maximize`/`-resize-desktop`/`-tls` modes, extra knobs behind one prompt), assembles argv, shows and runs it (`--print` to only show) → `--print` for both roles and all three TLS modes
- [x] 5. README: reference the three scripts (Usage + your own display) → `make lint`
- [x] 6. Verification: `bash -n` everywhere, run each script's non-destructive paths with piped answers, full `start-xvfb.sh` + `remmote.sh` round trip

## Done when

- [x] `start-xvfb.sh` creates a display that remmote-server can capture.
- [x] `start-xephyr.sh` and `start-xvfb.sh` explain themselves first, detect
  or offer to install their X server and the chosen window manager, and
  accept a custom display number.
- [x] `remmote.sh` prints (and runs) correct `remmote-server`/`remmote-client`
  command lines for the answers given.

## Notes (bugs the tests caught)

- **`choose` never matched**: options were passed as one string
  (`'server client'`) while it compared against `"$@"` — split them into
  separate arguments at every call site.
- **Prompts spun forever at EOF** (pipes, closed stdin): `read` now dies
  with "input ended before an answer arrived" instead of looping.
- **pid/log/cookie filenames were built before the display number was
  known** in interactive runs (`remmote-xvfb-.pid`), so `--stop` could
  not find them: `set_paths` now runs after the prompt too.
- **`remmote.sh` ran against a cookie-protected display without the
  cookie** (reproduced: `x protocol authentication refused`): it now
  finds the `remmote-xvfb-N.Xauthority` / `remmote-xephyr-N.Xauthority`
  that the start-* scripts leave behind, exports it, and prints it as
  part of the command line so the pasted line works elsewhere.
- **New in `remmote.sh`**: naming a display that does not exist offers
  to create it by running `start-xvfb.sh` / `start-xephyr.sh` inline
  (detached, so their own questions for size/window manager come up and
  the display keeps running afterwards); `--yes` picks xvfb without
  asking. Verified for both backends and for the decline path.
- **Short answers** (choose): prompts render as
  `Share what? (Desktop, App, Window) [D/a/w]:` and `mode (Off, Encrypt,
  Secret) [O/e/s]:` — capitals mark the default's mnemonic, matching is
  case-insensitive on the mnemonic or any word starting with a unique
  option's first letter (a/A/Ap/app/apple all mean App), and ambiguous
  input (`x` for xvfb/xephyr) re-asks showing `Xv/xe`.
- **Colons optional for ports**: `7677` → `:7677` (listen), `host` →
  `host:7677`, and a bare port means `127.0.0.1:port` on the client
  (the SSH-tunnel case). Caught on the way: address regexes with `\[`
  inside a bracket expression reject everything (the `]` closes the
  class early) — the prompts now use `^[^[:space:]]+$`.
- **Display picker**: the display questions list what is there —
  `X display to share (:0, :50, New) [0/50/n]:` — take a number with or
  without the colon, or New (or a number that is not there yet, which is
  New with that number). New asks `create with xvfb (v) or xephyr (e)
  [V/e]:`, then a display number (first free from :88), then runs the
  matching start-*.sh. `--yes` picks xvfb and the first free number.
- **Boxed client command** (server role): before running, the server's
  command is shown and then the `remmote-client` command that connects
  to it, inside a box (`print_box`, built in bash — `tr` is byte-based
  and mangles UTF-8 rules). It mirrors the choices made: this machine's
  address (from `hostname -I`) or `127.0.0.1` with a ready-made
  `ssh -L` line when the server listens on localhost only, the same
  `-tls` mode/secret, `-upscale` matching `-downscale`, and
  `-no-clipboard` when clipboard sync is off.
- **Displays it starts, it stops**: a display created during the session
  is remembered and stopped again (window manager and cookie too) when
  the run ends — Ctrl-C included — or when the session aborts mid-way
  (`trap tidy_displays EXIT`). A deliberate `--print` or "run it now? n"
  keeps it alive, because the printed command still needs it, and says
  where to stop it. With a created display the command runs as a child
  (`run_and_tidy`) instead of `exec`, and Ctrl-C is forwarded to it.
- **Xephyr resize forensics** (from its source, since it cannot be
  installed here): its nested screen follows its window 1:1
  (`-resizeable` → `ephyrResizeScreen`), and `ephyrRandRSetConfig` takes
  *any* size — including one that resizes the window along with it. Two
  traps made "resize later" dead-end at 320x240: (a)
  `ephyrResizeScreen` ends with `RROutputSetModes(output, NULL, 0, 0)`,
  wiping the output's mode list — and remmote's `ResizeScreen` picks
  modes from exactly that list, so every later resize found "no mode
  fits"; (b) remmote clamped requests to `GetScreenSizeRange`'s max,
  which on nested servers tracks the *current* size — so the screen
  could only ever shrink (640x400 and 320x240 are entries in Xephyr's
  fixed mode list). Fixes in `internal/xwin/screen.go`: the range clamp
  is gone, outputs with no modes fall back to the screen's mode list,
  and such "virtual" outputs get an exact-size mode created on demand
  (`RRCreateMode` + `RRAddOutputMode`, cached per size) with the
  advertised list as fallback if the server refuses made-up modes —
  never tried on a real monitor, which could be driven out of range.
- **Xephyr asks for its host session** instead of requiring `$DISPLAY`:
  `host display (the window opens here) (:0, :50) [0/50]:` lists the
  displays that are up (Enter takes `$DISPLAY` when there is one);
  `--host-display` and `--yes` skip the question, and the "none is
  running" error remains for a machine without any X session. It also
  picks up a host display's cookie, so Xephyr can nest inside a
  cookie-protected start-xvfb display.
- **In-use display numbers are refused** in the New flow ("display :0
  is already in use" → re-ask) — it used to reach start-*.sh and fail
  late, after its questions. Shared display helpers (`list_displays`,
  `first_free_display`, `can_open`, `use_display_auth`) moved from
  remmote.sh to lib/common.sh.
- Xephyr's real options (from the Debian man page): `-screen WxH`,
  `-resizeable`, `-host-cursor`, `-no-host-grab` — its window is fixed
  size unless `-resizeable`, and the nested screen follows the window.
