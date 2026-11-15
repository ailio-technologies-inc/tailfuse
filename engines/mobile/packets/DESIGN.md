# All-apps mode: design as built

All-apps mode lets every app on the phone reach every running tailnet by name
and by address. That covers SSH clients, database tools and anything else that
opens sockets, not only proxy-aware browsers. It works the way the official
Tailscale app does for one tailnet, but for several tailnets at once. It adds
to the proxy/router path and can be switched off: proxy mode stays unchanged
for devices that can't afford it.

This package (`engines/mobile/packets`) is the Go side; "Wiring" below covers
how the engine, `tfmobile` and the two native shells use it.

## The decision: translate packets, don't terminate connections

The brief proposed terminating every app flow in a gVisor netstack on the tun
and re-dialling it through the owning node (`tsnet.Server.Dial`). Phase 1
built a different design, because it measured much better on memory and
needs no tsnet fork.

**Built: per-tailnet NAT into each node's own WireGuard engine.**
`tsnet.Server` has a `Tun tun.Device` field (tailscale.com v1.104.7). When it
is set, the node's WireGuard engine reads outgoing packets from that device
and writes incoming ones to it. The node's own netstack is still created, but
it only handles the node's own `Dial`/`Listen` sockets
(`CheckLocalTransportEndpoints`), so the proxy path keeps working on the same
node. `Mux.Link(tailnet)` returns that device for each tailnet. The packet
path works like this:

```
app ─► phone kernel ─► utun / VpnService fd ─► Mux.Run (reads packets)
     src 198.18.0.1:p   dst 198.18.1.7:22 (synthetic) or 100.x:22 (unique real)
        │  new flow? classify the destination → (tailnet, real address) or refuse (RST / ICMP)
        │  rewrite: src → that node's own 100.x, dst → real; incremental checksums
        ▼
     link "work" (tun.Device) ─► that tsnet node's WireGuard ─► peer
     peer reply ─► WireGuard ─► link.Write ─► flow table (reverse) ─► rewrite back ─► fd ─► app
```

- Each flow keeps about 600 bytes of state: a flow-table entry (TCP
  connection, UDP association or ICMP echo id) holding two map keys and a few
  flags. Nothing per flow is buffered. TCP windows, retransmits and
  congestion control stay in the phone's kernel and the peer, where they are
  free.
- ICMP echo just works: ping reaches the peer and its reply comes back.
  ICMP errors from the tailnet (port unreachable, packet too big, time
  exceeded) are translated too, including the quoted inner header, so UDP
  "connection refused" and PMTU discovery behave correctly.
- The packet path allocates nothing per packet (`BenchmarkRoundTrip`:
  0 B/op). Buffers come from a pool, and each tailnet's queue is bounded.
- Proven end to end on real nodes: `testdata/integration` runs two tailnets on
  Tailscale's `testcontrol` and DERP. Both tailnets give their server
  `100.64.0.1` and the phone `100.64.0.2`. DNS answers `198.18.1.0` and
  `198.18.1.1`, and 256 KiB echoes correctly through each tailnet. On the same
  nodes, `tsnet.Server.Dial` with `Tun` set still works (the proxy path).

**Options considered**

