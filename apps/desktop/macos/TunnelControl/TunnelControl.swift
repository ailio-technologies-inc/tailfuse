// The desktop app's side of the Mac tunnel: a static library with a C surface that the Rust app
// (src-tauri/src/tunnel.rs) links. It installs the system extension, keeps one VPN configuration
// for it, starts and stops it, and carries the engine's JSON requests to it
// (NETunnelProviderSession.sendProviderMessage), as the phone app does
// (apps/mobile/modules/tailfuse-engine/ios/TailfuseEngineModule.swift).
//
// It has to live inside Tailfuse.app's own process: macOS only lets the app that contains a
// system extension ask to install it, and only from /Applications.
//
// Every entry point is asynchronous and answers once through `done(ctx, ok, text)`: ok is 1 or 0,
// text a UTF-8 string the callee owns only for the call (copy it). Status strings are stable:
// activate answers "activated", "needs_approval" (System Settings > Login Items & Extensions; the
// request stays open and answers again once approved) or "will_activate_after_reboot".

import Foundation
import NetworkExtension
import SystemExtensions

public typealias TFDone = @convention(c) (UnsafeMutableRawPointer?, Int32, UnsafePointer<CChar>) -> Void

private let extensionID = "com.ailio.tailfuse.tunnel"
private let queue = DispatchQueue(label: "com.ailio.tailfuse.tunnel-control")

private func answer(_ done: TFDone, _ ctx: UnsafeMutableRawPointer?, _ ok: Bool, _ text: String) {
  text.withCString { done(ctx, ok ? 1 : 0, $0) }
}

// MARK: - Installing the extension

private final class ActivationDelegate: NSObject, OSSystemExtensionRequestDelegate {
  let done: TFDone
  let ctx: UnsafeMutableRawPointer?
  private var answered = false
  init(_ done: TFDone, _ ctx: UnsafeMutableRawPointer?) {
    self.done = done
    self.ctx = ctx
  }

  private func once(_ ok: Bool, _ text: String) {
    // needs_approval is an interim answer; the request answers again when it finishes.
    if answered { return }
    answered = true
    answer(done, ctx, ok, text)
    pending.removeAll { $0 === self }
  }

  func request(
    _ request: OSSystemExtensionRequest, actionForReplacingExtension existing: OSSystemExtensionProperties,
    withExtension ext: OSSystemExtensionProperties
  ) -> OSSystemExtensionRequest.ReplacementAction {
    .replace  // a new Tailfuse.app brings its own build of the tunnel
  }

  func requestNeedsUserApproval(_ request: OSSystemExtensionRequest) {
    // Not `once`: the caller shows "approve in System Settings" and keeps waiting for the result.
    answer(done, ctx, true, "needs_approval")
  }

  func request(_ request: OSSystemExtensionRequest, didFinishWithResult result: OSSystemExtensionRequest.Result) {
    once(true, result == .willCompleteAfterReboot ? "will_activate_after_reboot" : "activated")
  }

  func request(_ request: OSSystemExtensionRequest, didFailWithError error: Error) {
    once(false, "the tunnel could not be installed: \(error.localizedDescription)")
  }
}

/// Requests in flight (OSSystemExtensionRequest holds its delegate weakly).
private var pending: [ActivationDelegate] = []

/// Installs (or updates) the tunnel extension. `done` may be called twice: "needs_approval", then
/// the final answer.
@_cdecl("tf_tunnel_activate")
public func tf_tunnel_activate(_ ctx: UnsafeMutableRawPointer?, _ done: TFDone) {
  queue.async {
    let delegate = ActivationDelegate(done, ctx)
    pending.append(delegate)
    let request = OSSystemExtensionRequest.activationRequest(forExtensionWithIdentifier: extensionID, queue: queue)
    request.delegate = delegate
    OSSystemExtensionManager.shared.submitRequest(request)
  }
}

// MARK: - The VPN configuration

private func loadManager(create: Bool, deviceName: String? = nil, _ done: @escaping (NETunnelProviderManager?, Error?) -> Void) {
  NETunnelProviderManager.loadAllFromPreferences { managers, error in
    if let error { return done(nil, error) }
    let ours = managers?.first { ($0.protocolConfiguration as? NETunnelProviderProtocol)?.providerBundleIdentifier == extensionID }
    guard create || ours != nil else { return done(ours, nil) }
    let manager = ours ?? NETunnelProviderManager()
    let proto = (manager.protocolConfiguration as? NETunnelProviderProtocol) ?? NETunnelProviderProtocol()
    proto.providerBundleIdentifier = extensionID
    proto.serverAddress = "Tailfuse"  // shown in System Settings > VPN; there is no server
    var config = proto.providerConfiguration ?? [:]
    if let deviceName, !deviceName.isEmpty { config["deviceName"] = deviceName }
    proto.providerConfiguration = config
    manager.protocolConfiguration = proto
    manager.localizedDescription = "Tailfuse"
    manager.isEnabled = true
    manager.saveToPreferences { error in
      if let error { return done(nil, error) }
      // Reload: a manager just saved for the first time cannot start until it is read back.
      manager.loadFromPreferences { error in done(error == nil ? manager : nil, error) }
    }
  }
}

