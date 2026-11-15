package engine

import (
	"net"
	"testing"

	"tailscale.com/net/netmon"
)

func TestParseInterfaces(t *testing.T) {
	text := "wlan0 30 1500 true true false false true | fe80::2f60:2c82:4163:8389%wlan0/64 10.1.10.131/24\n" +
		"r_rmnet_data0 21 1500 true false false false false | fe80::9318:6093:d1ad:ba7f%r_rmnet_data0/64\n" +
		"lo 1 65536 true false true false false | ::1/128 127.0.0.1/8\n" +
		"dummy0 3 1500 false false false false false | \n\n"
	ifs, err := ParseInterfaces(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(ifs) != 4 {
		t.Fatalf("got %d interfaces", len(ifs))
	}
	w := ifs[0]
	if w.Name != "wlan0" || w.Index != 30 || w.MTU != 1500 || w.Flags&net.FlagUp == 0 || w.Flags&net.FlagBroadcast == 0 || w.Flags&net.FlagMulticast == 0 || w.Flags&net.FlagLoopback != 0 {
		t.Fatalf("wlan0: %+v", w.Interface)
	}
	if len(w.AltAddrs) != 2 || w.AltAddrs[1].String() != "10.1.10.131/24" || w.AltAddrs[0].String() != "fe80::2f60:2c82:4163:8389/64" {
		t.Fatalf("wlan0 addrs: %v", w.AltAddrs)
	}
	if !ifs[2].IsLoopback() {
		t.Fatal("lo is not loopback")
	}
	if ifs[3].AltAddrs == nil || len(ifs[3].AltAddrs) != 0 || ifs[3].Flags&net.FlagUp != 0 {
		t.Fatalf("dummy0: %+v %v", ifs[3].Interface, ifs[3].AltAddrs)
	}
	if _, err := ParseInterfaces("garbage without bar"); err == nil {
		t.Fatal("garbage accepted")
	}
	if _, err := ParseInterfaces("wlan0 x 1500 true true false false true | "); err == nil {
		t.Fatal("bad number accepted")
	}
}

func netmonInterfaces() (netmon.InterfaceList, error) { return netmon.GetInterfaceList() }
