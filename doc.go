// Package tinygo is a personal collection of TinyGo hardware projects. It
// began as a NYC subway arrival clock and grew into a lab: HUB75 scanout
// engines for three boards, a fleet of panel apps, and from-scratch drivers
// for the hardware around them. Everything lives under internal/ on purpose —
// nothing is promoted for outside use until it has proven stable.
//
// The centerpiece is a 64x32 HUB75 LED matrix and the apps that draw on it:
//
//   - cmd/clock, the subway clock. A backend service (cmd/serve) merges the
//     MTA's static GTFS data with its GTFS-realtime feeds and exposes per-stop
//     arrivals over a small HTTP API (cmd/fetch downloads the static zips);
//     the clock polls that API and animates arrivals on the panel.
//   - cmd/conway, cmd/marble, cmd/breakout, cmd/bloom — games and toys.
//   - cmd/stress (worst-case timing patterns), cmd/bench (driver
//     measurements), cmd/acceldiag and cmd/psramprobe (hardware bring-up).
//   - cmd/simulate — runs any display app on the host, rendered with Ebiten.
//
// The portable app logic lives in internal/clock, internal/conway, and
// friends, shared by device and simulator through a small Display interface
// that both hub75.Device (TinyGo) and simulator.Display (host) satisfy. User
// input flows through internal/input's discrete and analog ports.
//
// The driver work sits alongside the apps: internal/hub75 (one scanout engine
// per chip — SAMD51, RP2350, ESP32-S3 — selected at build time),
// internal/psram (ESP32-S3 quad PSRAM bring-up), internal/wifi (one network
// transport per board), and internal/accel (tilt input).
//
// See README.md for build and run instructions and roadmap,
// and the cmd/*/config.example.* files for configuration.
package tinygo
