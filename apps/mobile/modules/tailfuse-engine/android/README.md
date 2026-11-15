# tailfuse-engine (Android)

UNCOMPILED: written without a JDK or Android SDK. Expect small compile fixes on first build.

- `libs/tfmobile.aar` is the Go engine (gitignored): `engines/mobile/scripts/build-engine.sh android`.
  `build.gradle` fails with that instruction if it is missing. The module only compiles against the
  aar (`compileOnly`); `plugins/withTailfuseAndroid.js` packages it in the app and sets minSdk 26.
- `TailfuseVpnService` runs the engine in the app process. It is a foreground service (type
  `systemExempted`, allowed for VPN apps) with a quiet notification, and a VPN that claims NO routes:
  one /32 address (192.0.2.1, TEST-NET-1) and nothing else. It sets the system HTTP proxy to the
  engine's router (`Builder.setHttpProxy`, API 29+), pushes the interface list to the engine at start
  and on every default-network change, and drains the tun fd so it cannot block.
- PAC proxies are not supported over VPNs, so the proxy is a direct 127.0.0.1:43100 for ALL http(s)
  traffic. The service therefore calls `Tfmobile.setPassthrough(true)` before `Tfmobile.start`: names
  no tailnet owns are connected directly by the engine, while tailnet names, names two tailnets
  claim, Tailscale address space and paused tailnets' names keep the strict rules (see
  `engines/mobile/API.md`, "Passthrough"). Private LAN addresses pass through; this device's
  loopback does not, except Tailfuse's own local ports. `APPLY_SYSTEM_PROXY` in the service turns
  the proxy off.
- No `addDisallowedApplication(packageName)`: with no routes the engine's sockets already use the
  underlying network, and excluding the app would take the proxy away from its own WebView/fetch.
- `TailfuseEngineModule` is the JS surface: `start` (VPN consent dialog, service start, waits up to 30 s
  for the engine), `stop`, `isUp`, `call`, `getAlwaysOn`/`setAlwaysOn` (opens VPN settings), `change`.
- Restarts: START_STICKY re-runs the engine after process death; `onRevoke` stops everything.

Verify on a device:
1. First `start` shows the VPN dialog; declining rejects with `vpn permission was declined`.
2. Notification text, "Turn off", tap opens the app; Android 13+ notification prompt does not block start.
3. System proxy really applies to Chrome/WebView/OkHttp apps with a route-less VPN, and IPv6/v4 browsing still works.
4. Tailnet hostnames open in Chrome; ordinary sites (http and https), WebSockets and long downloads
   still work through passthrough; a paused tailnet's names fail instead of reaching the internet.
5. Always-on from system settings starts the service; `getAlwaysOn` is true; another VPN app revokes us cleanly.
6. Swipe the app away and kill the process: service restarts, engine returns.
7. Wi-Fi to mobile switch reaches `setInterfaces`; `NetworkInterface` output is non-empty on API 30+.
8. Release build (R8): `consumer-rules.pro` keeps `go.**` and `tfmobile.**`.
