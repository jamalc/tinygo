//go:build tinygo

// Command bloom is a fireworks showcase for the HUB75 drivers — built to make the panel
// look brilliant rather than to test it. The animation lives in internal/bloom; this is
// just the device glue.
//
// It leans on GetPixel, the framebuffer read-back every hub75 driver exposes, so it runs on
// whichever driver the build target selects. The same animation also runs on the host via:
// go run ./cmd/simulate bloom.
package main

import (
	"github.com/jamalc/tinygo/internal/bloom"
	"github.com/jamalc/tinygo/internal/config"
	"github.com/jamalc/tinygo/internal/hub75"
)

func main() {
	cfg := config.New()

	d, err := hub75.New(cfg.HUB75())
	if err != nil {
		println("hub75:", err.Error())
		return
	}

	bloom.Run(cfg, d)
}
