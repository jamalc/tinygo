# internal/psram — ESP32-S3 PSRAM: what it is and what it unlocks

The Adafruit Matrix Portal S3 carries **2 MB of quad PSRAM** that TinyGo
leaves completely dark — its linker script maps only the 512 KB of
internal SRAM, and the ROM-bootloader replacement never runs the SPIRAM
init. This package is the from-scratch bring-up that lights it up, and
this doc records — more importantly than the driver — **what having 2 MB
actually buys**, so a later session can pick the right thing to build on
it.

## Status: built and hardware-verified

The package does the work ESP-IDF's 2nd-stage bootloader normally would;
the API surface (`Enable`, `Region`, `Allocator`, `Writeback`/
`Invalidate`) and the register-level mechanism are documented in the
package's godoc (`psram_esp32s3.go`, `alloc.go`) — the code is the
authority there. Verified on hardware (`cmd/psramprobe`): detect, a
round-trip across the full 2 MB, and a 64 KB allocator buffer round-trip
all pass, and it re-detects cleanly across reboots.

Two bring-up lessons worth keeping: the PSRAM clock (`SPI0.SRAM_CLK`) is
a **separate divider from flash** — unset, every read comes back
all-`0xFF`; and the bring-up stays in **single-line SPI, not QPI**,
because entering QPI strands the chip (QPI survives a CPU reset; only a
power cycle clears it). QPI is a deferred speed pass (see below).

## The fit principle

PSRAM is **not** GC heap — the Go runtime never sees it, so the
collector can't trace it. A good consumer is therefore:

> **large + flat/arena'd (no live Go pointers) + not DMA/per-pixel-clock
> hot + tolerant of ~10 MB/s.**

This is why the model is a separate region + manual allocator
(Arduino `ps_malloc`-style), **not** CircuitPython's transparent
"everything in PSRAM" heap — MicroPython's GC heap is pluggable and
PSRAM-first; TinyGo's is pinned to the linker SRAM region. So consumers
must be flat value buffers, not pointer-bearing Go structures (maps,
slices-of-strings, etc.).

Note the device is **not SRAM-pressured** today (it uses ~50 KB of
512 KB). PSRAM's value is **not relief** — it's holding datasets and
assets that don't fit 416 KB *at all*, which is what turns the S3 from a
"server-fed display" into a **standalone device**.

Nothing consumes the region yet — the driver is ready and waiting for the
consumer below.

## Headline consumer: cut the server, go standalone

The clock depends on `cmd/serve` because the device can't hold or parse
the GTFS static data. With PSRAM it can: hold GTFS static in PSRAM, fetch
the GTFS-realtime feed straight from the MTA, and compute arrivals
on-device. **No server.** (S3-only — the M4 has no PSRAM — so this gives
the S3 board a reason to exist.)

**Sizing is proven.** Measuring the real feeds (flat layout: string
arena + interned headsigns + a per-trip index):

| | |
|---|---|
| normalized trips | 23,450 (`normalizeTripID` collapses schedule prefixes) |
| unique headsigns | 78 → interns to ~1.6 KB |
| trip→headsign (arena 330 KB + 8 B/trip index 183 KB) | 515 KB |
| stops (1,488; 379 unique names) | 24 KB |
| routes | 12 KB |
| **flat GTFS total** | **0.54 MB — 1.46 MB headroom under 2 MB** |

The two giants — `stop_times.txt` (36 MB) and `shapes.txt` (5 MB) —
**aren't needed**; the realtime feed carries the actual times. The work
is a flat, pointer-free representation (string arena + interning +
sorted/hash index) plus the on-device realtime path — essentially porting
`cmd/serve`'s static + realtime + arrivals ([internal/gtfs](../gtfs),
`internal/realtime`, `internal/arrivals`) to TinyGo with a columnar
layout. Capacity is not the constraint; the data-structure design is.

## Other viable consumers (large + flat + cold)

- **Offline schedule fallback** — with 1.46 MB to spare, cache a slice of
  static schedule so the clock still shows times when the realtime feed
  or WiFi drops.
- **Multi-feed / multi-city** — hold several agencies' static data at
  once; switch lines without a re-fetch.
- **Arrival/delay history** — a ring buffer of observations for trends or
  sparklines ("running ~4 min late"). Flat records, written occasionally.
- **Richer rendering assets** — a glyph atlas (bigger fonts, full
  Unicode, multiple typefaces), sprites/icons, or a small subway-map
  image. Loaded once, read by CPU.
- **Big offscreen canvas** — much larger than the panel, CPU-rendered,
  blit a window to the grid. Enables smooth scrolling of long arrival
  lists, marquees, and screen transitions the apps render directly today.
- **On-device trip planning** — a routing graph (stations + connections)
  that won't fit 416 KB.

## Poor fits — keep these in SRAM

The DMA-hot side of the display is a **bad** candidate: the hub75 shift
buffers and GDMA descriptors are read by the DMA engine *every pixel
clock* (~10 MHz), and PSRAM latency would cause FIFO underruns and
visible corruption. (The CPU-only pixel grid would tolerate PSRAM —
GDMA never reads it — but at 6–24 KB it isn't worth the coupling, and it
stays on the GC heap.) This split is confirmed by Adafruit's own
Protomatter, whose ESP32-S3 arch header allocates its DMA buffers with
`heap_caps_malloc(x, MALLOC_CAP_DMA | MALLOC_CAP_8BIT)` and a literal
`// (not PSRAM)` comment. Also poor: tiny hot state, and any
pointer-graph Go data.

(The DMA-buffers-in-SRAM choice and the manual `Writeback`/`Invalidate`
both match Adafruit/Espressif exactly — the latter is `esp_cache_msync`
C2M/M2C, which is *required* on the S3's writeback cache and a no-op on
the classic ESP32.)

## QPI: a transparent speed pass, not a prerequisite

A later QPI pass (4-line `0xEB`/`0x38`) would lift bandwidth ~10 → 40
MB/s with an **identical API** — it unlocks no new capability and changes
no consumer code. So don't gate any consumer on it: build single-line,
optimize later. It still owes a clean fix for the strand-on-reboot
problem before it's worth doing.