| Option | Memory (measured on this Mac or reasoned) | Verdict |
|---|---|---|
| A. Terminate in our gVisor stack, re-dial with `tsnet.Dial` (the brief) | gVisor needs about 6 KiB per idle endpoint and about 50 KiB per endpoint with 64 KiB queued (`TestMemoryTermination`: 12 / 111 KiB per connection *pair*). Each flow is terminated twice: once in our stack, once in the node's netstack, where Tailscale's iOS buffer sizes are gVisor's defaults (1 MiB default, 4 MiB max, which can't be tuned without forking tsnet). Even 200 busy flows would come to roughly 20–40 MiB. | Rejected: memory scales with traffic, double buffering, half the throughput. |
| B. **NAT into each node's WireGuard (`Server.Tun`)** | Mux idle: 0.2 MiB heap. Flows: ~600 B each, capped at 4096 (≤2.4 MiB). Queues: ≤340 KiB per tailnet. | **Built.** |
| C. Packet I/O and/or the DNS responder in Swift (Network.framework) or Kotlin | Moving packets through `packetFlow` and gomobile costs one ObjC/Java allocation and a copy per packet, each way. A native DNS responder would need the ownership rules duplicated in two languages, or a callback per query. | Rejected. The one native piece left is fd discovery, and it is done in Go (`FindUtunFD`). |
| D. lwIP-based tun2socks | Lighter than gVisor per flow, but it still terminates. It adds a C dependency to the gomobile build, and B makes it unnecessary. | Rejected. |
| E. One shared netstack for all nodes / nodes without netstack | tsnet always creates its netstack (needed for `Dial`/`Listen`). B already removes our extra stack, and no app traffic touches the per-node netstacks. Dropping them entirely would mean forking tsnet (as tailmix does) and moving the proxy path onto B. | Deferred. Possible later as a fork; not needed to fit the budget. |

Ideas, not code, came from prior art. From `maisem/tailmix` (BSD-3): one
tun, one engine per tailnet, translation rather than termination, a
persisted pool keyed by (profile, canonical IP), and reclaiming only after
long disuse. From `franticplant/tailmesh-android` (BSD-3): DNS on an address
inside the tunnel over UDP and TCP, failing closed on ambiguity, and bounded
UDP lifetimes. tailmix forks tsnet; `Server.Tun` makes that unnecessary here.
No source from either project was copied. The utun fd scan is the documented
wireguard-apple (MIT) and Tailscale technique, reimplemented in Go.

## Address ranges and why

