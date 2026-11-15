package engine

import (
	"fmt"
	"net"
	"strings"
	"sync"

	"tailscale.com/net/netmon"
)

// ParseInterfaces reads the interface list in tailscale-android's
// getInterfacesAsString format, one interface per line:
//
//	wlan0 30 1500 true true false false true | fe80::2f60:2c82:4163:8389%wlan0/64 10.1.10.131/24
//
// name index mtu up broadcast loopback pointToPoint multicast | addresses (CIDR).
func ParseInterfaces(text string) ([]netmon.Interface, error) {
	var out []netmon.Interface
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		head, addrs, ok := strings.Cut(line, "|")
		if !ok {
			return nil, fmt.Errorf("interface line %q has no '|'", line)
		}
		var (
			name                                         string
			index, mtu                                   int
			up, broadcast, loopback, pointToPoint, multi bool
		)
		if _, err := fmt.Sscanf(strings.TrimSpace(head), "%s %d %d %t %t %t %t %t",
			&name, &index, &mtu, &up, &broadcast, &loopback, &pointToPoint, &multi); err != nil {
			return nil, fmt.Errorf("interface line %q: %v", line, err)
		}
		ifc := &net.Interface{Name: name, Index: index, MTU: mtu}
		for flag, on := range map[net.Flags]bool{
			net.FlagUp: up, net.FlagBroadcast: broadcast, net.FlagLoopback: loopback,
			net.FlagPointToPoint: pointToPoint, net.FlagMulticast: multi,
		} {
			if on {
				ifc.Flags |= flag
			}
		}
		// Non-nil AltAddrs: netmon uses these instead of asking the OS.
		alt := []net.Addr{}
		for _, a := range strings.Fields(addrs) {
			// Drop an IPv6 zone ("fe80::1%wlan0/64").
			if i := strings.Index(a, "%"); i >= 0 {
				if j := strings.Index(a[i:], "/"); j >= 0 {
					a = a[:i] + a[i+j:]
				} else {
					a = a[:i]
				}
			}
			ip, ipnet, err := net.ParseCIDR(a)
			if err != nil {
				continue
			}
			alt = append(alt, &net.IPNet{IP: ip, Mask: ipnet.Mask})
		}
		out = append(out, netmon.Interface{Interface: ifc, AltAddrs: alt})
	}
	return out, nil
}

var (
	ifMu       sync.Mutex
	ifList     []netmon.Interface
	ifGetterOn bool
)

// SetInterfaces gives Tailscale's network monitor the device's interfaces,
// for Android, where Go cannot list them itself. The monitor polls the getter
// (every 10 s on Android), so a new list is picked up without a restart.
func SetInterfaces(text string) error {
	list, err := ParseInterfaces(text)
	if err != nil {
		return err
	}
	ifMu.Lock()
	ifList = list
	register := !ifGetterOn
	ifGetterOn = true
	ifMu.Unlock()
	if register {
		netmon.RegisterInterfaceGetter(func() ([]netmon.Interface, error) {
			ifMu.Lock()
			defer ifMu.Unlock()
			return append([]netmon.Interface(nil), ifList...), nil
		})
	}
	return nil
}
