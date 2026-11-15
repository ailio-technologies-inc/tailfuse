//go:build android

package engine

import (
	"errors"

	"tailscale.com/net/netns"
)

// setSocketProtector makes every socket Tailscale opens in this process
// (WireGuard, DERP, control) bypass the VPN, as tailscale-android does.
func setSocketProtector(protect func(fd int) bool) {
	netns.SetAndroidProtectFunc(func(fd int) error {
		if !protect(fd) {
			return errors.New("vpnservice.protect failed")
		}
		return nil
	})
}
