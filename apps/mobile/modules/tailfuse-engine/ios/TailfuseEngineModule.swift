// The app side of Tailfuse on iOS: turns the packet tunnel (targets/tunnel) on and off and passes
// JSON requests to the engine, through the tunnel while it is up and in-process while it is down.
// See engines/mobile/API.md ("Where it runs") and README.md in this folder.

import ExpoModulesCore
import NetworkExtension
import Tfmobile
import UIKit

private let appGroup = "group.com.ailio.tailfuse"
private let tunnelBundleId = "com.ailio.tailfuse.tunnel"
private let startTimeout: TimeInterval = 30
private let stopTimeout: TimeInterval = 10
private let deviceNameKey = "deviceName"
private let alwaysOnKey = "alwaysOn"

public class TailfuseEngineModule: Module {
  // Main queue only.
  private var manager: NETunnelProviderManager?
  private var statusObserver: NSObjectProtocol?

  /// In-process engine calls (Tailfuse off). Serial, so start() can let one in flight finish
  /// before the extension takes over the state folder: only one process writes it at a time.
  private let localQueue = DispatchQueue(label: "com.ailio.tailfuse.engine.local", qos: .userInitiated)
  private var stateDirGiven = false  // localQueue only

  private var shared: UserDefaults? { UserDefaults(suiteName: appGroup) }

  public func definition() -> ModuleDefinition {
    Name("TailfuseEngine")

    Events("change")

    OnCreate { [self] in
      DispatchQueue.main.async { [self] in
        // The extension cannot push to the app; the only thing seen here is the VPN status (the JS
        // polls `state` every 3 s for the rest). Status notifications only arrive for a
        // configuration this process has loaded, hence the load.
        self.statusObserver = NotificationCenter.default.addObserver(
          forName: .NEVPNStatusDidChange, object: nil, queue: .main
        ) { [weak self] _ in
          self?.sendEvent("change", ["kind": "state"])
        }
        self.loadManager { _, _ in }
      }
    }

    OnDestroy {
      if let observer = self.statusObserver {
        NotificationCenter.default.removeObserver(observer)
      }
    }

    AsyncFunction("start") { (promise: Promise) in
      DispatchQueue.main.async { self.start(promise) }
    }

    AsyncFunction("stop") { (promise: Promise) in
      DispatchQueue.main.async { self.stop(promise) }
    }

    AsyncFunction("isUp") { (promise: Promise) in
      DispatchQueue.main.async {
        self.loadManager { manager, _ in
          // Starting counts: `state` is answered locally then (see call), with the tailnets
          // shown as connecting rather than off.
          promise.resolve(manager.map { Self.isActive($0.connection.status) } ?? false)
        }
      }
    }

    AsyncFunction("call") { (request: String, promise: Promise) in
      DispatchQueue.main.async { self.call(request, promise) }
    }

    AsyncFunction("getAlwaysOn") { () -> Bool in
      UserDefaults(suiteName: appGroup)?.bool(forKey: alwaysOnKey) ?? false
    }

    AsyncFunction("setAlwaysOn") { (enabled: Bool, promise: Promise) in
      DispatchQueue.main.async { self.setAlwaysOn(enabled, promise) }
    }
  }

  // MARK: - On and off

  private func start(_ promise: Promise) {
    // The phone's name, for the hostname on each tailnet (read by the extension). Since iOS 16 the
    // name is just the model ("iPhone") without the user-assigned-device-name entitlement; then
    // the engine's own fallback, "tailfuse-ios", says more.
    let name = UIDevice.current.name
    if name.isEmpty || name == UIDevice.current.model {
      shared?.removeObject(forKey: deviceNameKey)
    } else {
      shared?.set(name, forKey: deviceNameKey)
    }

    // Let an in-process call that is writing the config finish before the extension reads it.
    localQueue.async {
      DispatchQueue.main.async { self.startAfterLocalCalls(promise) }
    }
  }

  private func startAfterLocalCalls(_ promise: Promise) {
    loadManager { existing, error in
      if let error {
        promise.reject("ERR_TAILFUSE_START", "could not read the vpn configuration: \(error.localizedDescription)")
        return
      }
      let manager = existing ?? NETunnelProviderManager()
      self.configure(manager)
      // Saving is where iOS asks for the VPN permission the first time. It also re-enables the
      // configuration when another VPN app took over since.
      manager.saveToPreferences { error in
        if let error {
          if Self.isPermissionDenied(error) {
            promise.reject("ERR_VPN_PERMISSION", "vpn permission was declined")
          } else {
            promise.reject("ERR_TAILFUSE_START", "could not save the vpn configuration: \(error.localizedDescription)")
          }
          return
        }
        self.manager = manager
        // A saved configuration must be loaded again before it can be started.
        manager.loadFromPreferences { error in
          if let error {
            promise.reject("ERR_TAILFUSE_START", "could not load the vpn configuration: \(error.localizedDescription)")
            return
          }
          self.connect(manager, promise)
        }
      }
    }
  }

