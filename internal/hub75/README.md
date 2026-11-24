# internal/hub75 — the HUB75 driver

Drives HUB75 RGB LED matrix panels with one scanout engine per chip, selected at
build time by the target's chip tag. Every engine is offloaded — the refresh runs
on dedicated silicon (DMA, EVSYS, PIO, LCD_CAM), never the CPU — so a panel
cannot flicker from GC pauses or application load on any board.

| Engine | Chip (board) | Build | How the refresh is driven |
|---|---|---|---|
| **evsys** | SAMD51 (Matrix Portal M4) | default (no tag) | DMA byte stream; OE on a hardware timer (TCC3), retriggered by the Event System off the DMA — **zero interrupts** |
| **isr** | SAMD51 (Matrix Portal M4) | `-tags=atsamd51_isr` (`make … DRIVER=isr`) | DMA byte stream; a short per-window interrupt "conductor" on TC0/TC1 latches the row and ends the OE pulse |
| **pio** | RP2350 (Interstate 75 W) | by target | PIO state machine clocks a count-encoded word stream; DMA feeds it |
| **lcd** | ESP32-S3 (Matrix Portal S3) | by target | LCD_CAM i8080 bus + GDMA descriptor chain; BCM holds as descriptors over shared buffers |
| host | none of the above | by default | no-op engine: the framebuffer is the whole story, for `go test` and host builds |


## API and configuration

Applications never name an engine. They hold a `*hub75.Device` from `hub75.New`
and draw against it; the build tag picks the engine. `*Device` satisfies
`tinygo.org/x/drivers.Displayer` (compile-time asserted in `hub75.go`), so
`tinyfont` and `tinydraw` work directly, plus `Clear`, `GetPixel`, and
`SetBrightness`.

```go
cfg := hub75.DefaultConfig()          // 64×32, 8-bit, full brightness, double-buffered
panel, err := hub75.New(cfg)          // panel is refreshing once this returns
panel.SetPixel(3, 4, color.RGBA{R: 255, A: 255})
panel.Display()                       // encode the framebuffer and present (tear-free swap)
panel.SetBrightness(128)              // hardware duty-cycle dimming; the image is untouched
```

In this repo the apps build the config from their config file via `cfg.HUB75()`
(see `internal/config`), which starts from `DefaultConfig` and overlays the panel
geometry.

`Config` is deliberately small — it describes the panel and the look, **not**
the silicon. The field list, defaults, and `Validate`'s invariants live on the
type in `hub75.go`.

**Timing is not in `Config`.** Pixel clock and plane on-times are
engine-internal — each engine has a clearly marked `─── TUNING ───` const block
in its file, with each knob's meaning, default, and ceiling documented on the
constant.

## Measurements

From cmd/bench on a real panel (see cmd/bench/main.go for methodology). One
default driver per chip, so the comparison is across chips — plus the SAMD51's
second engine (isr).

cmd/bench pins the canonical `hub75.DefaultConfig` (64×32, 8-bit,
**double-buffered**) so runs are reproducible regardless of the app's config.txt.
All rows below are that config. The +6.0 KB framebuffer (64×32×3 B) is included
in every heap figure.

| Engine | Board | Frame rate† | Colors | Scanout CPU | Draw (produce+present) | Driver RAM (heap + static) | Status |
|---|---|---|---|---|---|---|---|
| **evsys** | matrixportal-m4 (SAMD51) | 192 Hz | 16.7M | **~0%** (noise floor, 0.0–0.1% run-to-run) | 5.2 ms | 38.6 KB + 16.0 KB descriptors | ✅ verified (default) |
| **isr** | matrixportal-m4 (SAMD51) | 71 Hz | 16.7M | **1.7%** | 14.1 ms | 38.7 KB + ~0 | ✅ verified |
| pio | interstate-75w (RP2350) | 188 Hz | 16.7M | **1.1%** | 5.4 ms | 138.4 KB + ~0 | ✅ verified‡ |
| lcd | matrixportal-s3 (ESP32-S3) | 151 Hz | 16.7M | **~0%** | 6.6 ms | 70.4 KB + ~0 | ✅ verified‡ |