private func statusName(_ status: NEVPNStatus) -> String {
  switch status {
  case .invalid: return "invalid"
  case .disconnected: return "disconnected"
  case .connecting: return "connecting"
  case .connected: return "connected"
  case .reasserting: return "reasserting"
  case .disconnecting: return "disconnecting"
  @unknown default: return "unknown"
  }
}

/// Saves the configuration (first time: macOS asks the person to allow a VPN configuration) and
/// starts the tunnel. Answers once it is connected, or why not (30 s at most).
@_cdecl("tf_tunnel_start")
public func tf_tunnel_start(_ deviceName: UnsafePointer<CChar>?, _ ctx: UnsafeMutableRawPointer?, _ done: TFDone) {
  let name = deviceName.map { String(cString: $0) }
  loadManager(create: true, deviceName: name) { manager, error in
    guard let manager else {
      return answer(done, ctx, false, "the VPN configuration could not be saved: \(error?.localizedDescription ?? "unknown")")
    }
    do {
      try manager.connection.startVPNTunnel()
    } catch {
      return answer(done, ctx, false, "the tunnel did not start: \(error.localizedDescription)")
    }
    waitFor(manager, until: [.connected], timeout: 30, done, ctx)
  }
}

@_cdecl("tf_tunnel_stop")
public func tf_tunnel_stop(_ ctx: UnsafeMutableRawPointer?, _ done: TFDone) {
  loadManager(create: false) { manager, _ in
    guard let manager else { return answer(done, ctx, true, "disconnected") }
    manager.connection.stopVPNTunnel()
    waitFor(manager, until: [.disconnected, .invalid], timeout: 15, done, ctx)
  }
}

/// "none" (never configured), or the connection's status name.
@_cdecl("tf_tunnel_status")
public func tf_tunnel_status(_ ctx: UnsafeMutableRawPointer?, _ done: TFDone) {
  loadManager(create: false) { manager, error in
    if let error { return answer(done, ctx, false, error.localizedDescription) }
    answer(done, ctx, true, manager.map { statusName($0.connection.status) } ?? "none")
  }
}

private func waitFor(
  _ manager: NETunnelProviderManager, until: Set<NEVPNStatus>, timeout: TimeInterval, _ done: TFDone,
  _ ctx: UnsafeMutableRawPointer?
) {
  let started = Date()
  let deadline = started.addingTimeInterval(timeout)
  func check() {
    let status = manager.connection.status
    if until.contains(status) { return answer(done, ctx, true, statusName(status)) }
    if Date() > deadline { return answer(done, ctx, false, "the tunnel is still \(statusName(status))") }
    // Starting, the status can read disconnected for a moment before it moves to connecting.
    // Still disconnected after 3 s means the extension tried and refused.
    if until.contains(.connected) && status == .disconnected && Date().timeIntervalSince(started) > 3 {
      return answer(done, ctx, false, "the tunnel stopped while starting (see Console, subsystem com.ailio.tailfuse.tunnel)")
    }
    queue.asyncAfter(deadline: .now() + 0.25, execute: check)
  }
  queue.async(execute: check)
}

// MARK: - The engine

/// One engine request (engines/mobile/API.md), one response, through the running tunnel.
@_cdecl("tf_tunnel_call")
public func tf_tunnel_call(_ request: UnsafePointer<CChar>, _ ctx: UnsafeMutableRawPointer?, _ done: TFDone) {
  let body = Data(String(cString: request).utf8)
  loadManager(create: false) { manager, _ in
    guard let session = manager?.connection as? NETunnelProviderSession, session.status == .connected else {
      return answer(done, ctx, false, "tailfuse is off")
    }
    do {
      try session.sendProviderMessage(body) { reply in
        answer(done, ctx, reply != nil, reply.map { String(decoding: $0, as: UTF8.self) } ?? "the tunnel did not answer")
      }
    } catch {
      answer(done, ctx, false, "the tunnel did not take the request: \(error.localizedDescription)")
    }
  }
}
