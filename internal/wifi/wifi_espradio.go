//go:build tinygo && espradio

// Native ESP32-S3 WiFi backend (Adafruit MatrixPortal S3 and other esp32s3/esp32c3 boards),
// via tinygo.org/x/espradio — the radio support TinyGo 0.41 added for Espressif chips. Unlike
// the M4 (ninafw coprocessor) and the Interstate 75 W (cyw43439 companion chip), the ESP32-S3
// IS the radio: espradio drives the on-die MAC/PHY and registers as a netdev, so the arrivals
// client's default net/http transport works with no manual stack (contrast wifi_cyw43439.go).
//
// Selected with the `espradio` build tag (a feature tag, like ninafw/cyw43439 — not a board
// name): build the clock with `-tags=hub75_lcd,espradio` on `-target=esp32s3-generic`.
package wifi

import (
	"time"

	"tinygo.org/x/drivers/netdev"
	nl "tinygo.org/x/drivers/netlink"
	link "tinygo.org/x/espradio/netlink"
)

// esplink is the espradio netdev. It is package-level because netdev.UseNetdev keeps a
// pointer to it for the life of the program (net/http reaches the radio through it).
var esplink link.Esplink

// Connect registers the espradio netdev and joins the network, retrying a few times (the
// radio can report a transient failure right after reset). Mirrors the ninafw backend; once
// it returns nil the standard net stack is up and net/http will resolve and dial.
func Connect(ssid, pass string) error {
	netdev.UseNetdev(&esplink)
	attempts := 0
	for {
		err := esplink.NetConnect(&nl.ConnectParams{
			Ssid:       ssid,
			Passphrase: pass,
		})
		if err == nil || err == nl.ErrConnected {
			if ip, aerr := esplink.Addr(); aerr == nil { // report the lease: a bad one explains a stalled dial
				println("wifi: dhcp lease", ip.String())
			} else {
				println("wifi: no ip:", aerr.Error())
			}
			// espradio's stack goroutine keeps warming up after association (ARP to the
			// gateway, socket pool). The very first outbound dial races that warm-up and can
			// stall — and espradio v0.1.0 gives no connect deadline to bound it. This settle
			// lets the stack finish so the first fetch usually connects on the first try; the
			// clock's fetchTimeout watchdog is the backstop if it still doesn't.
			time.Sleep(2 * time.Second)
			return nil
		}
		attempts++
		if attempts >= 5 {
			return err
		}
		time.Sleep(5 * time.Second)
	}
}
