// The Tailfuse packet tunnel extension, generated into the Xcode project by @bacons/apple-targets
// at `expo prebuild`. It runs the Go engine (linked through pods.rb) and claims no routes; see
// PacketTunnelProvider.swift and modules/tailfuse-engine/ios/README.md.

/** @type {import('@bacons/apple-targets/app.plugin').ConfigFunction} */
module.exports = () => ({
  type: "network-packet-tunnel",
  name: "tunnel",
  displayName: "Tailfuse",
  // com.ailio.tailfuse.tunnel: must match tunnelBundleId in TailfuseEngineModule.swift.
  bundleIdentifier: ".tunnel",
  // The app's own minimum (Expo SDK 58 needs iOS 16.4); the engine framework is built for 16.0.
  deploymentTarget: "16.4",
  frameworks: ["NetworkExtension"],
  entitlements: {
    "com.apple.developer.networking.networkextension": ["packet-tunnel-provider"],
    // Shared with the app: the engine's state folder lives in this group's container.
    "com.apple.security.application-groups": ["group.com.ailio.tailfuse"],
  },
});
