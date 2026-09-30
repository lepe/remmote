# Viewer-driven resize (+ own-display docs)

**Goal:** a viewer window resize now resizes the shared surface — the app
window under `-exec`/`-window`, or (behind the new `-resize-desktop` flag)
the host desktop via RANDR — and the README documents running against a
display you start yourself (`:88`).

Decisions (user): no X-server spawning in remmote (document `-display :88`
only); desktop resize is opt-in + best effort with letterbox fallback;
protocol bump v3 → v4; window mode targets exact 1:1.

## Tasks

- [x] 1. proto: `MsgResize 0x0E` + `Resize{Width,Height}` message, `ProtoVersion = 4` → `go test ./internal/proto`
- [x] 2. xwin: `Resize(win, w, h)` — `_NET_MOVERESIZE_WINDOW` (after clearing maximized) with `ConfigureWindow` fallback; message-layout tests → `go test ./internal/xwin`
- [x] 3. xwin: `ResizeScreen(w, h)` — RANDR best effort (clamp to range → fit output modes → `SetScreenSize`, revert crtcs on failure); pure `fitFootprints` unit test → `go test ./internal/xwin`
- [x] 4. capture: `Scene.ResizeTo` (pending target, applied when viewable — mirrors `maybeMaximize`) and `Capturer.ResizeTo` → `go vet ./...`
- [x] 5. server: `-resize-desktop` option, latest-wins `resizeReq` drained on the capture loop, gate + warn-once, `session.reader` case (× downscale, clamped) → `go test ./internal/server`
- [x] 6. client/viewer: `EventListener.Resize` + `Window` forwarding (skip while size == last sent) + debounced sender → `go test ./internal/viewer ./internal/client`
- [x] 7. integration: `scripts/integration-resize.sh` (5 cases: wire resize, real-client resize, both desktop policies, EWMH under openbox) + `make integration` → passes
- [x] 8. README: v4 wire table, `-resize-desktop` row, window/desktop resize sections, headless `:88` section → `make lint`
- [x] 9. Final verification + remove the RANDR scratch probe test → `make test lint integration` all green

## Done when

- [x] `make test lint` clean and `./scripts/integration-resize.sh` passes.
- [x] Viewer resize → app window resize (bare Xvfb path **and** the
  EWMH/openbox path) end to end.
- [x] Desktop resize refused with one clear warning unless `-resize-desktop`.

## Notes (what the probes established)

- **Xvfb screens can never be resized**: grow → `BadValue` (the RANDR
  range caps at the `-screen` size), shrink → `BadMatch` (its single
  mode is fixed). Hence the README note and test case 4, which pins the
  graceful refusal — logged once, screen untouched, stream alive.
- **`_NET_MOVERESIZE_WINDOW` variants, measured on live openbox**: only
  the width/height presence bits (10/11) resize exactly *and* leave the
  window put; every variant carrying x/y re-anchors it by a decoration's
  size per resize, because those coordinates may be read as
  frame-relative. Hence "size only, x/y = 0, source = pager".
- **The canvas solve** (`windowSizeForCanvas`) inverts the 32 px
  `quantizeOutward` grid backwards from the window's origin, so a
  request lands on the canvas within ±16 px (exact for multiples of 32),
  clamped to the screen edge — with an Info log when the clamp bites.
