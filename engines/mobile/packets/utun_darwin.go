//go:build darwin

package packets

import (
	"errors"

	"golang.org/x/sys/unix"
)

// From <sys/sys_domain.h> and <net/if_utun.h>.
const (
	sysprotoControl = 2
	utunOptIfname   = 2
)

// FindUtunFD returns the file descriptor of this process's utun interface:
// inside an iOS packet tunnel extension, the one NEPacketTunnelProvider
// opened. It scans the descriptor table for a kernel-control socket that
// answers UTUN_OPT_IFNAME, the technique wireguard-apple (MIT) and
// Tailscale's iOS client use; no private API is called. Call it after
// setTunnelNetworkSettings has completed.
func FindUtunFD() (int, error) {
	for fd := 0; fd < 1024; fd++ {
		name, err := unix.GetsockoptString(fd, sysprotoControl, utunOptIfname)
		if err == nil && len(name) > 4 && name[:4] == "utun" {
			return fd, nil
		}
	}
	return -1, errors.New("packets: no utun file descriptor in this process")
}
