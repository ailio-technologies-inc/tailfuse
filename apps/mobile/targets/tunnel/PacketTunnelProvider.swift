// The Tailfuse packet tunnel. It runs the engine (one Tailscale node per tailnet and the router on
// 127.0.0.1:43100) and claims no routes: the only thing it gives iOS is an HTTP/HTTPS proxy scoped
// with matchDomains to the names the tailnets own. See engines/mobile/API.md and
// apps/mobile/modules/tailfuse-engine/ios/README.md.

import Foundation
import NetworkExtension
import Tfmobile
import os

private let appGroup = "group.com.ailio.tailfuse"
private let log = OSLog(subsystem: "com.ailio.tailfuse.tunnel", category: "tunnel")

/// The router's address when `state` cannot be read; the engine's DefaultListen.
private let fallbackProxy = ("127.0.0.1", 43100)

/// A link-local-free, never-routed address for the utun interface. 198.18.0.0/15 is reserved for
/// benchmarking (RFC 2544), so it is never a real destination, never a tailnet address (those are
/// 100.64.0.0/10 and fd7a:115c:a1e0::/48) and never a LAN. With a /32 mask iOS adds no route but
/// the host route to this one address, so nothing on the device is sent into the tunnel.
private let tunnelAddress = "198.18.0.1"

final class PacketTunnelProvider: NEPacketTunnelProvider {
  /// Serialises network settings: setTunnelNetworkSettings calls must not overlap.
  private let queue = DispatchQueue(label: "com.ailio.tailfuse.tunnel.settings")
  // Guarded by queue.
  private var live = false  // settings may be applied (after start, before stop)
  private var applying = false  // a setTunnelNetworkSettings call is in flight
  private var again = false  // domains changed while one was in flight
  private var listener: TunnelListener?

  override func startTunnel(options: [String: NSObject]?, completionHandler: @escaping (Error?) -> Void) {
    logMemory("start")
    guard let container = FileManager.default.containerURL(forSecurityApplicationGroupIdentifier: appGroup) else {
      completionHandler(TunnelError("the app group container \(appGroup) is missing"))
      return
    }
    let stateDir = container.appendingPathComponent("tailfuse", isDirectory: true).path
    // The app writes the phone's name here before it starts the tunnel (UIDevice is not used in
    // the extension, to keep UIKit out of a 50 MB process).
    if let name = UserDefaults(suiteName: appGroup)?.string(forKey: "deviceName"), !name.isEmpty {
      TfmobileSetDeviceName(name)
    }

    queue.async { [self] in
      var error: NSError?
      if !TfmobileStart(stateDir, &error) {
        os_log("engine start failed: %{public}@", log: log, type: .error, error?.localizedDescription ?? "unknown")
        completionHandler(error ?? TunnelError("the engine did not start"))
        return
      }
      let listener = TunnelListener { [weak self] kind in
        if kind == "domains" { self?.domainsChanged() }
      }
      self.listener = listener  // Go holds a reference too; keep ours so it outlives every callback
      TfmobileSetListener(listener)
      self.live = true
      self.applySettings { error in
        if let error {
          os_log("settings failed: %{public}@", log: log, type: .error, error.localizedDescription)
          self.live = false
          TfmobileSetListener(nil)
          TfmobileStop()
        } else {
          self.drainPackets()
        }
        completionHandler(error)
      }
    }
  }

  override func stopTunnel(with reason: NEProviderStopReason, completionHandler: @escaping () -> Void) {
    os_log("stop, reason %d", log: log, type: .info, reason.rawValue)
    queue.async {
      self.live = false
      self.again = false
      TfmobileSetListener(nil)
      self.listener = nil
      // Stop waits for every node to close; keep it off the provider's thread.
      DispatchQueue.global(qos: .userInitiated).async {
        TfmobileStop()
        completionHandler()
      }
    }
  }

  /// One JSON request from the app (sendProviderMessage), one JSON response. A slow command (a
  /// node starting, `devices`) must never hold up the provider, so it runs on a global queue; the
  /// engine is safe for concurrent calls.
  override func handleAppMessage(_ messageData: Data, completionHandler: ((Data?) -> Void)?) {
    let request = String(decoding: messageData, as: UTF8.self)
    DispatchQueue.global(qos: .userInitiated).async {
      let response = TfmobileCall(request)
      completionHandler?(Data(response.utf8))
    }
  }

  // sleep(completionHandler:) and wake() keep NEProvider's defaults (do nothing). The nodes'
  // connections survive a short sleep, and Tailscale's own link monitor rebinds and re-handshakes
  // when the network changes on wake; stopping the nodes for every screen lock would cost a
  // control-plane round trip per tailnet and break long-lived connections for nothing.

  // MARK: - Network settings

