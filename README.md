# remmote

Pure-Go (CGo-free by default) Linux X11 screen sharing with **full remote
control** between two machines. One binary captures and controls; the other
views and drives.

- **`remmote-server`** — runs on the **host** (the machine to view/control).
  Tracks screen changes with **XDamage**, grabs pixels through **MIT-SHM**
  (with an automatic core-protocol fallback), encodes only the changed
  regions with a **hybrid codec** — raw pixels through zstd (*ZRAW*) when
  they compress ≥4:1 (typical desktop: ~8 ms and ~40 KB per 1080p frame,
  vs JPEG's ~59 ms and ~574 KB), JPEG otherwise (optionally lossy
  **WebP**) — and injects keyboard and mouse events via **XTEST** —
  including VNC-style synthetic-Shift choreography for capital letters
  and symbols.
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
make build          # → bin/remmote-server, bin/remmote-client (pure Go)
```

### Codecs

| `-codec` | Encoder | Wire byte | Needs |
|---|---|---|---|
| `hybrid` (default) | ZRAW when the rect compresses ≥4:1, else JPEG | 3 / 1 | — |
| `zraw` | zstd on raw pixels (lossless, always ZRAW) | 3 | — |
| `jpeg` | stdlib JPEG | 1 | — |
| `webp` | libwebp, ~25–35% smaller than JPEG | 2 | `-tags webp` build |

ZRAW is [klauspost/compress](https://github.com/klauspost/compress) zstd
at `SpeedFastest` over the raw frame bytes — pure Go, no build tag, and
because desktop content (flat colors, text) is highly redundant it lands
one to two orders of magnitude below JPEG in both time and bytes. The
hybrid gate (≥4:1) bounds bandwidth to 25% of raw and falls back to JPEG
for content zstd handles poorly (photos, gradients, noise). The picker
runs per rect, so a mixed screen always gets the cheap option; hybrid
never appears on the wire.

The client decodes every codec regardless of how it was built. A server
built without the WebP tag refuses `-codec webp` at startup — never
mid-stream.

### Optional: lossy WebP codec

WebP encodes ~25–35% smaller than JPEG at similar quality. The encoder is
libwebp (C), so it is kept out of the default build behind a build tag:

```sh
sudo apt install libwebp-dev     # Ubuntu/Debian
make build-webp                  # → same binaries + WebP, run server with -codec webp
```

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

`-maximize` puts the main window onto the whole host screen before sharing
it, so the canvas fills the screen instead of the window's natural size.
The request goes through the window manager (EWMH) where one runs and is a
plain resize otherwise, and it is applied once, when the window becomes
viewable — so it also works for an application whose window appears late,
or a single-instance one that hands off to an already-running process.
Dialogs keep their natural size.

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
| `-fps` | `60` | max frames/s (also the damage merge window) |
| `-codec` | `hybrid` | `hybrid`, `zraw`, `jpeg` or `webp` (webp needs a `-tags webp` build) |
| `-downscale` | `1` | divide stream width/height by 2 or 4 to reduce encoded pixels 4× or 16×; mouse coordinates are mapped back automatically |
| `-quality` | `75` | encoder quality 1-100 (clients may override live; JPEG/WebP only) |
| `-refresh` | `2s` | periodic full-frame keyframe interval (skipped while nothing changes) |
| `-dump-frame` | — | capture one frame to a PNG and exit (diagnostics) |
| `-test-inject` | — | move the pointer and type `a`, then exit (diagnostics) |
| `-exec` | — | run this command and share only its windows (e.g. `-exec xcalc`) |
| `-window` | — | share this existing window id (hex) and windows it spawns |
| `-maximize` | off | with `-exec`/`-window`: maximize the shared window onto the host screen once it appears |
| `-no-clipboard` | off | disable clipboard synchronization |
| `-v` / `-log-json` | off | debug level / JSON logs |

### remmote-client flags

| Flag | Default | Meaning |
|---|---|---|
| `-display` | `$DISPLAY` | X display for the viewer window |
| `-server` | (required) | `host:port` |
| `-quality` | `0` | JPEG/WebP quality 1-100; 0 = keep default. ZRAW remains lossless |
| `-fast-scale` | off | use nearest-neighbor viewer scaling for lower CPU use, with rougher edges |
| `-upscale` | `1` | magnify the stream by 1, 2 or 4 back to host resolution; match the server's `-downscale` so the canvas, window and pointer mapping use host coordinates |
| `-once` | off | exit after the first keyframe (no window; CI mode) |
| `-snapshot` | — | with `-once`: write the first full frame as a PNG |
| `-snapshot-after` | — | with `-snapshot`: run the live session for D, write the composited canvas (keyframe + deltas), exit (CI) |
| `-no-clipboard` | off | disable clipboard synchronization |
| `-v` / `-log-json` | off | as above |


### Responsiveness controls

For a LAN, start with hybrid compression and a 60 FPS cap:

```sh
./bin/remmote-server -codec hybrid -fps 60 -downscale 2
./bin/remmote-client -server HOST:7677 -fast-scale
```

`-downscale 2` sends half the width and height (one-quarter of the pixels).
`-downscale 4` sends one-sixteenth as many pixels but loses much more detail.
This reduces encoding, decoding, and bandwidth work; capture still reads the
original region. Keep `-downscale 1` for sharp text. Settings work with both
whole-desktop and application sharing, including pointer coordinate mapping.

### Restoring full resolution on the viewer

A downscaled stream leaves the viewer canvas, window and pointer mapping in
stream coordinates. Pass the matching `-upscale` to bring them back to host
resolution:

```sh
./bin/remmote-server -codec hybrid -fps 60 -downscale 2
./bin/remmote-client -server HOST:7677 -upscale 2
```

Each received pixel is replicated into a 2×2 block — the exact inverse of the
server's point sampling, so every pixel sits where the host drew it, and the
window opens at the host's size instead of half of it. Pointer coordinates are
divided on the way out and multiplied back by the server, so the click still
lands on the same host pixel as without `-upscale`. It costs client-side CPU
and memory (a full-resolution canvas) and recovers no detail the server threw
away; it fixes geometry, not sharpness. The two flags are independent — an
unmatched pair still connects, but the canvas and the pointer mapping will
disagree.

If bandwidth is the bottleneck, try:

```sh
./bin/remmote-server -codec jpeg -quality 40 -fps 30 -downscale 2
./bin/remmote-client -server HOST:7677 -quality 40 -fast-scale
```

Lower JPEG quality reduces bytes, but JPEG can use more CPU than hybrid/ZRAW
for desktop content. Quality does not affect lossless ZRAW frames. Client
quality requests affect the shared encoder for all connected viewers. Raising
FPS reduces capture waiting but increases CPU/network demand; lowering FPS
reduces that demand at the cost of slower visual feedback. A longer server
`-refresh` interval (for example `10s`) also reduces periodic full-screen work.
See [PERFORMANCE.md](PERFORMANCE.md) for measured tradeoffs and benchmark commands.


## How it works

```
┌────────────── host ──────────────┐        ┌──────────── viewer ─────────────┐
│ XDamage ──▶ dirty-rect merge ──▶ │        │  TCP ◀─ RectUpdate (ZRAW/JPEG) │
│ MIT-SHM GetImage (ZPixmap 32bpp) │  TCP   │  decode → canvas composite     │
│ BGRA→RGBA → encode → broadcast ──┼──────▶ │  scale-to-fit → SHM PutImage   │
│ XTEST FakeInput ◀── Key/Mouse ───┼◀────── │  key/pointer events → wire     │
└──────────────────────────────────┘        └─────────────────────────────────┘
```

- **Bandwidth**: only damaged regions are sent; a region covering >60% of
  the screen is sent as one full frame. A keyframe (full screen) refresh
  every `-refresh` lets any client that dropped frames self-heal — and is
  skipped while nothing changed, so an idle desktop costs near-zero
  bandwidth yet wakes instantly on the first change instead of waiting
  for the next tick.
- **Codec**: each rect is packed independently — ZRAW (lossless zstd,
  ~8 ms / ~40 KB per 1080p desktop frame) when it compresses ≥4:1, else
  JPEG (~59 ms / ~574 KB) for photographic content. Mixed screens always
  get the cheap option per rect.
- **Latency**: the capture loop sleeps only the time left in the frame
  budget (no fixed post-encode sleep), and the viewer repaints only the
  dirty sub-rect — converted in place straight into the SHM segment at
  1:1, so a typical update redraws in microseconds instead of pushing a
  full-window scale + PutImage every frame.
- **Input latency**: input is a separate path from video and never
  queues behind it. Pointer motion and discrete events (buttons, wheel,
  keys) travel in two lanes on both sides: motion is a one-slot
  latest-wins mailbox, discrete events are queued and never dropped or
  merged. A click therefore reaches XTEST after at most one position
  update, no matter how fast the mouse is moving — the case that
  otherwise made a click wait for the whole backlog of stale positions.
  Injection uses its **own X connection** and sends ordered XTEST requests
  without a synchronous round trip for every event. Viewer completion/expose
  notifications do not wait for rendering locks. The server's stats line
  reports queue-to-X-submission wait as `in_wait_maxms` (not end-to-end visual
  latency); the client logs a warning if
  input waits over 250 ms before reaching the wire.
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

## Wire protocol (v3)

Plain TCP, big-endian. Frame: `'R' 'M' type flags length:u32 payload`
(max payload 32 MiB). Client speaks first. v2 added the Clipboard
message; v3 added the ZRAW codec. The server refuses a mismatched
`ClientHello.version`, so a v2 client now gets an explicit refusal
instead of connecting and then dropping every rect.

| Type | Message | Direction | Payload |
|---|---|---|---|
| 0x01 | ClientHello | C→S | version u16 |
| 0x02 | ServerHello | S→C | version, width, height, depth, quality, name |
| 0x03 | RectUpdate | S→C | seq, x/y/w/h, codec (1=JPEG 2=WebP 3=ZRAW), flags (bit0 keyframe), data |
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
make integration   # keyframe (hybrid + jpeg) and live-delta pipelines on Xvfb
./scripts/integration-window.sh   # window-mode (-window) end-to-end
./scripts/integration-delta.sh    # live delta updates, position-exact (also in make integration)
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
internal/encode      JPEG / ZRAW (zstd) / hybrid / WebP (tag webp) encoders
internal/input       keymap + XTEST injector (+tests)
internal/server      sessions, broadcast, pacing, stats
internal/viewer      client window, canvas, dirty-region SHM blitter, input mapping
internal/client      reconnect loop, decode, -upscale, -once / -snapshot-after CI modes
internal/clipboard   bidirectional UTF-8 clipboard sync
internal/testfill    integration-test helper (paint/check/clip-set/clip-watch)
```

### Running as a service

See `scripts/remmote-server.service` for a hardened systemd unit
(`systemctl enable --now remmote-server`). It runs after
`graphical.target` with `DISPLAY`/`XAUTHORITY` set to the session to
share.

### Stopping

Press Ctrl+C in the terminal running either binary to shut it down cleanly.
The client cancels connection/reconnect waits and closes its viewer and
clipboard watcher. The server stops accepting connections, joins capture and
input workers, releases held remote keys/buttons, frees shared memory, and
terminates applications started with `-exec`. Applications shared with `-window`
are not terminated. SIGTERM follows the same cleanup path. A second signal
uses normal OS termination if cleanup is stuck.

Run `make integration-shutdown` to check real SIGINT handling with disposable
Xvfb displays, including stalled peers and application startup.
