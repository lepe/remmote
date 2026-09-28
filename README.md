# remmote

Pure-Go (CGo-free by default) Linux X11 screen sharing with **full remote
control** between two machines. One binary captures and controls; the other
views and drives.

- **`remmote-server`** — runs on the **host** (the machine to view/control).
  Tracks screen changes with **XDamage**, grabs pixels through **MIT-SHM**
  (with an automatic core-protocol fallback), encodes only the changed
  regions as JPEG (optionally lossy **WebP**), and injects keyboard and
  mouse events via **XTEST** — including VNC-style synthetic-Shift
  choreography for capital letters and symbols.
- **`remmote-client`** — runs on the **viewer**. Opens its own X11 window
  (created directly with the same pure-Go X bindings — no GUI toolkit),
  composites the stream, rescales live when you resize the window
  (letterbox), and forwards your input with host-screen coordinate
  mapping.
- **Clipboard sync** — copy on either machine, paste on the other
  (UTF-8 text, bidirectional, ≤256 KiB; disable with `-no-clipboard` on
  either side).

Everything is pure Go: `jezek/xgb` for the X protocol, `golang.org/x/sys`
for SysV shared memory, stdlib for TCP/JPEG/logging. The default build has
**zero CGo** and cross-compiles anywhere Go does.

> [!WARNING]
> **No authentication. No encryption.** Anyone who can reach the server's
> TCP port gets live view *and full keyboard/mouse control* of the host.
> Use on a trusted LAN only. For anything else, tunnel the TCP connection
> through SSH or WireGuard:
>
> ```sh
> ssh -L 7677:localhost:7677 user@host   # then: remmote-client -server localhost:7677
> ```

## Build

Requires Go ≥ 1.22. Nothing else.

```sh
make build          # → bin/remmote-server, bin/remmote-client (pure Go, JPEG)
```

### Optional: lossy WebP codec

WebP encodes ~25–35% smaller than JPEG at similar quality. The encoder is
libwebp (C), so it is kept out of the default build behind a build tag:

```sh
sudo apt install libwebp-dev     # Ubuntu/Debian
make build-webp                  # → same binaries + WebP, run server with -codec webp
```

The client always decodes both JPEG and WebP (pure Go), regardless of how
it was built. A server built without the tag refuses `-codec webp` at
startup — never mid-stream.

## Usage

Share the whole desktop — on the **host**:

```sh
./bin/remmote-server -display :0 -listen :7677
```

Share a single application instead (spawned by the server, killed with it):

```sh
./bin/remmote-server -display :0 -listen :7677 -exec xcalc
```

Share an existing window (find ids with `xwininfo`):

```sh
./bin/remmote-server -display :0 -listen :7677 -window 0x2c00005
```

On the **viewer**:

```sh
./bin/remmote-client -server 192.168.1.10:7677
```

That's it — move the mouse over the window and type. Close the viewer
window to disconnect (the client also auto-reconnects if the network
drops).

### Window mode (`-exec` / `-window`)

The viewer then sees a mini-desktop that is exactly the application: the
canvas is the bounding box of the application's windows at their on-screen
positions. Windows join the set when they are transient for a tracked
window (dialogs, menus), carry the app's `_NET_WM_PID`, or share its
`WM_CLASS`. Menus drawn inside a window are captured automatically. The
canvas resizes (with a keyframe) when windows move, resize, appear, or
close. Input: moving over the viewer moves the host pointer; clicks and
keystrokes raise and focus the target window first so they cannot land on
something stacked above; clicks in the gaps between windows are ignored.

When the application exits the **server stays up** and keeps serving the
last frame; restarting the share means restarting the command. Shutting
the server down kills the spawned process group.

Limitations: override-redirect popups that match none of the membership
rules are not captured; on non-compositing window managers, regions
obscured by unrelated windows may show stale pixels until the next
keyframe (2 s).

### remmote-server flags

| Flag | Default | Meaning |
|---|---|---|
| `-display` | `$DISPLAY` | X display to capture and control |
| `-listen` | `:7677` | TCP listen address |
| `-fps` | `30` | max frames/s (also the damage merge window) |
| `-codec` | `jpeg` | `jpeg` or `webp` (webp needs a `-tags webp` build) |
| `-quality` | `75` | encoder quality 1-100 (clients may override live) |
| `-refresh` | `2s` | periodic full-frame keyframe interval |
| `-dump-frame` | — | capture one frame to a PNG and exit (diagnostics) |
| `-test-inject` | — | move the pointer and type `a`, then exit (diagnostics) |
| `-exec` | — | run this command and share only its windows (e.g. `-exec xcalc`) |
| `-window` | — | share this existing window id (hex) and windows it spawns |
| `-no-clipboard` | off | disable clipboard synchronization |
| `-v` / `-log-json` | off | debug level / JSON logs |

### remmote-client flags

| Flag | Default | Meaning |
|---|---|---|
| `-display` | `$DISPLAY` | X display for the viewer window |
| `-server` | (required) | `host:port` |
| `-quality` | `0` | 1-100: ask the server to change quality; 0 = keep default |
| `-once` | off | exit after the first keyframe (no window; CI mode) |
| `-snapshot` | — | with `-once`: write the first full frame as a PNG |
| `-no-clipboard` | off | disable clipboard synchronization |
| `-v` / `-log-json` | off | as above |

