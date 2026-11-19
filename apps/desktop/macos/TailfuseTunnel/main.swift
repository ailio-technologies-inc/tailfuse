// A system extension's entry point: hand the process to NetworkExtension, which creates
// PacketTunnelProvider (Info.plist NEProviderClasses) when the tunnel starts.
import Foundation
import NetworkExtension

autoreleasepool {
  NEProvider.startSystemExtensionMode()
}
dispatchMain()