  private func domainsChanged() {
    queue.async {
      guard self.live else { return }
      self.logMemory("domains")
      if self.applying {
        self.again = true  // coalesced: one more apply after the one in flight, with the latest names
      } else {
        self.applySettings(nil)
      }
    }
  }

  /// Applies settings built from the engine's current state. Call on queue.
  private func applySettings(_ done: ((Error?) -> Void)?) {
    applying = true
    let settings = makeSettings()
    setTunnelNetworkSettings(settings) { error in
      self.queue.async {
        self.applying = false
        done?(error)
        if self.again && self.live {
          self.again = false
          self.applySettings(nil)
        }
      }
    }
  }

  private func makeSettings() -> NEPacketTunnelNetworkSettings {
    // The remote address is only shown in Settings > VPN and excluded from the tunnel's routes;
    // the "server" really is the router on loopback.
    let settings = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: "127.0.0.1")
    let ipv4 = NEIPv4Settings(addresses: [tunnelAddress], subnetMasks: ["255.255.255.255"])
    ipv4.includedRoutes = []  // claim nothing: not the default route, no split routes
    settings.ipv4Settings = ipv4

    let state = EngineState.read()
    // [""] would match every name and nil means "every connection": with no owned names there is
    // no proxy at all, so a tunnel with no tailnets yet never touches anyone's traffic.
    let domains = state.domains.filter { !$0.isEmpty }
    os_log("applying settings: %d match domains", log: log, type: .info, domains.count)
    if !domains.isEmpty {
      let proxy = NEProxySettings()
      let server = NEProxyServer(address: state.proxyHost, port: state.proxyPort)
      proxy.httpEnabled = true
      proxy.httpServer = server
      proxy.httpsEnabled = true
      proxy.httpsServer = server
      proxy.autoProxyConfigurationEnabled = false
      proxy.matchDomains = domains
      // The engine never owns a single-label name; say so to iOS as well.
      proxy.excludeSimpleHostnames = true
      settings.proxySettings = proxy
    }
    return settings
  }

  /// Nothing is routed into the tunnel, so packetFlow should stay empty. Reading anyway costs
  /// nothing while it is empty and drops anything that does arrive (e.g. a packet sent to the
  /// tunnel's own address) instead of leaving it queued in the kernel.
  private func drainPackets() {
    packetFlow.readPackets { [weak self] _, _ in
      self?.drainPackets()
    }
  }

  // MARK: - Memory

  /// The extension is killed above 50 MB. Logs what is left before that and the footprint jetsam
  /// counts, so a run on a device can be read in Console (subsystem com.ailio.tailfuse.tunnel).
  private func logMemory(_ when: String) {
    var info = task_vm_info_data_t()
    var count = mach_msg_type_number_t(MemoryLayout<task_vm_info_data_t>.size / MemoryLayout<natural_t>.size)
    let kr = withUnsafeMutablePointer(to: &info) {
      $0.withMemoryRebound(to: integer_t.self, capacity: Int(count)) {
        task_info(mach_task_self_, task_flavor_t(TASK_VM_INFO), $0, &count)
      }
    }
    let footprint = kr == KERN_SUCCESS ? info.phys_footprint : 0
    os_log(
      "memory at %{public}@: footprint %llu KiB, available %llu KiB", log: log, type: .info,
      when, footprint / 1024, UInt64(os_proc_available_memory()) / 1024)
  }
}

/// Receives the engine's change events on a Go goroutine.
private final class TunnelListener: NSObject, TfmobileListenerProtocol {
  private let handler: (String) -> Void
  init(_ handler: @escaping (String) -> Void) { self.handler = handler }
  func onChange(_ kind: String?) { handler(kind ?? "") }
}

/// The part of the engine's `state` the tunnel needs.
private struct EngineState {
  var proxyHost = fallbackProxy.0
  var proxyPort = fallbackProxy.1
  var domains: [String] = []

  static func read() -> EngineState {
    var out = EngineState()
    let response = TfmobileCall(#"{"cmd":"state"}"#)
    guard
      let object = try? JSONSerialization.jsonObject(with: Data(response.utf8)) as? [String: Any],
      object["ok"] as? Bool == true,
      let result = object["result"] as? [String: Any]
    else {
      os_log("state unreadable: %{public}@", log: log, type: .error, String(response.prefix(200)))
      return out
    }
    out.domains = result["match_domains"] as? [String] ?? []
    if let proxy = result["proxy"] as? String, let colon = proxy.lastIndex(of: ":"),
      let port = Int(proxy[proxy.index(after: colon)...])
    {
      out.proxyHost = String(proxy[..<colon]).trimmingCharacters(in: CharacterSet(charactersIn: "[]"))
      out.proxyPort = port
    }
    return out
  }
}

private struct TunnelError: LocalizedError {
  let message: String
  init(_ message: String) { self.message = message }
  var errorDescription: String? { message }
}