† Frame rate is cmd/bench's encode+present rate over 100 back-to-back Displays,
**not** the pure hardware refresh. With the double-buffered `DefaultConfig`, each
`Display` encodes the back buffer and then waits for the tear-free swap at the
next frame boundary, so the figure is bounded by whichever is slower — the
encode, or that engine's refresh-paced swap cadence. The swap-wait surfaces isr's
~71 Hz BCM refresh; evsys free-runs fast enough to stay near its encode rate.

‡ pio and lcd run on different, faster chips (RP2350 dual ~150 MHz, ESP32-S3 dual
~240 MHz) than the SAMD51 (120 MHz), so absolute numbers aren't a clean
cross-chip comparison — the scanout-CPU column and the architecture story are.
All four engines pass the `cmd/stress` worst-case sweep and run the full clock
(incl. WiFi) on hardware.

Reading the rows:

- **Scanout CPU is the split that matters.** evsys and lcd are zero-ISR —
  hardware retriggers or clocks the bus, so their scanout cost sits at the noise
  floor. isr re-arms via a per-*window* interrupt (~1.7%, rising with refresh);
  pio re-arms once per *frame*, so its cost scales with hardware refresh —
  1.1% at holdDutyRatio 1's ~750 Hz.
- **The frame-rate column tracks each engine's swap cadence**, not raw encode:
  isr lands on its ~71 Hz BCM refresh; evsys free-runs near its encode rate.
- **M4 heap is ~equal across its two engines** (~38.6 KB double-buffered; they
  share the byte data plane). evsys adds a 16.0 KB static descriptor pool on top;
  isr's static is ~0. Static pools never appear in a heap figure — see
  "Accounting gaps" below.
- **pio and lcd carry heavier heaps** (138.4 KB / 70.4 KB): pio double-buffers
  full 32-bit word streams; lcd materializes 16-bit shift words plus the GDMA
  hold descriptors and the `[]seg` plan behind them. lcd is 1 unit/pixel natively
  (LCD_CAM emits the shift clock itself), so the S3 never pays the SAMD51's
  2-units-per-pixel CLK trick.
- **produce+present spans 5.2–14.1 ms** under double-buffering. evsys (5.2 ms)
  and pio (5.4 ms) are encode-bound — their hardware refresh outruns the encode.
  lcd (6.6 ms) and especially isr (14.1 ms) are higher because their `Display`
  blocks on the tear-free swap — isr's 71 Hz refresh adds ~9 ms of wait. That
  swap-wait is the cost of tear-free, not a regression.
- **pio's rate and IRQ cost track `holdDutyRatio`.** At ratio 9 the same board
  benched 150 Hz / 0.2%: longer holds mean slower refresh, fewer re-arm IRQs,
  and a swap-bound Display. The knob moves this whole row, not just brightness.

Forward work is the S3's PSRAM-backed standalone mode
([internal/psram/README.md](../psram/README.md)).

## The SAMD51 engines (Matrix Portal M4)

The SAMD51 is the one chip in this package with two interchangeable engines.
Both stream the pixel bytes to PORTB by DMA and share the pin map and DMAC
descriptor layout, stated once in `hub75_samd51.go`. evsys is the default because
it costs no CPU and reaches the highest refresh; isr is the simpler DMA model
(one channel, no event plumbing).

```sh
make clock                       # evsys (M4 default)
make clock DRIVER=isr            # isr   (=> -tags=atsamd51_isr, M4 only)
```

### How evsys works (the default)

A self-looping DMA chain (channel 0) streams the byte data + latch/address beats
forever — OE is **not** in that stream. OE is a timer waveform output on
TCC3/WO[0] (PB12). At each (row, plane) window the latch descriptor emits a
peripheral **block event**; the Event System routes it to TCC3's RETRIGGER input,
so OE pulses low for a compare-defined on-time and back high, in hardware, with
no interrupt. The per-plane on-time (the BCM weighting) is streamed into
`TCC3.CCBUF0` by a **second DMA channel** (channel 1), one WORD beat per window,
off a sibling EVSYS channel sharing the same block event.

