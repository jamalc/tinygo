//go:build tinygo && ninafw

package wifi

import (
	"time"

	"tinygo.org/x/drivers/netlink"
	"tinygo.org/x/drivers/netlink/probe"
)

func Connect(ssid, pass string) error {
	link, _ := probe.Probe()
	attempts := 0
	for {
		err := link.NetConnect(&netlink.ConnectParams{
			Ssid:       ssid,
			Passphrase: pass,
		})
		if err == nil || err == netlink.ErrConnected {
			return nil
		}
		attempts++
		if attempts >= 5 {
			return err
		}
		time.Sleep(5 * time.Second)
	}
}