| What | Address | Why |
|---|---|---|
| Synthetic IPv4 pool | `198.18.0.0/15` (handed out from `198.18.1.0`) | RFC 2544 benchmarking space: never a real destination, never used by carriers (100.64/10, 10/8) or home LANs (192.168/16), never a Tailscale address. The iOS shell already uses 198.18.0.1. |
| Synthetic IPv6 pool | `fd5e:9b07:4a1c::/48` (from `fd5e:9b07:4a1c:1::`) | RFC 4193 ULA with a random global ID: cannot clash with Tailscale's `fd7a:115c:a1e0::/48`. |
| Tunnel's own address | `198.18.0.1`, `fd5e:9b07:4a1c::1` | The source of every app packet. |
| DNS responder | `198.18.0.2`, `fd5e:9b07:4a1c::2`, port 53, UDP and TCP | Inside the routed pool, so no extra route is needed. |
| Tailscale ranges (routed too) | `100.64.0.0/10`, `fd7a:115c:a1e0::/48` | Real tailnet addresses work when exactly one running tailnet has that address (RouteTable's existing unique-ownership rule). |

A synthetic address maps to a (tailnet, real address) pair and is stable:
the mapping is persisted (`PoolOptions.File`, proposed
`<state>/synthetic-addresses.json`). Each family is capped at 4096 entries.
When the pool is full, an entry unused for an hour is reclaimed, and an
active flow is never affected because it holds the resolved pair.

**Which address DNS hands out.** If the answer is a Tailscale address that
only this tailnet has, the real address is returned, with TTL capped at 60 s
because uniqueness can change. Otherwise a synthetic address is returned,
with TTL capped at 300 s. That includes an address two tailnets share, and
an address not yet learned. A non-Tailscale address is returned unchanged
(reached over the phone's own network, like the proxy path), unless the
optional `RouteChecker` says the tailnet routes it (subnet routes). Then it
gets a synthetic address too.

## Packet rules

A new flow is classified once. After that, packets use the flow table.

- **Synthetic destination:** the pool gives (tailnet, real). An unused
  synthetic address is refused.
- **Tailscale address:** `Rules.ClassifyAddr` must return `Owned`. Conflict
  and TailnetSpace are refused, never guessed.
- **Anything else in the tunnel:** refused.
- The owning tailnet must have a link (a running node) and its own address
  of that family (`SetAddrs`). Otherwise the flow is refused.
- **How a refusal looks:** TCP gets a RST (an immediate "connection
  refused"). UDP and other protocols get ICMP destination unreachable,
  administratively prohibited. ICMP echo to a refused address gets the same.
  Multicast and the tunnel's own address are dropped silently.
- A TCP segment that is not a SYN and has no flow (after expiry or a node
  restart) gets a RST.
- **Inbound:** packets from a tailnet that no flow asked for are dropped. In
  this mode the phone does not accept connections from tailnets.
- **Limits (`DefaultLimits`):** 4096 flows. Idle timeouts: TCP 4 h, TCP
  opening 30 s, TCP closing 10 s, UDP 2 min, ICMP 30 s. 256 packets queued
  per tailnet, then drops. At most 32 DNS queries in flight. The DNS cache
  holds 512 entries. At most 16 DNS-over-TCP connections.
- When a node stops (`link.Close`, which tsnet calls) or its addresses
  change, its flows are dropped and its DNS cache is flushed.

## DNS rules

These are the same ownership rules as the proxy path. `Rules.ClassifyName`
is `RouteTable.Classify`: longest suffix wins, then learned split-DNS
domains, then remembered hosts.

| Name | Answer |
|---|---|
| Owned, tailnet running | A/AAAA from that tailnet's own DNS (`TailnetDNS.QueryDNS` = `local.Client.QueryDNS`), mapped as above. NXDOMAIN passes through. HTTPS/SVCB get NODATA, because their address hints would bypass the mapping. Other types (TXT, SRV, PTR…) are relayed with address records removed. |
| Owned, but tailnet paused / not running | SERVFAIL (QueryDNS fails). **Never forwarded.** |
| Conflict (two tailnets) | REFUSED |
| Claimed (an off tailnet's last domains) | SERVFAIL, never forwarded |
| Reverse name in Tailscale or synthetic space | PTR from the one owning tailnet, otherwise NXDOMAIN. Never forwarded. |
| Unowned, iOS (`Upstream == nil`) | REFUSED (iOS only sends us `matchDomains`) |
| Unowned, Android | Forwarded raw to the device's resolvers (`Resolvers`: UDP, then TCP if truncated). Not cached here; Android's resolver caches. |

UDP answers respect 512 bytes, or the client's EDNS size up to 1232. A
larger answer is sent with TC set, and the client retries over TCP. DNS over
TCP is served by a small gVisor stack (16 KiB windows) bound only to the two
DNS addresses. It starts on the first TCP SYN to port 53 and stops after
2 min idle, so it costs nothing while unused. Lookups are cached per
(tailnet, name, type) with the record TTL. The cache stores real addresses;
the real-or-synthetic choice is made fresh on every answer. Negative answers
are cached for 15 s.

The no-leak guarantee matches `passthrough.go`. Only `Unowned` names ever
reach `Upstream`. Owned, conflicting, claimed and paused names do not, and
neither do reverse names in tailnet space. Tests cover each case
(`TestDNSRefusals`, `TestDNSReverse`).

## iOS: utun fd, not packetFlow

**Decision: read the utun fd in Go** (`FindUtunFD()` +
`NewFileDevice(fd, utun=true, own=false)`).

- `packetFlow.readPackets` / `writePackets` through gomobile means, for every
  packet, an `NSData` and `[NSNumber]` on the Swift side, a cgo crossing, and
  a Go `[]byte` copy. That is two allocations and two copies per packet in
  each direction, plus the GC and ObjC autorelease pressure that comes with
  them, inside a 50 MB process.
- With the fd, Go reads and writes the kernel socket directly on its poller.
  The only per-packet work is the 4-byte `AF_INET`/`AF_INET6` header (one
  pooled copy on write). This is what wireguard-apple and Tailscale's iOS
  client ship in the App Store. The scan uses only public API:
  `getsockopt(SYSPROTO_CONTROL, UTUN_OPT_IFNAME)` on each fd.
- `own=false`: the system owns the fd. Stopping sets a read deadline and
  never closes the fd.
- If a future iOS breaks the scan, `MemDevice` is the fallback. Swift would
  pump `packetFlow` into it through a gomobile `PacketIO` callback; that is
  slower but correct.

## Shell configuration

### iOS (`PacketTunnelProvider`, all-apps mode)

```swift
let s = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: "127.0.0.1")
let v4 = NEIPv4Settings(addresses: ["198.18.0.1"], subnetMasks: ["255.255.255.255"])
v4.includedRoutes = [NEIPv4Route(destinationAddress: "198.18.0.0", subnetMask: "255.254.0.0"),
                     NEIPv4Route(destinationAddress: "100.64.0.0", subnetMask: "255.192.0.0")]
s.ipv4Settings = v4
let v6 = NEIPv6Settings(addresses: ["fd5e:9b07:4a1c::1"], networkPrefixLengths: [128])
v6.includedRoutes = [NEIPv6Route(destinationAddress: "fd5e:9b07:4a1c::", networkPrefixLength: 48),
                     NEIPv6Route(destinationAddress: "fd7a:115c:a1e0::", networkPrefixLength: 48)]
s.ipv6Settings = v6
if !domains.isEmpty {                       // NEVER set DNS settings with no match domains:
  let dns = NEDNSSettings(servers: ["198.18.0.2", "fd5e:9b07:4a1c::2"])  // that would send ALL DNS to us
  dns.matchDomains = domains                // state.match_domains, as today
  dns.matchDomainsNoSearch = true
  s.dnsSettings = dns
}
s.mtu = 1280
// No proxySettings in all-apps mode: every app, browsers included, uses the routes.
```

- No `excludedRoutes` are needed. The extension's own sockets (WireGuard
  UDP, DERP, control) go to public addresses outside the included routes.
- After `setTunnelNetworkSettings` completes: `fd = FindUtunFD()`, then
  `StartPackets(fd, utun: true)`. Delete `drainPackets()`: calling
  `packetFlow.readPackets` would steal packets from the fd.
- `includeAllNetworks` and `enforceRoutes` stay false.
- Proxy mode keeps today's settings exactly.

### Android (`TailfuseVpnService`, all-apps mode)

```kotlin
Builder().setSession("Tailfuse").setMtu(1280)
  .addAddress("198.18.0.1", 32).addAddress("fd5e:9b07:4a1c::1", 128)
  .addRoute("198.18.0.0", 15).addRoute("100.64.0.0", 10)
  .addRoute("fd5e:9b07:4a1c::", 48).addRoute("fd7a:115c:a1e0::", 48)
  .addDnsServer("198.18.0.2").addDnsServer("fd5e:9b07:4a1c::2")
  .setMetered(false)
// no setHttpProxy, no allowFamily (we own IPv6 routes now), no drain thread
val fd = builder.establish()!!.detachFd()
Tfmobile.startPackets(fd, false)   // Go owns and closes the fd
```

- **`setHttpProxy` and passthrough can be removed entirely in this mode**,
  which fixes "PAC not supported over VPN". Owned names resolve through our
  DNS to routable addresses. Everything else is forwarded to the real
  resolvers and connected directly by the app. Call
  `Tfmobile.setPassthrough(false)`; the router stays for the ports feature
  and the `Call` API.
- Every query reaches the VPN DNS server, so the shell must give the engine
  the underlying network's resolvers (`LinkProperties.dnsServers` of the
  default non-VPN network). Push them at start and whenever the network or
  its link properties change.
- Protect the engine's sockets with `VpnService.protect(fd)` through
  Tailscale's `netns.SetAndroidProtectFunc`. A carrier DNS server or WireGuard
  endpoint inside 100.64.0.0/10 (carrier-grade NAT) would otherwise loop into
  the tunnel. Until that is in, `Resolvers` drops upstream servers in
  tunnel/Tailscale space.

## gomobile surface (tfmobile)

```go
// The mode is the set_all_apps command (saved; on by default), not a function.
// StartPackets attaches the tun. iOS: fd = -1 means "find the utun fd" (FindUtunFD), utun = true.
// Android: the detached VpnService fd, utun = false (Go closes it).
func StartPackets(fd int, utun bool) error
// StopPackets detaches the tun (call before Stop; idempotent).
func StopPackets()
// SetDNSServers (Android): the underlying network's resolvers, one per line ("1.1.1.1", "[2001:db8::1]:53").
func SetDNSServers(list string)
// Protector (Android): VpnService.protect for the engine's own sockets.
type Protector interface{ Protect(fd int) bool }
func SetProtector(p Protector)
```

`state` has `all_apps`, `all_apps_active`, `all_apps_error` and
`"tunnel": {"addresses":[…],"routes":[…],"dns":[…],"mtu":1280}` (API.md), so
neither shell hard-codes the ranges. `match_domains` feeds
`NEDNSSettings.matchDomains`.

## Memory budget and measurements

All numbers are from **this Mac** (Apple M4 Pro, Go heap under the iOS
settings: 32 MiB soft limit, GC 50). They show how costs grow and compare
the designs; they are not iPhone numbers. The real check is the shell's
`logMemory` (`phys_footprint`) on a device.

| Item | Measured / bound |
|---|---|
| Two idle tsnet nodes (owner's earlier Mac measurement) | ~31 MB RSS |
| Mux, no flows | 0.2 MiB heap, 0.5 MiB RSS |
| Flow table | ~600 B/flow: 1000 flows = 0.6 MiB, 4000 = 2.3 MiB (cap 4096), 16 000 = 9.1 MiB |
| Per-packet path | 0 allocations per round trip (`BenchmarkRoundTrip`) |
| Throughput | ~440–910 Mbit/s each way through the Mux with 4000 active flows (Mac, in-memory device) |
| Node queues | ≤ 256 × 1.3 KiB = 340 KiB per tailnet |
| Pool | ≤ 4096 entries per family (~150 B each) |
| DNS | 512 cache entries (≪100 KiB); TCP stack only while in use |
| Peak heap during the 4000-flow flood | +24 MiB over idle. This is the harness's own per-packet copies (MemDevice, the reflector), not the Mux, which allocates 0 B per packet. A real fd device does not copy. |
| Comparison, option A | gVisor: 12 KiB per idle connection pair, ~100–110 KiB per pair with 64 KiB queued (both buffer settings) |
| wireguard-go on iOS (exists in both designs) | pools capped at 1024 × 1700 B per node under load |

**Budget (iOS, 2–3 tailnets):** about 31 MB for the nodes, plus ≤ 3 MB for
the Mux, plus wireguard-go's buffers under load. That leaves headroom under
50 MB; option A would not. Measure with
`PACKETS_MEM=1 go test -run Memory -v ./packets/` and
`go test -bench RoundTrip ./packets/`.

## Known gaps

- **Subnet routes (`accept_routes`)** work by name when the adapter
  implements `RouteChecker` (synthetic addresses). Raw subnet addresses
  (10.x typed by hand) are not routed; the phone's own LAN owns those
  ranges. Per-route opt-in would need extra `includedRoutes`/`addRoute` and
  a classification rule.
- **Exit nodes:** not supported in this mode. It would mean routing
  0.0.0.0/0 to one tailnet, which is a different product decision.
- **MagicDNS short names / search domains:** single-label names are never
  owned (desktop rule). iOS `searchDomains` and Android search domains are
  not set.
- **IPv6-only networks / NAT64:** the routes and synthetic addresses work on
  any underlying network, because traffic is tunnelled. Answers are not
  synthesised across families (A for v4 peers, AAAA for v6). A v4-only
  tailnet device on a v6-only app path is not bridged.
- **MTU / PMTU:** 1280 end to end, which matches Tailscale's tun.
  Non-first IP fragments are dropped (with MTU 1280 apps should not send
  any). "Packet too big" from inside the tailnet is translated.
- **Inbound connections** from tailnets to the phone are dropped (there are
  no listeners in this mode).
- **Ownership can change mid-life:** a real address handed out while unique
  becomes refused for new flows once a second tailnet learns it. Existing
  flows continue. TTL is capped at 60 s to bound this.
- **Android Private DNS "strict"** sends DNS over TLS straight to the chosen
  host, bypassing our server, so tailnet names fail. This is the same as
  the Tailscale app. The UI should say so.
- **Android lockdown ("block connections without VPN")** may block traffic
  outside our split routes; needs testing on a device.
- A carrier that numbers the phone itself in 100.64.0.0/10 loses reach to
  its carrier-internal 100.x hosts while the mode is on. Tailscale has the
  same trade-off. The option is to route only learned /32s, at the cost of a
  settings re-apply each time ownership changes.

## App Review risks

- The utun fd scan uses public `getsockopt`. wireguard-apple and Tailscale
  ship it. If it ever broke, `MemDevice` plus packetFlow is the fallback.
- Claiming 100.64.0.0/10 and a 198.18.0.0/15 route is a split tunnel with
  no default route, so this is not a full-device VPN. The description should
  say Tailfuse routes only tailnet traffic. The Tailscale app's precedent
  applies.
- A DNS server with `matchDomains` only sees tailnet names. Never ship
  `matchDomains = [""]`, and never set DNS settings with an empty list
  (both would capture all DNS).
- Guideline 5.4 (VPN apps): a NEVPN-based app from an organization
  enrolled as such. Nothing here changes that; there is no data collection
  on the packet path.

## Wiring (phase 2, as built)

- **Engine** (`../allapps.go`): all-apps mode is a saved setting, on by default
  (`config.json` `proxy_only` is its inverse), applied only where the shell can hand over the tun
  (`Options.Packets`, set by `tfmobile`; tfdev and the desktop-parity tests stay in proxy mode).
  At `Start` the engine makes the Mux (pool in `<state>/synthetic-addresses.json`), and each node
  starts with `NodeOptions.Tun = mux.Link(name)`. `pollOnce` calls `SetAddrs` from each running
  node's status. `remove_profile` calls `Pool.Forget`. Passthrough is held off while packets run.
- **Adapters:** `Rules` is `RouteTable.Classify` (the verdicts map one to one). `TailnetDNS` is the
  node's `local.Client.QueryDNS` (`DNSQuerier`), and fails when the node isn't Running. A change
  from phase 1: for a domain of the person's own (`RouteTable.PublicFallback`: a configured suffix
  outside `ts.net`, or a remembered host), a missing answer falls back to ordinary DNS, as the proxy
  path does. On Android that goes to `SetDNSServers` (the system resolver would loop into our own
  VPN DNS). On iOS it uses the system resolver, with a guard that fails a lookup coming back to the
  responder instead of recursing. MagicDNS names and served domains never fall back.
- **Mode switch** (`set_all_apps`): the engine restarts the nodes with or without their link and
  fires `domains`. The shells re-apply on `domains` (iOS: new settings, then
  `StartPackets`/`StopPackets`; Android: re-`establish()`). No VPN restart is needed.
- **Fallback:** `StartPackets` failing (no utun fd, bad fd) or the tun ending sets
  `all_apps_error`. That changes the shells' mode key, so `domains` fires and the shells apply
  proxy settings for the session. The error clears at `Start` or `set_all_apps`.
- **State:** `all_apps`, `all_apps_active`, `all_apps_error`, `tunnel` (addresses, routes, DNS,
  MTU), so neither shell hard-codes the ranges.
- **gomobile:** `StartPackets(fd, utun)`, `StopPackets`, `SetDNSServers`,
  `Protector`/`SetProtector` (Android: `netns.SetAndroidProtectFunc` plus protected upstream DNS
  sockets, which may then use a carrier resolver inside 100.64.0.0/10).

## Files

`rules.go` interfaces · `pool.go` synthetic pool · `mux.go` packet path ·
`nodetun.go` per-tailnet `tun.Device` · `flows.go` flow table · `craft.go`
RST/ICMP/UDP builders · `dns.go` responder + cache · `dnstcp.go` DNS over
TCP · `upstream.go` Android resolvers · `device.go` fd and in-memory devices ·
`utun_darwin.go` fd discovery. Tests: `*_test.go` (fakes, gVisor stacks
standing in for the phone kernel and the peers), `memory_test.go` (harness
+ benchmark), `testdata/integration/` (real nodes, run via `run.sh`).
