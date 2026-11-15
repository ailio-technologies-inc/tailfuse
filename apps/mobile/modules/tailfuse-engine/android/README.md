# tailfuse-engine (Android)

Compiles and packages into an arm64 debug APK (see apps/mobile/README.md, "Android build"). Never run on a device: the checklist at the bottom is still open.

- `libs/tfmobile.aar` is the Go engine (gitignored): `engines/mobile/scripts/build-engine.sh android`.
  `build.gradle` fails with that instruction if it is missing. The module only compiles against the
  aar (`compileOnly`); `plugins/withTailfuseAndroid.js` packages it in the app and sets minSdk 26.
- `TailfuseVpnService` runs the engine in the app process. It is a foreground service (type
  `systemExempted`, allowed for VPN apps) with a quiet notification, pushes the interface list to the
  engine at start and on every default-network change, and protects the engine's sockets from the
  VPN (`Tfmobile.setProtector` -> `VpnService.protect`). It runs in one of two modes, read from the
  engine's `state` (`engines/mobile/API.md`, "All-apps mode"):
  - **All apps** (default, `all_apps` true and `all_apps_error` empty): the VPN takes its addresses,
    routes (198.18.0.0/15, 100.64.0.0/10, fd5e:9b07:4a1c::/48, fd7a:115c:a1e0::/48), DNS servers and
    MTU 1280 from `state.tunnel`, and hands the fd to the engine with
    `Tfmobile.startPackets(pfd.dup().detachFd(), false)` (the engine owns and closes the duplicate; the service keeps and closes the original, so no path leaks the interface). No system proxy;
    the engine holds passthrough off while packets run. Names no tailnet owns are forwarded to the
    DNS servers of the networks outside the VPN, which the service pushes with
    `Tfmobile.setDNSServers` at start and on every change (a `NOT_VPN` network callback: the app's
    own default network is the VPN). This removes the "PAC not supported over VPN" problem.
  - **Proxy**: a VPN that claims NO routes (one /32, 192.0.2.1), the system HTTP proxy set to the
    engine's router (`Builder.setHttpProxy`, API 29+) and passthrough on, as before; the fd is
    drained so it cannot block. PAC proxies are not supported over VPNs, so the proxy is a direct
    127.0.0.1:43100 for ALL http(s) traffic; names no tailnet owns are connected directly by the
    engine, while tailnet names, names two tailnets claim, Tailscale address space and paused
    tailnets' names keep the strict rules ("Passthrough" in API.md). `APPLY_SYSTEM_PROXY` turns the
    proxy off.
  - A mode change (`set_all_apps`, or the engine falling back for the session when the packets cannot
    start) arrives as a `domains` event; the service re-establishes the VPN in the other mode.
- No `addDisallowedApplication(packageName)`: the app's own WebView should reach tailnets in either
  mode, and the engine's sockets are protected instead.
- `TailfuseEngineModule` is the JS surface: `start` (VPN consent dialog, service start, waits up to 30 s
  for the engine), `stop`, `isUp`, `call`, `getAlwaysOn`/`setAlwaysOn` (opens VPN settings), `change`.
- Restarts: START_STICKY re-runs the engine after process death; `onRevoke` stops everything.

Verify on a device:
1. First `start` shows the VPN dialog; declining rejects with `vpn permission was declined`.
2. Notification text, "Turn off", tap opens the app; Android 13+ notification prompt does not block start.
3. All apps (default): `state.all_apps_active` is true. Termux `ssh`/`curl` to a tailnet name and to a
   raw `100.x` address work; two tailnets sharing a `100.x` address each get their own `198.18.x.x`
   answer; a paused tailnet's names fail (SERVFAIL) and never reach public DNS.
4. All apps: ordinary sites and apps still resolve (forwarding to the underlying DNS servers): check on
   Wi-Fi, on mobile data, after switching between them, and on a carrier whose DNS is in 100.64.0.0/10.
   With Settings > Private DNS set to a hostname ("strict"), tailnet names are expected to fail.
5. All apps: Chrome uses no proxy (`chrome://net-internals/#proxy`); a custom domain added with
   `add_domain` opens.
6. Switch the Settings row to proxy mode: the VPN re-establishes without the app restarting; Chrome
   reaches tailnet names through the system proxy, ordinary sites through passthrough, and the old fd
   is closed (no "tailfuse-tun-drain" thread left over from all-apps mode). Switch back.
7. Proxy: IPv6 and IPv4 browsing still work with the route-less VPN; WebSockets and long downloads work
   through passthrough.
8. Always-on from system settings starts the service; `getAlwaysOn` is true; another VPN app revokes
   us cleanly. Try "Block connections without VPN" in all-apps mode: ordinary traffic must still flow.
9. Swipe the app away and kill the process: service restarts, engine returns in the saved mode.
10. Wi-Fi to mobile switch reaches `setInterfaces` and `setDNSServers`; `NetworkInterface` output is
    non-empty on API 30+.
11. Release build (R8): `consumer-rules.pro` keeps `go.**` and `tfmobile.**` (including `tfmobile.Protector`).
