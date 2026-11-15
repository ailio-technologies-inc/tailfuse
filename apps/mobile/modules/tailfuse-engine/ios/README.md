# Tailfuse on iOS: how the native pieces fit

- **Engine**: `Frameworks/Tfmobile.xcframework`, a static gomobile framework built by
  `engines/mobile/scripts/build-engine.sh ios` (not checked in; both podspecs fail with that hint if it's missing).
- **Packet tunnel** (`app/targets/tunnel`, made into an Xcode target by `@bacons/apple-targets`):
  runs the engine with the state folder at `<group.com.ailio.tailfuse>/tailfuse`. It claims no routes.
  Its only effect on the phone is an HTTP/HTTPS proxy (`127.0.0.1:43100`), scoped by `matchDomains` to the engine's
  `match_domains`, re-applied (coalesced) on every `domains` event. With no owned names it sets no proxy.
- **App module** (`TailfuseEngineModule.swift`): owns the one `NETunnelProviderManager`. It starts and
  stops the tunnel and keeps always-on as a preference in the app group. It routes `call` through
  `sendProviderMessage` while the tunnel is connected, and in-process (`TfmobileCall`, never `Start`) while it is not.
- **Linking**: the app gets the framework from `TailfuseEngine.podspec` (Expo autolinking). The
  extension gets it from `Frameworks/TailfuseTfmobile.podspec` through `targets/tunnel/pods.rb`. It sits one folder
  down so that autolinking doesn't link it into the app. Each process has its own Go runtime.

## Check on a device (the simulator can't run packet tunnels)

1. First start shows the VPN prompt. Declining rejects with `vpn permission was declined`.
2. Console (subsystem `com.ailio.tailfuse.tunnel`): footprint stays well under 50 MB with 2+ tailnets
   signed in. Watch the `memory at domains` lines.
3. Safari opens a tailnet name; other sites don't touch the proxy (check the router's log).
4. With no tailnets, nothing is proxied. A name that only shares a string suffix (`xwork.ts.net`) is
   not sent to the proxy.
5. Always-on: kill the app and toggle airplane mode. It comes back. Off in the app stays off.
6. Lock for 10+ minutes, then wake: tailnet names still load.
