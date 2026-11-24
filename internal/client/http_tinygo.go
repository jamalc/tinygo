//go:build tinygo

package client

import "net/http"

// httpClient carries NO timeout, deliberately. TinyGo's net/http port (through
// at least 0.41.1) removed the upstream timeout machinery but kept the
// didTimeout() call in Client.do: with a Timeout set, any transport-level
// failure — a refused or reset connect, exactly what a flaky network serves —
// calls a nil didTimeout func and panics the firmware (net/http/client.go:461;
// send()'s didTimeout named return is never assigned on the roundTrip path).
// And the port never enforces the deadline anyway (computed, threaded through,
// armed nowhere), so a timeout here bought no protection in exchange for the
// panic. The poll loop's own fetchTimeout (internal/clock) surfaces a slow
// fetch, and the netdev backends time dead connects out on their own schedule.
//
// TinyGo's net/http also does not pool connections: each poll dials fresh and
// releases the socket on Body.Close — nothing to keep-alive-disable, which
// matters given espradio's single TCP port and the lneto bridge's two-slot
// socket table.
var httpClient = &http.Client{}
