// The Tailfuse packet tunnel. It runs the engine (one Tailscale node per tailnet and the router on
// 127.0.0.1:43100) in one of two modes, chosen by the engine's state (engines/mobile/API.md,
// "All-apps mode"):
//
// - all apps (the default): the tunnel claims Tailfuse's own 198.18.0.0/15 and ULA ranges and
//   Tailscale's ranges, a DNS server inside the tunnel answers the names the tailnets own
//   (matchDomains), and the engine reads the utun fd itself, so every app reaches every tailnet;
// - proxy: the tunnel claims no routes, and the only thing it gives iOS is an HTTP/HTTPS proxy
//   scoped with matchDomains to the names the tailnets own (browsers and other proxy-aware apps).
//
// A mode change (`set_all_apps`, or the engine falling back when the packets cannot start) arrives
// as a "domains" event; the settings are re-applied without restarting the tunnel.
// See also apps/mobile/modules/tailfuse-engine/ios/README.md.

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
/// 100.64.0.0/10 and fd7a:115c:a1e0::/48) and never a LAN. In proxy mode, with a /32 mask iOS adds
/// no route but the host route to this one address, so nothing on the device is sent into the
/// tunnel. All-apps mode uses the same address as the source of every app packet.
private let tunnelAddress = "198.18.0.1"

