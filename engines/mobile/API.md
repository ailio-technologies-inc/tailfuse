# Tailfuse mobile engine: the contract

The engine is a Go library. It runs one embedded Tailscale node (`tsnet`) per tailnet and a
loopback router that sends each hostname to the tailnet that owns it, under the same rules as the
desktop core (`crates/tailfuse-core`): each suffix has exactly one owner, the longest suffix wins,
and a name no tailnet owns is refused, never routed elsewhere.

Three things talk to it, and all three use this file as the contract:

- the native shells (the iOS packet tunnel in Swift, the Android `VpnService` in Kotlin), through
  the gomobile package `tfmobile`;
- the Expo app (TypeScript), through the native module `TailfuseEngine`, which passes JSON through;
- `cmd/tfdev`, a command-line harness for a computer.

## Where it runs

| Platform | Process that runs the nodes | When Tailfuse is off |
|---|---|---|
| iOS | The packet tunnel extension. The app sends it requests with `sendProviderMessage`. | The app links the same framework and calls `Call` itself; only the commands marked *works off* succeed. |
| Android | The app's own process, kept alive by the `VpnService`. | The same process; `Call` without `Start`. |

Only one process ever writes the state folder at a time: the extension while the tunnel is up,
the app while it is down.

## gomobile surface (`tfmobile`)

```go
// Start loads the config from stateDir (created if missing), starts the router on
// 127.0.0.1:43100 and brings up every tailnet that is not paused. Safe to call twice.
func Start(stateDir string) error

// Stop shuts every node and the router down. Safe to call when not started.
func Stop()

// Running reports whether Start has succeeded and Stop has not been called since.
func Running() bool

// Call runs one request and returns one response, both JSON (below). It never panics and never
// returns an empty string. Before Start, stateDir must have been given with SetStateDir.
func Call(request string) string

// SetStateDir tells Call where the config lives when the engine is not started.
func SetStateDir(stateDir string)

// SetListener registers the one listener for change events (nil clears it).
func SetListener(l Listener)

type Listener interface {
    // OnChange is called from a background goroutine, coalesced to at most a few times a second.
    // kind is "state" (anything the state request reports has changed) or "domains" (the set of
    // names in match_domains has changed; the shell must re-apply its network settings).
    OnChange(kind string)
}

// SetDeviceName gives the engine the name this device should take on each tailnet (the phone's
// own name, e.g. "Ada's iPhone"; the engine turns it into a valid hostname). Call before Start.
// Without it the engine falls back to "tailfuse-<os>".
func SetDeviceName(name string)

// SetInterfaces gives the engine the device's network interfaces. Android only, where Go cannot
// list them itself; same text format as tailscale-android's getInterfacesAsString.
func SetInterfaces(text string)

// SetPassthrough lets names no tailnet owns be connected directly from the device instead of
// refused (see "Passthrough" below). Android only; iOS must never turn it on. Callable before
// Start; off by default.
func SetPassthrough(on bool)
```

Generated names: Swift/Objective-C `TfmobileStart`, `TfmobileCall`, … and the protocol
`TfmobileListener`; Kotlin/Java `tfmobile.Tfmobile.start(...)`, `tfmobile.Listener` (gomobile's
default package, no `-javapkg`).

## Requests and responses

Request: `{"cmd": "<name>", "args": { ... }}` (`args` may be omitted).
Response: `{"ok": true, "result": <value>}` or `{"ok": false, "error": "<lower-case message>"}`.

| cmd | args | result | works off |
|---|---|---|---|
| `state` | none | `State` | yes |
| `add_profile` | `{"auth_key"?: string}` | `{"name": string}`: a new tailnet whose sign-in has started; its `auth_url` appears in `state` within a few seconds. Fails with `that needs the unlimited upgrade` when `free_profiles` tailnets exist and `unlocked` is false | no |
| `login` | `{"name": string}` | `null`; starts a fresh sign-in for an existing tailnet, `auth_url` appears in `state` | no |
| `set_connected` | `{"name": string, "on": bool}` | `null`; off sets `paused` and stops that node, on clears it and starts it | yes (saved; takes effect at Start) |
| `remove_profile` | `{"name": string}` | `null`; stops the node, deletes its keys and its local ports | yes |
| `test_host` | `{"host": string}` (a hostname or a URL) | `{"host": string, "owner": string or null}` | yes (suffix rules only) |
| `add_tunnel` | `{"name", "listen", "target", "profile"}` (all strings) | `null` | yes |
| `remove_tunnel` | `{"name": string}` | `null` | yes |
| `set_unlocked` | `{"on": bool}` | `null`; saved in the config. The app sets it from the store's record of the in-app purchase | yes |
| `devices` | `{"name": string}` | `[{"name", "dns_name", "ips": [string], "os", "online": bool}]` | no |

