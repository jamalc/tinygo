//go:build !tinygo

package wifi

func Connect(ssid, pass string) error {
	return nil
}
