//go:build tinygo

// Command conway is a Conway's Game of Life animation for testing the display.
//
// Usage:
//
//	make conway [BOARD=matrixportal-m4]
package main

import (
	_ "embed"

	"github.com/jamalc/tinygo/internal/config"
	"github.com/jamalc/tinygo/internal/conway"
	"github.com/jamalc/tinygo/internal/hub75"
)

func main() {
	cfg := config.New()

	panel, err := hub75.New(cfg.HUB75())
	if err != nil {
		panic(err)
	}

	conway.Run(cfg, panel)
}