A command that needs the nodes while Tailfuse is off fails with the error `tailfuse is off`.

### State

```jsonc
{
  "up": true,                      // the engine is started (false from an app-side Call on iOS)
  "version": "0.1.26",             // engine version
  "proxy": "127.0.0.1:43100",      // the router: HTTP proxy (CONNECT and plain) and SOCKS5 on one port
  "pac_url": "http://127.0.0.1:43100/tailfuse.pac",
  "unlocked": false,               // the unlimited-tailnets purchase is recorded
  "free_profiles": 2,              // tailnets allowed without it
  "tailscale_logs": false,         // Tailscale's log upload is on in this process (see below)
  "passthrough": false,            // unowned names are connected directly (Android, see below)
  "match_domains": ["home-lab.ts.net", "work.ts.net"],  // every name the router owns, no leading dot
  "profiles": [
    {
      "name": "work",              // unique, stable id; chosen by the engine at sign-in
      "account": "ada@example.com",// who signed in; "" until known
      "tailnet": "example.com",    // the tailnet's display name; "" until known
      "suffixes": [".work.ts.net"],// owned suffixes, leading dot
      "hostname": "adas-iphone",   // this device's name on that tailnet; "" until known
      "ips": ["100.64.0.7", "fd7a:115c:a1e0::7"],
      "online": true,              // the node is running and signed in
      "paused": false,             // switched off on purpose
      "error": "",                 // a problem to show beside the tailnet, "" when none
      "auth_url": ""               // a sign-in page waiting to be opened, "" when none
    }
  ],
  "tunnels": [
    {"name": "prod-db", "listen": "127.0.0.1:5432", "target": "db.work.ts.net:5432", "profile": "work"}
  ]
}
```

A tailnet that is neither `online`, `paused` nor in `error` is waiting for a sign-in.

## The free limit

Two tailnets are free, as on desktop. More need the one-time in-app purchase (product id
`com.ailio.tailfuse.unlimited`, non-consumable). The store is the source of truth: the app asks
it for the purchase at launch and after a purchase or restore, and passes the answer to
`set_unlocked`. The engine only keeps the flag and refuses a third `add_profile` without it.
Tailnets that already exist keep working if the flag is cleared; only adding is refused.
Every listed tailnet counts, including one whose sign-in has not finished. The limit is only
checked by `add_profile`: a sign-in that began under it always finishes, even if the flag was
cleared meanwhile or the tailnet turns out to be one Tailfuse already has.
Desktop license keys are not accepted on mobile (the stores do not allow it).

## Signing in to a new tailnet

1. The shell turns Tailfuse on (the VPN permission prompt happens here the first time).
2. `add_profile` returns a provisional name at once.
3. Poll `state`; when that profile has an `auth_url`, open it in a browser.
4. The person signs in. The engine learns the tailnet's MagicDNS suffix, account and name, renames
   the provisional profile after the tailnet (the same naming rule as the desktop app), saves it,
   and fires `domains`.
5. If the tailnet is one Tailfuse already has, the provisional profile is dropped and the existing
   one is refreshed; `state` then simply no longer lists the provisional name.
6. If nobody finishes within 10 minutes the provisional profile is removed.

## The shells' network settings

- **iOS:** a packet tunnel that claims no routes, with HTTP and HTTPS proxy settings pointing at
  `proxy` and `matchDomains` set to `match_domains`. Re-apply on every `domains` event.
- **Android:** a `VpnService` that claims no routes, with `setHttpProxy` set to `proxy`
  (host:port). Android does not support PAC proxies over a VPN, so every app's HTTP(S) traffic
  reaches the router: the shell calls `SetPassthrough(true)` before `Start`.

Because nothing is routed into the tunnel, the nodes' own traffic cannot loop back into it.

## Passthrough

Off by default, and then the desktop rule holds: a name no tailnet owns is refused. Android turns
it on because its VPN can only hand apps one plain HTTP proxy for everything, so ordinary browsing
would otherwise break. **iOS must never turn it on:** there `matchDomains` already keeps every
other name away from the router, and passthrough would only widen what reaches it.

With passthrough on, a name **no tailnet has anything to do with** is connected directly from the
device (a plain socket, never through a node), for CONNECT, plain HTTP and SOCKS5. Everything else
keeps the strict rules:

