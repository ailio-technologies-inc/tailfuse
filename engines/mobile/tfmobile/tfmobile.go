// Package tfmobile is the gomobile surface of the Tailfuse mobile engine.
// See ../API.md. Only gomobile-compatible types appear here.
package tfmobile

import (
	"sync"

	engine "github.com/ailio-technologies-inc/tailfuse/engines/mobile"
)

// Listener receives change events.
type Listener interface {
	// OnChange is called from a background goroutine, coalesced to at most a
	// few times a second. kind is "state" or "domains".
	OnChange(kind string)
}

var (
	once sync.Once
	eng  *engine.Engine
)

func get() *engine.Engine {
	once.Do(func() {
		// Packets: both shells can hand over their tun (all-apps mode).
		eng = engine.New(engine.Options{NodeFactory: engine.TSNetFactory, Packets: true})
	})
	return eng
}

// Start loads the config from stateDir (created if missing), starts the
// router on 127.0.0.1:43100 and brings up every tailnet that is not paused.
// Safe to call twice.
func Start(stateDir string) error { return get().Start(stateDir) }

// Stop shuts every node and the router down. Safe to call when not started.
func Stop() { get().Stop() }

// Running reports whether Start has succeeded and Stop has not been called since.
func Running() bool { return get().Running() }

// Call runs one JSON request and returns one JSON response. It never panics
// and never returns an empty string.
func Call(request string) string { return get().Call(request) }

// SetStateDir tells Call where the config lives when the engine is not started.
func SetStateDir(stateDir string) { get().SetStateDir(stateDir) }

// SetListener registers the one listener for change events (nil clears it).
func SetListener(l Listener) {
	if l == nil {
		get().SetListener(nil)
		return
	}
	get().SetListener(l.OnChange)
}

// SetInterfaces gives the engine the device's network interfaces (Android
// only), in tailscale-android's getInterfacesAsString format. A malformed
// list is ignored.
func SetInterfaces(text string) { _ = engine.SetInterfaces(text) }

// SetDeviceName gives the engine the name this device should take on each
// tailnet (the phone's own name, e.g. "Ada's iPhone"); the engine turns it
// into a valid hostname. Call before Start. Without it the engine falls back
// to "tailfuse-<os>".
func SetDeviceName(name string) { get().SetDeviceName(name) }

// SetPassthrough lets names no tailnet owns be connected directly from the
// device instead of refused. Android only: its VPN can only hand apps one
// plain HTTP proxy for all traffic. Never on iOS, where matchDomains already
// keeps other traffic away from the router. May be called before Start.
func SetPassthrough(on bool) { get().SetPassthrough(on) }

// StartPackets attaches the phone's tun for all-apps mode, after the shell
// has applied the all-apps network settings. iOS: fd -1 (the engine finds
// the packet tunnel's utun fd itself) and utun true; the fd stays the
// system's. Android: the fd from ParcelFileDescriptor.detachFd() and utun
// false; the engine owns and closes it. On failure the engine falls back to
// proxy mode for this session and reports why in state.all_apps_error (the
// shell then re-applies proxy settings on the "domains" event that follows).
func StartPackets(fd int, utun bool) error { return get().StartPacketsFD(fd, utun) }

// StopPackets detaches the tun. Call before replacing it (Android
// re-establish) or before Stop. Safe when none is attached.
func StopPackets() { get().StopPackets() }

// SetDNSServers gives the engine the underlying (non-VPN) network's DNS
// servers, one per line ("1.1.1.1", "[2001:db8::1]:53"). Android only: names
// no tailnet owns are forwarded there. Call at start and on every network
// change.
func SetDNSServers(list string) { get().SetDNSServers(list) }

// Protector keeps a socket out of the VPN (Android: VpnService.protect).
type Protector interface {
	Protect(fd int) bool
}

// SetProtector registers the protector (Android only), before Start. Every
// socket the nodes open, and the upstream DNS sockets, are protected.
func SetProtector(p Protector) {
	if p == nil {
		return
	}
	get().SetProtect(p.Protect)
}
