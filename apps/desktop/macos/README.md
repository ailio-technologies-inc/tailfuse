# The Mac tunnel: all-apps mode

Settings > **Every app** makes Tailfuse on the Mac work the way the Tailscale app does, for every
tailnet at once: ssh, database tools, Node, curl and browsers reach each tailnet by name and by
address, with no proxy settings.

Off (the default), the app runs one userspace `tailscaled` per tailnet behind its router and the
system PAC, so only proxy-aware programs reach the tailnets. On, those stand down and a
NetworkExtension packet tunnel runs the phones' Go engine instead (`engines/mobile`, design in
`engines/mobile/packets/DESIGN.md`). It claims `100.64.0.0/10`, `fd7a:115c:a1e0::/48` and
Tailfuse's own `198.18.0.0/15`, and answers the tailnets' names from a DNS server inside the tunnel
(split DNS, the names each tailnet owns only).

| Piece | Where |
| --- | --- |
| The system extension (`com.ailio.tailfuse.tunnel`) | `TailfuseTunnel/`: `PacketTunnelProvider.swift`, ported from the iPhone's `apps/mobile/targets/tunnel` |
| Installing, starting and messaging it, from inside the app | `TunnelControl/TunnelControl.swift`, a static library `src-tauri/build.rs` compiles and links |
| Its Rust face | `src-tauri/src/tunnel.rs` |
| The mode: the switch, the hand-over, the window's state | `src-tauri/src/allapps.rs` |
| Building it into Tailfuse.app and signing | `build-tunnel.sh` |

Turning it on carries the setup over with the link a phone scans (`sync::link`, engine
`import_setup`): every tailnet, its domains and its local ports. Keys never travel. The tunnel is
its own device on each tailnet, so each one signs in once more, in the browser.

## Building

```sh
engines/mobile/scripts/build-engine.sh macos      # the engine framework (git-ignored)
cd apps/desktop && npm run tauri build -- --bundles app
TF_APP_PROFILE=~/…/Tailfuse_Developer_ID.provisionprofile \
TF_TUNNEL_PROFILE=~/…/Tailfuse_Tunnel_Developer_ID.provisionprofile \
  macos/build-tunnel.sh src-tauri/target/release/bundle/macos/Tailfuse.app --notarize
```

macOS loads the extension only when Tailfuse.app is in `/Applications`, signed with the profiles,
and notarized. The first time, it asks the person to allow the extension (System Settings > Login
Items & Extensions) and to allow a VPN configuration.

## What the Apple account needs

- `com.ailio.tailfuse`: Network Extensions (on) and **System Extension** (to turn on).
- `com.ailio.tailfuse.tunnel`: Network Extensions (on).
- A **Developer ID** provisioning profile for each, with the Developer ID Application certificate.

The App Store Connect key on the owner's laptop can read the account but not change it (403), so
these are made by hand at developer.apple.com, or with a key that has the Admin role.

## In a release

Releases ship the tunnel. The release build on the Mac mini (infra `tailfuse-release-macos`) runs
`build-engine.sh macos` and `build-tunnel.sh --assemble-only`, which puts the extension in the
bundle unsigned. The cluster's `tailfuse-notarize` (rcodesign, on Linux) then embeds the two
Developer ID profiles from its secret store (infra `bake-host/signing-secrets.json`), signs the
extension with `TailfuseTunnel/TailfuseTunnel.entitlements` and the app with
`Tailfuse.entitlements`, and notarizes and staples. Both profiles expire in 2044. A build
without the extension hides the switch (`allapps::available`).

## The window in all-apps mode

The window, the tray and the menu-bar icon read the tunnel's engine. Signing in, switching a
tailnet on or off and signing in to a new tailnet go to it directly; adding or removing a tailnet,
a domain or a local port, and a tailnet's exit node or subnet routes, are saved to the app's
config as before and sent to the tunnel at once (`allapps::plan`, one request per difference), so
switching all-apps mode off later finds the same setup.

## Open work

- A tailnet first signed in from the tunnel exists only in the tunnel's engine; switching all-apps
  mode off does not bring it to the app's own nodes.
- Windows and Linux: not started (a WinTun / TUN equivalent of the same engine).