| Destination | With passthrough |
|---|---|
| a name or address a tailnet owns (suffix, learned domain, remembered host, device address) | that tailnet, exactly as before; if it is paused, offline or not signed in the connection fails (502 / SOCKS `0x04`) and the name is never looked up publicly |
| a domain an off tailnet served when it last ran (remembered in `learned-domains.json`) | refused (403 / `0x02`), never looked up publicly |
| a name or address two tailnets claim | refused |
| Tailscale address space (100.64.0.0/10, fd7a:115c:a1e0::/48) no running tailnet has, also when a name resolves into it (even partly) | refused |
| this device: loopback, 0.0.0.0, `::` | refused, unless the address is one of Tailfuse's local ports; the router's own address never |
| multicast and broadcast | refused |
| private LAN (10/8, 172.16/12, 192.168/16, fc00::/7) and link-local | **allowed**: the phone's browser reaches these through its normal network anyway, and the router only listens on loopback, so it gives no app anything it could not already reach |
| anything else | direct |

Conflicts and off tailnets' domains are checked before any public lookup. A direct name is
resolved once and the checked addresses are the ones dialed (no second lookup to rebind).

The plain-HTTP path is a forward proxy for any site: hop-by-hop headers (and those named in
`Connection`) are dropped, bodies stream both ways through pooled 16 KiB buffers (nothing is held
whole), the router frames bodies itself (`Content-Length`, or re-chunked when the upstream gave no
length) so the client connection survives an upstream that closes, `Expect: 100-continue` is
answered at once, upgrades (WebSocket over `http://`) are handed over as raw streams, a kept-alive
upstream connection the server already closed is redialed once for requests without a body, and
the only timeouts are 5 minutes for an upstream to start answering and 5 minutes without progress
in a transfer (a long download that keeps moving is never cut). Trailers are dropped.

## Tailscale's logging

Desktop Tailfuse sends no telemetry, and neither does the engine: every node starts with
Tailscale's process-wide no-logs switch (`TS_NO_LOGS_NO_SUPPORT`) on, so tsnet's log uploader is a
no-op and the control plane is told this device opted out. Some tailnets refuse such a node (its
health reports "requires logging"). As on desktop, a tailnet that says so *while it is being added*
gets `tailscale_logs` saved and is connected with logging allowed. Unlike desktop, where each
tailnet is its own `tailscaled`, the switch cannot be set per tailnet in one process: while any
running tailnet needs logging, every node in the process runs with logging allowed, and `state`
reports `"tailscale_logs": true` so the app can say so. The engine restarts all nodes when this
changes (for example when that tailnet is removed or switched off). An existing tailnet that starts
requiring logging gets an `error` asking to remove it and add it again; logging is never turned on
without a sign-in.

## Engine details (as built)

- **Provisional names** are `new-tailnet`, `new-tailnet-2`, … Profile names are 1 to 40 of
  `a-z`, `0-9`, `-` (they are folder names under `<stateDir>/nodes/`).
- **Auth keys** given to `add_profile` are kept in `config.json` (0600) only until that tailnet
  has signed in, then cleared.
- **The PAC** (`pac_url`) returns `PROXY 127.0.0.1:43100; SOCKS5 127.0.0.1:43100` for owned names
  and `DIRECT` for the rest (desktop returns `SOCKS5` only; phones are given HTTP proxies). It is
  only served to requests whose `Host` is loopback, so a web page that rebinds its own name to
  127.0.0.1 cannot read it.
- **Refusals:** SOCKS5 reply `0x02` (not allowed); HTTP `403` with
  `no tailfuse profile owns hostname <host>`. An owned name whose tailnet is off or not signed in
  gets SOCKS5 `0x04` / HTTP `502`; it is never sent anywhere else.
- **Local ports:** `listen` may be `PORT`, `:PORT` or `127.0.0.1:PORT` and must be loopback;
  `target` must be `host:port`; the profile must have finished signing in. While Tailfuse is on, a
  port that cannot be bound fails `add_tunnel` and is not saved.
- **Names a tailnet serves** are learned every 30 seconds while it is online (split-DNS domains,
  search domains, its devices' addresses), with the desktop rules: claimed by exactly one tailnet,
  `ts.net` and single labels ignored; names that resolve to one tailnet's device are remembered in
  `learned-hosts.json` and re-checked each round. All of these appear in `match_domains`.
- **Names are resolved by the tailnet's own DNS first** (MagicDNS and split DNS through the node's
  100.100.100.100), then by the device's resolver.
- **Device name:** `SetDeviceName` lower-cases the name, drops apostrophes and letters outside
  `a-z`, turns every other run of characters into one `-` and keeps at most 63 characters
  ("Ada's iPhone" → `adas-iphone`). A name with nothing left is ignored. A `hostname` saved in
  `config.json` still wins.
- **iOS memory:** on iOS the engine sets a Go soft memory limit of 32 MiB and GC percent 50.
- **tfdev** takes `-state DIR`, `-listen ADDR` (use another port when desktop Tailfuse is
  running on 43100), `-v`, `-off`, and `-ioslimits`; besides JSON lines it accepts `start` and `stop`.