  private func connect(_ manager: NETunnelProviderManager, _ promise: Promise) {
    let connection = manager.connection
    if connection.status == .connected {
      promise.resolve(nil)
      return
    }
    if connection.status != .connecting && connection.status != .reasserting {
      do {
        try connection.startVPNTunnel()
      } catch {
        promise.reject("ERR_TAILFUSE_START", "could not start tailfuse: \(error.localizedDescription)")
        return
      }
    }
    var seenActive = false
    wait(on: connection, timeout: startTimeout) { status in
      if status == .connecting || status == .reasserting { seenActive = true }
      return status == .connected || status == .invalid || (status == .disconnected && seenActive)
    } done: { settled in
      switch connection.status {
      case .connected:
        promise.resolve(nil)
      case _ where !settled:
        promise.reject("ERR_TAILFUSE_START", "tailfuse did not come up within \(Int(startTimeout)) seconds")
      default:
        connection.fetchLastDisconnectError { error in
          let reason = error.map { ": \($0.localizedDescription)" } ?? ""
          promise.reject("ERR_TAILFUSE_START", "tailfuse stopped while starting\(reason)")
        }
      }
    }
  }

  private func stop(_ promise: Promise) {
    loadManager { manager, _ in
      guard let manager else {
        promise.resolve(nil)
        return
      }
      let disconnect = {
        manager.connection.stopVPNTunnel()
        self.wait(on: manager.connection, timeout: stopTimeout) { status in
          status == .disconnected || status == .invalid
        } done: { _ in
          promise.resolve(nil)
        }
      }
      // With on-demand still on, iOS would connect again at once. The always-on preference is
      // kept and applied again at the next start.
      guard manager.isOnDemandEnabled else {
        disconnect()
        return
      }
      manager.isOnDemandEnabled = false
      manager.saveToPreferences { error in
        if let error {
          promise.reject("ERR_TAILFUSE_STOP", "could not turn always-on off: \(error.localizedDescription)")
          return
        }
        disconnect()
      }
    }
  }

  private func setAlwaysOn(_ enabled: Bool, _ promise: Promise) {
    let previous = shared?.bool(forKey: alwaysOnKey) ?? false
    shared?.set(enabled, forKey: alwaysOnKey)
    loadManager { manager, _ in
      // While Tailfuse is off the preference is only stored: turning on-demand on now would
      // start the tunnel at once. start() applies it.
      guard let manager, enabled == false || Self.isActive(manager.connection.status) else {
        promise.resolve(nil)
        return
      }
      self.applyOnDemand(manager, enabled)
      manager.saveToPreferences { error in
        if let error {
          self.shared?.set(previous, forKey: alwaysOnKey)
          manager.isOnDemandEnabled = previous && Self.isActive(manager.connection.status)
          promise.reject("ERR_TAILFUSE_ALWAYS_ON", "could not change always-on: \(error.localizedDescription)")
          return
        }
        promise.resolve(nil)
      }
    }
  }

  // MARK: - Engine calls

  private func call(_ request: String, _ promise: Promise, fellBack: Bool = false) {
    loadManager { manager, _ in
      guard let session = manager?.connection as? NETunnelProviderSession else {
        self.callLocally(request, promise)
        return
      }
      switch session.status {
      case .connected:
        do {
          try session.sendProviderMessage(Data(request.utf8)) { reply in
            DispatchQueue.main.async {
              if let reply {
                promise.resolve(String(decoding: reply, as: UTF8.self))
              } else {
                // No reply: the tunnel went down mid-call (or is going). Route it again, once.
                self.retry(request, promise, fellBack: fellBack)
              }
            }
          }
        } catch {
          self.retry(request, promise, fellBack: fellBack)
        }
      case .connecting where Self.isStateRead(request), .reasserting where Self.isStateRead(request):
        // Launching while the tunnel comes up (always-on, or just switched on) would otherwise
        // show nothing until it is up. Reading the state only reads the config, so answer here,
        // as on: the tailnets then read "Connecting…" until the extension answers.
        self.callLocally(request) { promise.resolve(Self.markedUp($0)) }
      case .connecting, .reasserting, .disconnecting:
        // The extension may already own the state folder: wait until it is clearly up or down.
        self.wait(on: session, timeout: startTimeout) { status in
          status == .connected || status == .disconnected || status == .invalid
        } done: { settled in
          if settled {
            self.call(request, promise, fellBack: fellBack)
          } else {
            promise.resolve(Self.errorResponse("tailfuse is busy starting or stopping"))
          }
        }
      default:  // .disconnected, .invalid
        self.callLocally(request, promise)
      }
    }
  }

