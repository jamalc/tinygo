//go:build !tinygo

package client

import (
	"net/http"
	"time"
)

// httpClient has an explicit timeout so a hung connection can't stall a fetch:
// on the host (simulator, tests) net/http enforces it. The device builds must
// not set one — see http_tinygo.go.
var httpClient = &http.Client{Timeout: 10 * time.Second}