final class PacketTunnelProvider: NEPacketTunnelProvider {
  /// Serialises network settings: setTunnelNetworkSettings calls must not overlap.
  private let queue = DispatchQueue(label: "com.ailio.tailfuse.tunnel.settings")
  // Guarded by queue.
  private var live = false  // settings may be applied (after start, before stop)
  private var applying = false  // a setTunnelNetworkSettings call is in flight
  private var again = false  // domains changed while one was in flight
  private var listener: TunnelListener?
  private var mode = "starting"  // "all apps" or "proxy": the settings last applied, for the logs
  /// Proxy mode reads packetFlow to drop strays. All-apps mode must not: the engine reads the utun
  /// fd itself, and a pending read would take packets from it. Read from the callback, so guarded
  /// by a lock rather than the queue.
  private let drainLock = NSLock()
  private var draining = false

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
          TfmobileStopPackets()
          TfmobileStop()
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
      self.setDraining(false)
      TfmobileSetListener(nil)
      self.listener = nil
      // Stop waits for every node to close; keep it off the provider's thread.
      DispatchQueue.global(qos: .userInitiated).async {
        TfmobileStopPackets()
        TfmobileStop()
        completionHandler()
      }
    }
  }

  /// One JSON request from the app (sendProviderMessage), one JSON response. A slow command (a
  /// node starting, `devices`, `set_all_apps` restarting the nodes) must never hold up the
  /// provider, so it runs on a global queue; the engine is safe for concurrent calls.
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

  /// "domains": the owned names, or the mode, changed. Re-apply.
  private func domainsChanged() {
    queue.async {
      guard self.live else { return }
      self.logMemory("domains")
      if self.applying {
        self.again = true  // coalesced: one more apply after the one in flight, with the latest state
      } else {
        self.applySettings(nil)
      }
    }
  }

  /// Applies settings built from the engine's current state, then starts or stops the packets to
  /// match. Call on queue.
  private func applySettings(_ done: ((Error?) -> Void)?) {
    applying = true
    let state = EngineState.read()
    let allApps = state.allAppsMode
    if !allApps && state.packetsActive {
      // Leaving all-apps mode: let go of the utun fd before the routes go.
      TfmobileStopPackets()
    }
    if allApps {
      setDraining(false)
    }
    let settings = allApps ? makeAllAppsSettings(state) : makeProxySettings(state)
    setTunnelNetworkSettings(settings) { error in
      self.queue.async {
        self.applying = false
        if error == nil {
          self.mode = allApps ? "all apps" : "proxy"
          if allApps {
            if !state.packetsActive { self.startPackets() }
          } else if !self.isDraining() {
            self.setDraining(true)
            self.drainPackets()
          }
        }
        done?(error)
        if self.again && self.live {
          self.again = false
          self.applySettings(nil)
        }
      }
    }
  }

  /// Hands the utun fd to the engine (it finds the fd itself). On failure the engine records
  /// all_apps_error and fires "domains", and the next apply falls back to proxy settings.
  private func startPackets() {
    var error: NSError?
    if TfmobileStartPackets(-1, true, &error) {
      os_log("all apps: packets started", log: log, type: .info)
      logMemory("packets")
    } else {
      os_log(
        "all apps: packets did not start (%{public}@); falling back to proxy mode", log: log, type: .error,
        error?.localizedDescription ?? "unknown")
    }
  }

  private func makeProxySettings(_ state: EngineState) -> NEPacketTunnelNetworkSettings {
    // The remote address is only shown in Settings > VPN and excluded from the tunnel's routes;
    // the "server" really is the router on loopback.
    let settings = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: "127.0.0.1")
    let ipv4 = NEIPv4Settings(addresses: [tunnelAddress], subnetMasks: ["255.255.255.255"])
    ipv4.includedRoutes = []  // claim nothing: not the default route, no split routes
    settings.ipv4Settings = ipv4

    // [""] would match every name and nil means "every connection": with no owned names there is
    // no proxy at all, so a tunnel with no tailnets yet never touches anyone's traffic.
    let domains = state.domains.filter { !$0.isEmpty }
    os_log("applying proxy settings: %d match domains", log: log, type: .info, domains.count)
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

  private func makeAllAppsSettings(_ state: EngineState) -> NEPacketTunnelNetworkSettings {
    let t = state.tunnel
    let settings = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: "127.0.0.1")
    let v4 = NEIPv4Settings(addresses: t.v4Addresses, subnetMasks: t.v4Addresses.map { _ in "255.255.255.255" })
    v4.includedRoutes = t.v4Routes.map { NEIPv4Route(destinationAddress: $0.0, subnetMask: $0.1) }
    settings.ipv4Settings = v4
    let v6 = NEIPv6Settings(addresses: t.v6Addresses, networkPrefixLengths: t.v6Addresses.map { _ in 128 })
    v6.includedRoutes = t.v6Routes.map {
      NEIPv6Route(destinationAddress: $0.0, networkPrefixLength: NSNumber(value: $0.1))
    }
    settings.ipv6Settings = v6
    settings.mtu = NSNumber(value: t.mtu)
    // DNS settings only with names to match: without matchDomains (or with [""]) iOS would send
    // every query on the phone to the tunnel's DNS server.
    let domains = state.domains.filter { !$0.isEmpty }
    if !domains.isEmpty {
      let dns = NEDNSSettings(servers: t.dns)
      dns.matchDomains = domains
      dns.matchDomainsNoSearch = true
      settings.dnsSettings = dns
    }
    // No proxy settings: every app, browsers included, reaches the tailnets through the routes.
    os_log(
      "applying all-apps settings: %d routes, %d match domains", log: log, type: .info,
      t.v4Routes.count + t.v6Routes.count, domains.count)
    return settings
  }

  private func setDraining(_ on: Bool) {
    drainLock.lock()
    draining = on
    drainLock.unlock()
  }

  private func isDraining() -> Bool {
    drainLock.lock()
    defer { drainLock.unlock() }
    return draining
  }

  /// Proxy mode: nothing is routed into the tunnel, so packetFlow should stay empty. Reading anyway
  /// costs nothing while it is empty and drops anything that does arrive (e.g. a packet sent to the
  /// tunnel's own address) instead of leaving it queued in the kernel. Stops once all-apps mode
  /// takes over (at most the one read already pending is lost to it).
  private func drainPackets() {
    packetFlow.readPackets { [weak self] _, _ in
      guard let self, self.isDraining() else { return }
      self.drainPackets()
    }
  }

  // MARK: - Memory

  /// The extension is killed above 50 MB. Logs what is left before that, the footprint jetsam
  /// counts, and the mode, so a run on a device can be read in Console (subsystem
  /// com.ailio.tailfuse.tunnel).
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
      "memory at %{public}@ (%{public}@ mode): footprint %llu KiB, available %llu KiB", log: log, type: .info,
      when, mode, footprint / 1024, UInt64(os_proc_available_memory()) / 1024)
  }
}

