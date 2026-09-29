# Performance checks

Run the microbenchmarks with:

```sh
go test ./internal/encode ./internal/proto ./internal/capture -run '^$' -bench . -benchmem -count 3
```

Measurements on an Intel Core i7-6700 (linux/amd64), comparing the checkout
before and after this optimization pass. Values below are medians of three
runs, with encoder setup amortized across iterations. These are synthetic
codec benchmarks, not measurements of end-to-end frame rate or input latency.

| ZRAW input | Before | After | Allocations before → after |
| --- | ---: | ---: | ---: |
| Desktop-like 1920×1080 | 6.775 ms | 5.492 ms | 13 → 1 |
| Strided 320×200 delta | 86.73 µs | 83.84 µs | 7 → 1 |
| Noisy 1920×1080 | 5.972 ms | 4.136 ms | 12 → 1 |

ZRAW now compresses in one call, reuses scratch buffers, and copies only the
final returned payload. Hybrid encoding discards unsuitable ZRAW candidates
without making that final copy. Queued payloads remain independently owned.
Compressed sizes changed by only 3–4 bytes in these workloads. Reuse retains
scratch capacity for the largest previously encoded region, trading persistent
buffer memory for less allocation and garbage collection.

The client uses a borrowed rect-update view while decoding and compositing a
message. A 1 MiB payload benchmark allocated 48 bytes for this view versus
1,048,628 bytes for the copying decoder. The original copying API remains
available when independent ownership is required.

The viewer now enforces its 60 Hz draw limit, including retries when both SHM
segments are busy, and waits for notifications without an idle polling ticker.
The first update after an idle gap still draws immediately. Sustained updates
may wait up to one frame interval so redraw work can be coalesced.

Pixel-conversion experiments that measured slower were discarded. Regression
coverage also checks nonzero subimage origins, clipped source alignment, and
payload ownership after encoder-buffer reuse.

## Input and visual latency

The next pass addresses input/rendering stalls rather than just codec throughput:

- XTEST, focus, and raise requests remain ordered on the dedicated input X
  connection, without waiting for a synchronous round trip per event. Protocol
  errors are drained asynchronously.
- Viewer completion, expose, and resize notifications no longer wait for the
  drawing mutex. SHM completions identify their segment, including after resize.
- Capture pacing measures from batch start, not the end of encoding. The CLI
  default is now 60 FPS. Unchanged-size keyframes reuse the canvas.
- The frame outbox holds two updates instead of sixteen, and the requested TCP
  send buffer is 256 KiB instead of 4 MiB. This limits stale work under load;
  short network stalls may now trigger recovery keyframes more often. Kernel
  accounting and already transmitted bytes still affect actual buffering.
- Disconnected client writers are canceled and joined before reconnecting, so
  they cannot steal subsequent keyboard/mouse events. Handshake reads preserve
  bytes belonging to the first frame.

Additional medians of three runs on the same machine:

| Workload | Setting | Time |
| --- | --- | ---: |
| Xvfb pointer injection, including final synchronization | round trip per event | 29.78 µs/event |
| Same | ordered asynchronous requests | 4.00 µs/event |
| 1920×1080 → 1536×864 viewer scaling | smooth | 35.24 ms |
| Same | `-fast-scale` | 3.26 ms |
| Desktop resize + hybrid encode, excluding X capture | native | 5.55 ms |
| Same | `-downscale 2` | 1.95 ms |
| Same | `-downscale 4` | 0.46 ms |
| Client `-upscale 2`, full-screen keyframe (960×540 → 1920×1080) | + composite | 7.6 ms |
| Client `-upscale 4`, full-screen keyframe (480×270 → 1920×1080) | + composite | 4.4 ms |
| Client `-upscale 2`, 320×200 delta (→ 640×400) | + composite | 0.74 ms |

Fast scaling uses nearest-neighbor samples, with horizontal sample positions
computed once per region. Tests compare partial redraws against the reference
scaler at multiple sizes. Downscaling uses a fixed source sampling grid so
keyframes and deltas agree even at odd sizes and nonzero origins. The 2×/4×
settings sacrifice text detail and pointer-coordinate precision; they do not
reduce the original X capture region. Quality 1–100 affects JPEG/WebP, not ZRAW.

The client can undo the geometry half of `-downscale` with a matching
`-upscale`, replicating each received pixel back into the block the server
sampled it from. That is nearest-neighbour, which is the exact inverse of
the server's point sampling, so it costs the copy of one destination row
per source row rather than a general resample. The same work through
`x/image/draw`'s nearest-neighbour scaler measured 31 ms on the 2× keyframe
— four times slower for identical output, which a test pins byte for byte.
The scratch buffer is reused across rects, so steady-state magnification
allocates nothing. It restores canvas size, window size and pointer
mapping; it cannot restore the detail the server dropped.

These figures isolate components; they are not measurements of remote
key-to-photon latency or a comparison against X2Go.

```sh
go test ./internal/server ./internal/viewer -run '^$' \
  -bench 'Benchmark(ScaledEncode|ViewerScale)' -benchmem -count 3

go test ./internal/client -run '^$' -bench BenchmarkUpscale -benchmem -count 3

# Use a disposable Xvfb display started with -noreset, never a live desktop:
REMMOTE_TEST_DISPLAY=:994 go test ./internal/input -run TestX11 \
  -bench BenchmarkX11Input -benchmem -count 3

./scripts/integration-delta.sh hybrid 2
./scripts/integration-delta.sh hybrid 2 2   # -downscale 2 + matching -upscale 2
./scripts/integration-delta.sh jpeg 4
```
