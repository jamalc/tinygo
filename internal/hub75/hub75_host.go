//go:build !atsamd51 && !rp2350 && !esp32s3

// Host backend: no hardware. It lets the package build and run under `go test`, and the apps
// build on a dev machine, when no chip target is selected. The engine drives nothing — the
// Device is a pure framebuffer: SetPixel/Clear mutate the grid (in hub75.go) and present is a
// no-op. So the package and its consumers compile and run unchanged off-target.
package hub75

// driverName is this build's engine name, exposed via hub75.DriverName.
const driverName = "host"

// engine is the host no-op backend. It holds no state and drives no silicon; every method is
// present so the generic Device in hub75.go compiles and runs against it.
type engine struct{}

// start succeeds without touching hardware — the framebuffer is the whole story on host.
func (e *engine) start(cfg Config, pix []rgb) error { return nil }

// present is a no-op: the framebuffer is already the source of truth and there is nothing to
// refresh.
func (e *engine) present(pix []rgb) error { return nil }

// setBrightness is a no-op on host; with no engine, brightness has no observable effect.
func (e *engine) setBrightness(b uint8) {}