```
 DMA Ch0 ── byte data + latch beats, self-looping ── emits a BLOCK event per window
    │                                                          │
    ├── EVSYS ch0 (ASYNC) ───────────────▶ TCC3 RETRIGGER  (WO[0] drives OE on PB12)
    └── EVSYS ch1 (RESYNCHRONIZED + GCLK) ▶ DMAC Ch1 trigger ▶ TCC3.CCBUF0  (next on-time)
```

Claimed hardware: DMA channels 0 and 1, TCC3, two EVSYS channels, and a GCLK on
the CC channel. Only one device per program.

The per-plane CC schedule is periodic (period = planes), which is what lets a
single self-looping descriptor on channel 1 carry it — 4 descriptors per window
on channel 0 instead of 5, saving 4 KB of static descriptor pool.

#### Bring-up facts worth keeping

These were each non-obvious and cost a debugging session; they are *why the code
looks the way it does*:

- **`CHEVCTRL` is enable-protected.** A channel's event control (`EVIE`/`EVOE`)
  must be armed **before** the channel is enabled.
- **The CC channel's EVSYS user is `EVSYS_ID_USER_DMAC_CH_1 = 6`, not 1.** The
  SAMD51 USER table is `PORT_EV_0..3 = 1..4`, *then* `DMAC_CH_0..7 = 5..12`. A
  wrong id lets the event reach the EVSYS channel (EVD sets) but never the DMAC —
  the panel goes uniformly dim.
- **Two paths, deliberately different.** The OE link (ch0) is **ASYNC** — a TCC
  retrigger samples the pulse directly, no channel clock needed. The CC link
  (ch1) is **RESYNCHRONIZED** with a channel GCLK and rising edge select, because
  a *DMAC trigger* user samples the event on a clock. ASYNC drives the TCC fine
  but never triggers the DMAC.
- **OE polarity:** `INVEN0` is set so OE idles high (blanked) and goes low for
  the lit on-time after each latch.
- **One-window CCBUF lag → rotate by one.** `CCBUF0` is buffered: a value written
  at event *N* loads into `CC0` at event *N+1*. So the CC schedule is rotated by
  one (`ccSched[i] = oeOnTicks((i+1) % planes)`) and the first two windows are
  seeded directly in `configureOETimer` (`CC0` = plane 0, `CCBUF0` = plane 1).
  Get this wrong and every plane's brightness shifts by one bit-weight (wrong
  gamma + bright-plane ghosting). The rotation and those seeds are one mechanism;
  change one and the other moves with it.

Reference: SAM D5x/E5x Family Data Sheet (DS60001507), Ch. 31 Event System — the
Event Generators / Event Users tables (sub-table numbers drift between revisions,
so anchor on the chapter).

### How isr works (the alternative)

A frame is pre-rendered into a "bitstream": for each (row, bit-plane) one byte
per clock edge plus a trailing idle byte, formatted exactly as PORTB's low byte
expects (six RGB bits, the clock bit, and ADDR_A, which shares the byte). The
idle byte returns CLK low after the last pixel, so the panel always sees the
latch with the clock parked low — the tolerant state when the 5 V rail sags.

TC0 paces DMA channel 0, which streams the bitstream to the port with no CPU
involvement. TC1's overflow interrupt (`windowTick`, the "conductor") runs once
per bit-plane window: it latches the freshly shifted row, sets the address lines,
programs the next window length (BCM: plane *p* displays for `baseHoldTicks<<p`),
and re-arms the DMA. TC1's second compare (CC1) ends the OE on-time early for
global brightness, preserving BCM ratios exactly.

On-times are **decoupled from display windows**: each window is floored at the
row shift time (a row must finish shifting under the previous plane's display),
and planes whose on-time falls below that floor are simply blanked for the
remainder — OE ends early via the compare, so BCM ratios stay exact. That is what
makes 6–8 bit-planes practical at full refresh; the trade is some duty-cycle
slack at the dim end.

Claimed hardware: DMA channel 0, TC0, TC1. Only one device per program.

### Tuning

Every knob lives in its engine's `─── TUNING ───` const block — evsys's
window/on-time pair, isr's shift period and hold, pio's `beatDiv` and
`holdDutyRatio`, lcd's `pixClkDiv` and `holdDutyRatio` (that last one carries a
full duty/refresh/RAM guide). The constants' doc comments are the authority on
defaults and ceilings. The cross-cutting advice: if a panel shows smearing or
stale rows (often on sagging USB power), slow the shift or lower brightness —
margin, not code, is usually what's missing.

