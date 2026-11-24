# tinygo

A personal lab for [TinyGo](https://tinygo.org/) hardware projects.

This began as a NYC subway clock and grew: HUB75 scanout engines for three boards
(Adafruit Matrix Portal M4, Matrix Portal S3, Pimoroni Interstate 75 W), a set
of apps for the panel, and from-scratch drivers for the hardware around it —
ESP32-S3 PSRAM bring-up, per-board WiFi transports, accelerometer input. See
[Status & roadmap](#status--roadmap) and [History](#history) below; the deep
dives live with their packages ([internal/hub75](internal/hub75/README.md),
[internal/psram](internal/psram/README.md)).

## Subway clock

Real-time NYC subway arrivals on a 64×32 HUB75 LED matrix. A Go backend merges
the MTA's static GTFS data with its GTFS-realtime feeds and serves per-stop
arrivals over a small HTTP API. The clock polls that API and animates the
arrivals on the panel — as TinyGo firmware on any supported board, or in a host
simulator rendered with [Ebiten](https://ebitengine.org/).

```
 device / simulator  ──HTTP──▶  cmd/serve  ──HTTPS──▶  MTA GTFS feeds
```

## Components

| Path             | What it is                                                        | Runs on        |
|------------------|-------------------------------------------------------------------|----------------|
| `cmd/serve`      | Arrivals HTTP API (`/arrivals`, `/bikes`, `/stops`, `/stops/search`, `/health`) | native (server) |
| `cmd/fetch`      | Downloads the MTA static GTFS zip files                           | native         |
| `cmd/simulate`   | Host simulator (Ebiten); runs any of the display apps             | native         |
| `cmd/clock`      | The subway clock, as device firmware                              | TinyGo (device)|
| `cmd/docks`      | Open Citi Bike docks at nearby stations, as device firmware        | TinyGo (device)|
| `cmd/conway`     | Conway's Game of Life on the panel, as device firmware            | TinyGo (device)|
| `cmd/bloom`      | Fireworks on the panel, as device firmware                        | TinyGo (device)|
| `cmd/stress`     | Worst-case patterns to verify HUB75 data timing                   | TinyGo (device)|
| `cmd/bench`      | Measures the HUB75 engines on a real panel (feeds [internal/hub75/README.md](internal/hub75/README.md)) | TinyGo (device)|
| `cmd/marble`, `cmd/breakout` | The tilt games, as device firmware                    | TinyGo (device)|
| `cmd/acceldiag`  | Prints the accelerometer's axes, to set a board's tilt orientation | TinyGo (device)|
| `cmd/psramprobe` | ESP32-S3 PSRAM bring-up probe                                     | TinyGo (device)|

The portable display logic (`internal/clock`, `internal/docks`, `internal/conway`, `internal/stress`,
`internal/bloom`, `internal/marble`, `internal/breakout`) is shared by
the device and the simulator through a small `Display` interface that both `hub75.Device`
(the TinyGo HUB75 driver) and `simulator.Display` satisfy.

Apps that take user input do so through `internal/input`, which has no dependencies and
offers two ports, because a lean is not a button press:

- `Source` is **discrete** — a control fired. Used to step pages (clock, stress).
- `TiltSource` is **analog** — how far the board is leaning, as a vector. Used as gravity
  by the tilt maze (marble), where quantizing to four directions would throw away the
  very thing the game is built on, and (X axis only) to drive the breakout paddle.

One adapter can serve both: the simulator keyboard reports arrow *presses* as `Source`
and arrow keys *held* as a lean, and on device `internal/accel` reports the accelerometer's
lean as `TiltSource` while `internal/buttons` maps the board's UP/DOWN buttons. Adapters
missing a peripheral report nothing at all rather than failing, so every app builds and runs
on every board.

## Citi Bike docks

Where to bring a bike back: the open docks right now at a few nearby Citi Bike
stations, one block per station. The server polls Citi Bike's public GBFS feed
(no key) and serves just the stations asked for at `/bikes?station=…`, since
the whole feed is a megabyte. The device lists them by short station number
under `bikes:` in its config.

On a 128×64 panel, two stations draw full size. Each block shows the station's
name, the open-dock count in large digits (green for plenty, amber for three or
fewer, red for none), the classic and e-bike counts, and a bar with one cell per
dock: open, classic, e-bike, out of service. On a 64×32 panel, or with more
stations, each block shrinks to one line. A station that isn't taking returns
dims to a dash. A small amber dot in the corner means the data is a couple of
polls late. After four minutes the numbers grey out.

## Configuration

Each binary reads a config file; the real files are gitignored. `serve` and
`simulate` read theirs at runtime; the device firmware embeds `internal/config/config.txt`
at build time (it has no filesystem). Copy the examples and edit them:

```sh
cp cmd/serve/config.example.yaml       cmd/serve/config.yaml
cp cmd/simulate/config.example.txt     cmd/simulate/config.txt
cp internal/config/config.example.txt  internal/config/config.txt   # embedded into device firmware
```

The clock and simulate configs take a `stops:` line of space-separated stop IDs.
Each stop may add an optional route filter after `:` — a comma-separated list of
tokens:

- a bare route (e.g. `Q`) is a **pin**: always shown, with **"No service"** when
  it isn't arriving;
- `!route` (e.g. `!R`) is a **mute**: never shown, even when arriving.

With no tokens, every arriving route is shown. For example, `R30N:Q,!R` always
shows Q (No service if it's down), hides R, and shows everything else live.

## Quick start

### Backend

```sh
cp cmd/serve/config.example.yaml cmd/serve/config.yaml   # adjust paths/interval if needed
go run ./cmd/fetch                                       # download GTFS zips
go run ./cmd/serve                                       # listens on :8080
```

The MTA GTFS-realtime feeds are public — no API key required.

### Simulator

The simulator layers its config over the device config: it starts from the
embedded `internal/config/config.txt` (which must exist — the build embeds it)
and `cmd/simulate/config.txt` overrides per key. Typically it only needs to
point `host` at the local backend.

```sh
cp internal/config/config.example.txt internal/config/config.txt   # if not done already
cp cmd/simulate/config.example.txt cmd/simulate/config.txt   # set host (+ any overrides)
go run ./cmd/simulate            # the clock (default)
go run ./cmd/simulate docks      # Citi Bike dock availability (needs bikes: in config)
go run ./cmd/simulate conway     # Conway's Game of Life
go run ./cmd/simulate stress     # worst-case HUB75 timing patterns
go run ./cmd/simulate bloom      # fireworks
go run ./cmd/simulate marble     # tilt maze (hold the arrow keys to lean the board)
go run ./cmd/simulate breakout   # breakout (hold left/right to lean the paddle)
```

### Device (Adafruit Matrix Portal M4 + 64×32 HUB75 panel)

```sh
cp internal/config/config.example.txt internal/config/config.txt   # WiFi + host + stops
make clock     # flash the clock to a Matrix Portal M4 (the default board)
make docks     # flash the Citi Bike dock board (stations from bikes: in config.txt)
make conway    # flash Conway's Game of Life
make stress    # flash the timing-stress patterns (Up/Down step through patterns)
make marble    # flash the tilt maze                 (tilt to roll)
make breakout  # flash Breakout                      (tilt left/right)
```

`config.txt` is embedded into the firmware at build time, so reflash after changing it.

### Playing the games on hardware

The games are steered by the board's on-board accelerometer, read by
[internal/accel](internal/accel). Both MatrixPortal boards carry an LIS3DH (at address
`0x19`, not the `0x18` the driver defaults to); the Interstate 75 W has none, so the games
build and run there but sit level.

Whatever position the board is in at power-on becomes "level" — the sensor's resting vector
is calibrated at startup and everything is measured as deviation from it, so the games play
the same flat on a desk or held in two hands.

**Calibrate the orientation before trusting a new board.** How the sensor is rotated
relative to the panel can't be determined without the hardware in hand. The M4's mapping is
measured and correct; the S3's is inherited from it and unverified:

```sh
make acceldiag   # prints the live axes over serial and draws the lean on the panel
```

Hold the board the way you want to hold it, wait for the calibration line, then lean it
**right** and note which of the DEV columns moves and in which direction — that axis and
sign is the `right` field. Lean it toward the **bottom** of the screen for `down`. Put both
into `internal/accel/accel_matrixportalm4.go` (or `..._s3.go`) and reflash. The dot on the
panel should then follow your lean.

The feel constants — `fullLean` (how far is "all the way"), `deadZone`, and `smoothing` —
are all at the top of [internal/accel/tilt.go](internal/accel/tilt.go).

Other boards build with `BOARD=`: `make clock BOARD=matrixportal-s3` or
`make clock BOARD=interstate-75w`. All three boards are hardware-verified — each engine
passes the `cmd/stress` sweep and runs the full clock on a panel (measurements in
[internal/hub75/README.md](internal/hub75/README.md)). The M4's HUB75 engines (evsys, isr)
and their tuning are documented there too; `DRIVER=isr` selects the M4's ISR-conductor
engine instead of the default evsys.

## Requirements

- Go 1.26+
- [TinyGo](https://tinygo.org/) (for the device firmware only)
- Hardware: a 64×32 HUB75 LED panel and a supported board — Adafruit Matrix Portal M4,
  Matrix Portal S3, or Pimoroni Interstate 75 W (all hardware-verified)

## Status & roadmap

| Board | Chip | Engine | WiFi | Status |
|---|---|---|---|---|
| Adafruit Matrix Portal M4 | SAMD51 | **evsys** (default) | ninafw | ✅ hardware-verified |
| Adafruit Matrix Portal M4 | SAMD51 | **isr** (`DRIVER=isr`) | ninafw | ✅ hardware-verified |
| Pimoroni Interstate 75 W | RP2350 | **pio** | cyw43439 | ✅ hardware-verified |
| Adafruit Matrix Portal S3 | ESP32-S3 | **lcd** (LCD_CAM) | espradio | ✅ hardware-verified |
| host (dev machine) | — | no-op | — | ✅ (simulator + `go test`) |

Every engine passes the `cmd/stress` worst-case sweep and runs the full clock
(incl. WiFi) on a panel. The engines, their measurements, and the optimization
backlog are documented in [internal/hub75/README.md](internal/hub75/README.md).

Next to investigate, roughly in priority order:

- **PSRAM-backed standalone clock (S3).** The S3's 2 MB PSRAM is bring-up-complete
  (`internal/psram`) — might be enough to hold the GTFS static data on-device and,
  if with on-device realtime is possible, drop the server dependency.
- **Harden the cyw43439 WiFi path (Interstate 75 W).** The clock runs over the
  lneto→netdev bridge (`internal/wifi/wifi_cyw43439.go`), so `net`/`net/http`
  work as on every board — still maturing on reconnection and sustained load.
  Untested: `GetHostByName` on hardware; `Listen`/`Accept`/`Bind`/UDP are
  refused rather than stubbed.
- **Seesaw-family input hardware.** Adafruit ANO rotary
  encoder QT, seesaw rotary encoder breakout, and NeoTrellis — all I2C devices on
  Adafruit's seesaw bridge chip.

## Memory on TinyGo — the lessons that keep paying

Hard-won on the clock app; they apply to anything long-running on a device heap.

- **Watch churn, not just live heap.** TinyGo's allocator never compacts and
  rounds growing slices to powers of two, so sustained churn breaks the heap into
  pieces until an allocation can't find one contiguous block — the clock OOM'd at
  68 KB *free*. Fix the churn (compose text on change, not per tick; reuse fetch
  buffers), not the headroom: cutting churn 16.6× (1,190 → 72 KB/min) fixed what
  4 KB of freed buffers would not have.
- **Instrument before optimizing.** Three rounds of investigation blamed the
  wrong path by inferring the cause *from* the number it was meant to explain.
  The phase-level allocation trace (`internal/client/trace.go`, enabled via
  `client.TraceAlloc`) settled it in one flash.
- **Send `Content-Length` from the server.** net/http chunks anything over its
  4 KB buffer; a length lets the device read into one exactly-sized reused buffer
  instead of de-chunking and doubling.
- **The poll loop's shape is load-bearing.** One long-lived fetch goroutine with
  reused buffers — no per-poll goroutine or buffer churn — and the render loop
  never blocks on the network: it collects the result on a later tick. It used to
  *block* through every fetch, on the belief that espradio's cold connect crashes
  if the loop runs concurrently through it; that crash was never root-caused, and
  the commit that stopped it also cut the churn 16.6× and added a DHCP settle —
  the churn fix is the likely cure. If a cold connect ever destabilizes again,
  the parked-loop arrangement is in git history (`parkDuringFetch`, removed
  2026-07-25).
- **A `Client.Timeout` is a device-side landmine.** TinyGo's net/http (≤0.41.1)
  never enforces the deadline but panics on any failed request when one is set
  (`didTimeout` is nil on the round-trip error path). The client sets it on the
  host only (`internal/client/http_tinygo.go`).
