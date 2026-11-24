//go:build tinygo

// Command marble is the tilt-maze game, as device firmware.
//
// The ball rolls under the panel's own lean: the accelerometer feeds
// internal/marble as an analog gravity vector, unquantized. Whichever way the
// board is held at power-on becomes level, so tilting from there is what moves
// the ball.
//
// Usage:
//
//	make marble [BOARD=matrixportal-m4]
package main

import (
	_ "embed"

	"github.com/jamalc/tinygo/internal/accel"
	"github.com/jamalc/tinygo/internal/config"
	"github.com/jamalc/tinygo/internal/hub75"
	"github.com/jamalc/tinygo/internal/marble"
)

func main() {
	cfg := config.New()

	panel, err := hub75.New(cfg.HUB75())
	if err != nil {
		panic(err)
	}

	marble.Run(cfg, panel, accel.New())
}