## Hardware notes

- **Power.** The panel needs its own 5 V supply for full brightness. On USB power
  it runs dimmer and the sagging rail narrows the panel logic's timing margins;
  if artifacts appear, raise the shift period / hold constants or cap
  `SetBrightness`.
- **Latch sequence.** CLK is parked low through the latch, the address is set
  before LAT, and the LAT pulse is widened. The dominant reason is fast
  controller GPIO outpacing the panel's latch/decoder chips at nominal voltage
  (per Protomatter's ~8 µs address settle and hzeller's `--led-slowdown-gpio`);
  supply sag only aggravates it. We pad *less* than those references, so the
  sequence is lean, not over-hardened.
- **Geometry.** Only 64×32 panels have been verified on hardware. 64-tall
  (ADDR_E) and chained configurations follow the same logic and pass validation
  but are untested.

## Testing

Everything above the register backends — gamma, the framebuffer,
geometry/Validate — is pure computation. Off-target builds select the no-op host
engine (`hub75_host.go`), so the package and its consumers compile and run under
plain `go test` with the framebuffer as the whole story. The register programming
itself requires hardware to verify; that rests on the apps and `cmd/stress`'s
worst-case patterns.

```sh
go test ./internal/hub75/                          # host: pure policy
tinygo build -target=matrixportal-m4 ./cmd/clock   # compile-check an engine
make bench BOARD=…                                 # measure a row for the table above
```

## Files

| File | Role |
|---|---|
| `hub75.go` | Shared floor + public surface: `Device`, `New`, the Displayer contract, `Config`/`Validate`/`DefaultConfig`, gamma + brightness helpers. Pure policy, always compiles. |
| `hub75_samd51.go` | Shared SAMD51 substrate (`atsamd51`): pin map, DMAC descriptor type + BTCTRL bits, `configurePins`, bounded `waitSync`. |
| `hub75_samd51_evsys.go` | evsys engine (`atsamd51 && !atsamd51_isr`): DMA chain + OE timer + two EVSYS links + the CC-streamer channel. |
| `hub75_samd51_isr.go` | isr engine (`atsamd51 && atsamd51_isr`): DMA stream + TC0/TC1 + the `windowTick` conductor. |
| `hub75_rp2350.go` | pio engine (`rp2350`): PIO count-loop program + DMA word stream. |
| `hub75_esp32s3.go` | lcd engine (`esp32s3`): LCD_CAM i8080 + GDMA descriptor holds. |
| `hub75_host.go` | No-hardware engine for host build/test (none of the chip tags). |

## Optimization backlog

The reasoning is kept terse here so it isn't re-derived; each item names its
engine and its measured or computed size.

**Already pulled** (kept because they explain the current shape):

- *evsys: CC schedule off the chain* (−4 KB static). The per-plane OE on-time
  streams off a second DMA channel instead of a fifth per-window descriptor —
  pool 20 → 16 KB.
- *isr: encode gamma hoist* (−56% produce+present, zero RAM). Read each pixel
  once, fan the gamma-corrected values across planes; evsys encodes this way too.
- *lcd: descriptor-plan right-sizing* (−5,760 B heap at 128×64×4, measured).
  `maxPlanLen`'s `+1` partition slack is tight (verified exhaustively over
  geometry × depth × ratio × brightness); the old outer `2×` was pure slack.
  `ramBytes` now also counts the `[]seg` plan and hold headers it had omitted
  (it understated by ~20%, more at high `holdDutyRatio`).

**Available:**

- *evsys: descPool right-sizing* (up to −12 KB static). The pool is sized for
  max rows × max planes × double-buffer at once; real use is
  `scanRows*planes*4`, so single-buffered depth-4 configs leave 12 KB of bss
  idle. Heap-allocating at start (as lcd does) sizes it to the config.