  private func retry(_ request: String, _ promise: Promise, fellBack: Bool) {
    if fellBack {
      promise.resolve(Self.errorResponse("tailfuse did not answer"))
    } else {
      call(request, promise, fellBack: true)
    }
  }

  /// The engine in this process, never started: only the "works off" commands succeed.
  private func callLocally(_ request: String, _ promise: Promise) {
    callLocally(request) { promise.resolve($0) }
  }

  private func callLocally(_ request: String, _ done: @escaping (String) -> Void) {
    localQueue.async {
      if !self.stateDirGiven {
        guard
          let container = FileManager.default.containerURL(forSecurityApplicationGroupIdentifier: appGroup)
        else {
          done(Self.errorResponse("the app group container is missing"))
          return
        }
        TfmobileSetStateDir(container.appendingPathComponent("tailfuse", isDirectory: true).path)
        self.stateDirGiven = true
      }
      done(TfmobileCall(request))
    }
  }

  private static func isStateRead(_ request: String) -> Bool {
    let json = try? JSONSerialization.jsonObject(with: Data(request.utf8)) as? [String: Any]
    return json?["cmd"] as? String == "state"
  }

  /// A local `state` reply with "up" set, for while the tunnel is starting.
  private static func markedUp(_ response: String) -> String {
    guard
      var json = try? JSONSerialization.jsonObject(with: Data(response.utf8)) as? [String: Any],
      json["ok"] as? Bool == true,
      var result = json["result"] as? [String: Any]
    else { return response }
    result["up"] = true
    json["result"] = result
    guard let data = try? JSONSerialization.data(withJSONObject: json) else { return response }
    return String(decoding: data, as: UTF8.self)
  }

  // MARK: - The configuration

  /// The one Tailfuse configuration, or nil before the first start. Cached; call on main.
  private func loadManager(_ done: @escaping (NETunnelProviderManager?, Error?) -> Void) {
    if let manager {
      done(manager, nil)
      return
    }
    NETunnelProviderManager.loadAllFromPreferences { managers, error in
      DispatchQueue.main.async {
        let ours = managers?.first {
          ($0.protocolConfiguration as? NETunnelProviderProtocol)?.providerBundleIdentifier == tunnelBundleId
        }
        if let ours, self.manager == nil {
          self.manager = ours
        }
        done(self.manager, error)
      }
    }
  }

  private func configure(_ manager: NETunnelProviderManager) {
    let proto = (manager.protocolConfiguration as? NETunnelProviderProtocol) ?? NETunnelProviderProtocol()
    proto.providerBundleIdentifier = tunnelBundleId
    proto.serverAddress = "Tailfuse"
    proto.disconnectOnSleep = false
    manager.protocolConfiguration = proto
    manager.localizedDescription = "Tailfuse"
    manager.isEnabled = true
    applyOnDemand(manager, shared?.bool(forKey: alwaysOnKey) ?? false)
  }

  private func applyOnDemand(_ manager: NETunnelProviderManager, _ enabled: Bool) {
    let rule = NEOnDemandRuleConnect()
    rule.interfaceTypeMatch = .any
    manager.onDemandRules = [rule]
    manager.isOnDemandEnabled = enabled
  }

  // MARK: - Helpers

  /// Calls done(true) once check passes for the connection's status, or done(false) after
  /// timeout. Driven by status notifications, not polling. Call on main.
  private func wait(
    on connection: NEVPNConnection, timeout: TimeInterval,
    until check: @escaping (NEVPNStatus) -> Bool, done: @escaping (Bool) -> Void
  ) {
    if check(connection.status) {
      done(true)
      return
    }
    var finished = false
    var observer: NSObjectProtocol?
    let finish = { (settled: Bool) in
      guard !finished else { return }
      finished = true
      if let observer { NotificationCenter.default.removeObserver(observer) }
      done(settled)
    }
    observer = NotificationCenter.default.addObserver(
      forName: .NEVPNStatusDidChange, object: connection, queue: .main
    ) { _ in
      if check(connection.status) { finish(true) }
    }
    DispatchQueue.main.asyncAfter(deadline: .now() + timeout) { finish(false) }
  }

  private static func isActive(_ status: NEVPNStatus) -> Bool {
    status == .connected || status == .connecting || status == .reasserting
  }

  /// Declining the VPN prompt fails the save with NEVPNError.configurationReadWriteFailed
  /// ("permission denied").
  private static func isPermissionDenied(_ error: Error) -> Bool {
    let ns = error as NSError
    return ns.domain == NEVPNErrorDomain && ns.code == NEVPNError.Code.configurationReadWriteFailed.rawValue
  }

  private static func errorResponse(_ message: String) -> String {
    let data = try? JSONSerialization.data(withJSONObject: ["ok": false, "error": message])
    return data.map { String(decoding: $0, as: UTF8.self) } ?? #"{"ok":false,"error":"internal error"}"#
  }
}