## How it works

```
┌────────────── host ──────────────┐        ┌──────────── viewer ─────────────┐
│ XDamage ──▶ dirty-rect merge ──▶ │        │  TCP ◀─ RectUpdate (JPEG/WebP) │
│ MIT-SHM GetImage (ZPixmap 32bpp) │  TCP   │  decode → canvas composite     │
│ BGRA→RGBA → encode → broadcast ──┼──────▶ │  scale-to-fit → SHM PutImage   │
│ XTEST FakeInput ◀── Key/Mouse ───┼◀────── │  key/pointer events → wire     │
└──────────────────────────────────┘        └─────────────────────────────────┘
```

- **Bandwidth**: only damaged regions are sent; a region covering >60% of
  the screen is sent as one full frame. A keyframe (full screen) refresh
  every `-refresh` lets any client that dropped frames self-heal. Idle
  desktops use near-zero bandwidth.
- **Clipboard**: each side runs a small selection watcher (its own X
  connection, hidden window). Local copies are pushed to the peer within
  ~0.7 s; remote text takes over the local CLIPBOARD. Loop-safe: content
  that made the round trip is never bounced back. Text only.
- **Slow clients**: each client has a small frame queue; when it overflows
  the server drops frames and flags the client for a keyframe instead of
  ever blocking the capture loop. A 10 s write deadline reaps clients that
  cannot drain at all.
- **Screen resize**: RANDR notifications rebuild the capture buffers and
  announce `ScreenResize` followed by a keyframe; the viewer adapts without
  restarting.
- **Keepalive**: the server pings every 10 s; both sides use 30 s read
  deadlines, so dead peers are reaped instead of hanging.

## Wire protocol (v2)

Plain TCP, big-endian. Frame: `'R' 'M' type flags length:u32 payload`
(max payload 32 MiB). Client speaks first. v2 added the Clipboard
message.

| Type | Message | Direction | Payload |
|---|---|---|---|
| 0x01 | ClientHello | C→S | version u16 |
| 0x02 | ServerHello | S→C | version, width, height, depth, quality, name |
| 0x03 | RectUpdate | S→C | seq, x/y/w/h, codec (1=JPEG 2=WebP), flags (bit0 keyframe), data |
| 0x04 | ScreenResize | S→C | width, height |
| 0x05/0x06 | Ping/Pong | both | nonce u64, ts u64 |
| 0x07 | MouseMove | C→S | x, y (host screen coords) |
| 0x08 | MouseButton | C→S | button 1-7 (4-7 wheel), down |
| 0x09 | Wheel | C→S | dx, dy (reserved) |
| 0x0A | Key | C→S | down, keysym u32 |
| 0x0B | SetQuality | C→S | quality u8 |
| 0x0C | Close | both | code u8, reason |
| 0x0D | Clipboard | both | len u32 + UTF-8 text (≤256 KiB) |

## Troubleshooting

- **"x protocol authentication refused"** — set `XAUTHORITY` (e.g.
  `XAUTHORITY=~/.Xauthority remmote-server -display :0`) or run from a
  shell inside the session.
- **"MIT-SHM missing / SHM attach failed"** — the server automatically
  falls back to banded core `GetImage`; capture still works, just slower
  and more CPU.
- **"DAMAGE missing"** — falls back to periodic full frames every
  `-refresh`.
- **"XTEST extension missing"** — fatal: input injection is the point of
  the tool.
- **Keystrokes missing on the host** — the keysym has no keycode in the
  host's layout (watch `-v` logs); the server never injects `NoSymbol`.
- **Colors wrong on exotic visuals** — only TrueColor 24/32bpp roots with
  32 bits/pixel are supported; anything else fails fast with a clear
  error.
- **Uppercase/symbols type wrong** — the server synthesizes Shift around
  shifted keysyms; if the host layout has no shifted column for that key
  it cannot be typed remotely.

## Development

```sh
make test          # unit tests (no X needed)
make lint          # go vet + gofmt check
make integration   # end-to-end on Xvfb :990 (falls back to :50)
./scripts/integration-window.sh   # window-mode (-window) end-to-end
make vendor        # vendor deps for offline builds
```

Integration tests want `xvfb` (`sudo apt install xvfb`) so they don't
borrow a live desktop. See `scripts/integration.sh`. `internal/testfill`
paints a known color and verifies client snapshots pixel-by-pixel.

### Layout

```
cmd/remmote-server   host main
cmd/remmote-client   viewer main
internal/proto       wire protocol codec (+tests)
internal/xconn       X bootstrap, extension detection, screen facts
internal/capture     SHM + damage + fallback capture (+tests)
internal/encode      JPEG (always) / WebP (build tag `webp`) encoders
internal/input       keymap + XTEST injector (+tests)
internal/server      sessions, broadcast, pacing, stats
internal/viewer      client window, canvas, SHM blitter, input mapping
internal/client      reconnect loop, decode, -once mode
internal/clipboard   bidirectional UTF-8 clipboard sync
internal/testfill    integration-test helper (paint/check/clip-set/clip-watch)
```

### Running as a service

See `scripts/remmote-server.service` for a hardened systemd unit
(`systemctl enable --now remmote-server`). It runs after
`graphical.target` with `DISPLAY`/`XAUTHORITY` set to the session to
share.