/// Receives the engine's change events on a Go goroutine.
private final class TunnelListener: NSObject, TfmobileListenerProtocol {
  private let handler: (String) -> Void
  init(_ handler: @escaping (String) -> Void) { self.handler = handler }
  func onChange(_ kind: String?) { handler(kind ?? "") }
}

/// state.tunnel: the all-apps addresses, routes and DNS servers, parsed for NetworkExtension. The
/// defaults are the engine's (packets/DESIGN.md), for a state that cannot be read.
private struct TunnelSettings {
  var v4Addresses = ["198.18.0.1"]
  var v6Addresses = ["fd5e:9b07:4a1c::1"]
  var v4Routes: [(String, String)] = [("198.18.0.0", "255.254.0.0"), ("100.64.0.0", "255.192.0.0")]
  var v6Routes: [(String, Int)] = [("fd5e:9b07:4a1c::", 48), ("fd7a:115c:a1e0::", 48)]
  var dns = ["198.18.0.2", "fd5e:9b07:4a1c::2"]
  var mtu = 1280

  init() {}

  init(_ object: [String: Any]) {
    let addresses = (object["addresses"] as? [String] ?? []).compactMap(Self.cidr)
    let routes = (object["routes"] as? [String] ?? []).compactMap(Self.cidr)
    let v4a = addresses.filter { !$0.0.contains(":") }.map { $0.0 }
    let v6a = addresses.filter { $0.0.contains(":") }.map { $0.0 }
    let v4r = routes.filter { !$0.0.contains(":") }.map { ($0.0, Self.mask($0.1)) }
    let v6r = routes.filter { $0.0.contains(":") }
    if !v4a.isEmpty { v4Addresses = v4a }
    if !v6a.isEmpty { v6Addresses = v6a }
    if !v4r.isEmpty { v4Routes = v4r }
    if !v6r.isEmpty { v6Routes = v6r }
    if let d = object["dns"] as? [String], !d.isEmpty { dns = d }
    if let m = object["mtu"] as? Int, m >= 1280 { mtu = m }
  }

  /// "198.18.0.0/15" -> ("198.18.0.0", 15).
  private static func cidr(_ text: String) -> (String, Int)? {
    let parts = text.split(separator: "/")
    guard parts.count == 2, let bits = Int(parts[1]) else { return nil }
    return (String(parts[0]), bits)
  }

  /// A prefix length as a dotted IPv4 mask: 15 -> "255.254.0.0".
  private static func mask(_ bits: Int) -> String {
    let b = max(0, min(32, bits))
    let value: UInt32 = b == 0 ? 0 : ~UInt32(0) << UInt32(32 - b)
    return (0..<4).map { String((value >> UInt32(24 - $0 * 8)) & 0xff) }.joined(separator: ".")
  }
}

/// The part of the engine's `state` the tunnel needs.
private struct EngineState {
  var proxyHost = fallbackProxy.0
  var proxyPort = fallbackProxy.1
  var domains: [String] = []
  /// Use all-apps settings: the saved setting is on and nothing made this session fall back.
  var allAppsMode = false
  /// The engine is reading the utun fd.
  var packetsActive = false
  var tunnel = TunnelSettings()

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
    let error = result["all_apps_error"] as? String ?? ""
    out.allAppsMode = (result["all_apps"] as? Bool ?? false) && error.isEmpty
    out.packetsActive = result["all_apps_active"] as? Bool ?? false
    if let tunnel = result["tunnel"] as? [String: Any] {
      out.tunnel = TunnelSettings(tunnel)
    }
    if !error.isEmpty {
      os_log("all apps unavailable this session: %{public}@", log: log, type: .error, error)
    }
    return out
  }
}

private struct TunnelError: LocalizedError {
  let message: String
  init(_ message: String) { self.message = message }
  var errorDescription: String? { message }
}