- *Small static wins* (~64 B — listed because the stated rationales are wrong,
  not for the bytes): `geoLit`/`geoWindow` fit `[8]uint16` (both uses already
  cast to uint16); evsys's `windowWord[r]` duplicates `latchWord[r][1]`.
- *Non-blocking Display* (isr, evsys; up to ~2× app throughput when app compute
  < frame period). Request the swap and return instead of spinning to the
  boundary; needs a back-buffer guard so the app can't draw into a
  not-yet-adopted buffer.

**Blocked / unverified:**

- *Hardware-generated CLK (M4)* — dead lever. Halving the 2-units-per-pixel
  stream needs a timer waveform on CLK, and CLK is hard-wired to PB06, which has
  no TC/TCC output on the SAMD51. A board constraint, not an effort question.
  (The S3 already gets this free: LCD_CAM emits the clock.)
- *pio: CLK off the data word* (−128 KB at 128×64×8) — unverified. Side-setting
  CLK (as Pimoroni's own driver does) would drop `segWords` from `2*width+4` to
  `width+4` — the difference between depth 8 + DoubleBuffer being `ErrTooBig` at
  128×64 and fitting. Gate: whether the 3-instruction count-loop can express
  both shift and hold beats without toggling CLK during a hold; needs the
  program written and flashed.

**Accounting gaps** (defects found by auditing cost comments against code;
method: a comment asserting a cost is a claim — check it against the code, then
a measurement):

- *Neither SAMD51 engine implements `ErrTooBig`* — width is unbounded, so a wide
  chain OOM-panics inside `make` instead of returning a clean error. Fix is a
  `ramBytes`-style guard per engine.
- *isr's conductor runs per window, not per row* (~12 entries/row/frame at
  depth 8), and its CPU *rises* as ColorDepth falls (windows are floored at the
  shift time). Magnitude unmeasured — settle with a DWT timestamp (`cycles()` is
  already in the file).
- *lcd's `e.plan` is append-grown* — ~6 KB of power-of-two slack at the bench
  config, and **not recoverable**: preallocating `maxPlanLen` is larger than the
  rounding at some geometries. Harmless (GDMA reads the pre-sized `descChain`,
  never the plan); revisit only if an app drives `SetBrightness` on a tight S3
  config.
- *Static vs heap needs both tools*: evsys's 16 KB pool is bss (`make sizes`);
  lcd's equivalent is heap (`cmd/bench`). Neither alone tells an engine's true
  footprint.

**The holdDutyRatio asymmetry** (do not port conclusions between engines): pio
count-encodes the dwell into a word that already exists — RAM-invariant at any
ratio. lcd spends it on objects (descriptors + plan) — linear in the ratio
(~87 KB at ratio 1 vs ~206 KB at ratio 9, 128×64×6). Same knob, same prose,
opposite cost model.

**Untapped silicon**, most useful first: a whole free core on both the RP2350
and the S3 (the refresh needs no CPU — room for encode, fetch, or the standalone
clock's GTFS/TLS work); the S3's hardware crypto (AES/SHA/RSA), relevant the day
the standalone clock runs TLS on-device; PIO1/PIO2 untouched on the RP2350
(second chain, or WiFi on its own block) and HSTX as a faster shift engine (8
lanes — data+clock only, addressing stays PIO). The SAMD51 is thin by design:
single-core, and evsys already sits at the materialized end.

## Non-goals

- **Merging isr + evsys behind an OE-mode flag.** They share the byte data plane
  and differ only in where OE timing lives; folding them would thread that one
  real difference through conditionals — burying exactly what the two-engine
  split exists to keep clear.
- **Single-bitstream tear-free.** A contradiction on these engines: the second
  encoded stream *is* the tear-free mechanism (the DMA scans the wire stream,
  not raw RGB, so `pix` can't stand in). Re-encoding the live stream in place
  either races the scan (tears) or halts the refresh (flicker). Single-buffer
  (tears, less RAM) already ships as `DoubleBuffer: false`; there is no third
  option.
- **Parallel chains / non-standard scan types.** The single-port-group atomic
  write and standard 1/(Height÷2) indoor scan are assumed throughout; parallel
  output is a Pi/FPGA-class feature.
