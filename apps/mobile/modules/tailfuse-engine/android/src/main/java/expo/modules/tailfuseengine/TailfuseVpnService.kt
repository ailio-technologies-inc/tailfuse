package expo.modules.tailfuseengine

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.graphics.drawable.Icon
import android.net.ConnectivityManager
import android.net.Network
import android.net.ProxyInfo
import android.net.VpnService
import android.os.Build
import android.os.Handler
import android.os.Looper
import android.os.ParcelFileDescriptor
import android.provider.Settings
import android.system.ErrnoException
import android.system.Os
import android.system.OsConstants
import android.system.StructPollfd
import android.util.Log
import androidx.core.content.ContextCompat
import org.json.JSONObject
import tfmobile.Listener
import tfmobile.Tfmobile
import java.io.File
import java.net.NetworkInterface
import java.util.Collections
import java.util.Locale
import java.util.concurrent.Executors
import java.util.concurrent.RejectedExecutionException
import kotlin.concurrent.thread

/**
 * Keeps the Go engine alive and gives the phone a VPN that claims no routes.
 *
 * Nothing is routed into the tunnel, so the engine's own traffic cannot loop back into it. The VPN
 * exists for three reasons: Android keeps the process alive while a VPN service runs, the VPN network
 * can carry a system HTTP proxy (API 29+) that points apps at the engine's local router, and the
 * person gets the system's "VPN is on" key icon. See engines/mobile/API.md ("Where it runs").
 *
 * The engine calls (Start/Stop) are slow, so everything that touches it runs on [worker].
 */
class TailfuseVpnService : VpnService() {
  companion object {
    private const val TAG = "TailfuseVpn"
    const val ACTION_START = "com.ailio.tailfuse.vpn.START"
    const val ACTION_STOP = "com.ailio.tailfuse.vpn.STOP"
    private const val CHANNEL_ID = "tailfuse_status"
    private const val NOTIFICATION_ID = 1

    // An address is mandatory for establish(). 192.0.2.1 is TEST-NET-1 (RFC 5737): reserved for
    // documentation, never routed on the internet. As a /32 the only connected route Android adds is
    // the address itself, so no other traffic reaches the tunnel.
    private const val TUNNEL_ADDRESS = "192.0.2.1"
    private const val FALLBACK_PROXY_HOST = "127.0.0.1"
    private const val FALLBACK_PROXY_PORT = 43100

    // PAC proxies are NOT supported over VPNs (VpnService.Builder.setHttpProxy Javadoc), so the only
    // thing Android can be told is a direct host:port proxy for EVERY http(s) request. That is why
    // bringUp() turns on the engine's passthrough before Start: names no tailnet owns are then
    // connected directly by the engine (never through a tailnet), while tailnet names, conflicts,
    // Tailscale address space and paused tailnets' names keep the strict rules (engines/mobile/API.md,
    // "Passthrough"). Set to false to ship without the system proxy (local ports only).
    private const val APPLY_SYSTEM_PROXY = true

    /** The running service, if any. Read by the module for isAlwaysOn. */
    @Volatile var instance: TailfuseVpnService? = null
      private set

    /** Set by the module; receives "state" and "domains" from the engine, and "state" on start/stop. */
    @Volatile var sink: ((String) -> Unit)? = null

    fun stateDir(context: Context): String = File(context.filesDir, "tailfuse").absolutePath

    /** Starts the service in the foreground. Call while the app is visible, or from the system. */
    fun start(context: Context) {
      val intent = Intent(context, TailfuseVpnService::class.java).setAction(ACTION_START)
      ContextCompat.startForegroundService(context, intent)
    }

    /** Turns Tailfuse off; also works when only the engine (not the service) is up. */
    fun stop() {
      val service = instance
      if (service != null) service.shutdown() else {
        Tfmobile.stop()
        sink?.invoke("state")
      }
    }
  }

  private val worker = Executors.newSingleThreadExecutor { r -> Thread(r, "tailfuse-vpn").apply { isDaemon = true } }
  private val main = Handler(Looper.getMainLooper())
  @Volatile private var tunnel: ParcelFileDescriptor? = null
  @Volatile private var shuttingDown = false
  @Volatile private var bringUpScheduled = false
  private var callbackRegistered = false

  private val engineListener = object : Listener {
    override fun onChange(kind: String?) {
      val k = kind ?: "state"
      // "domains" needs no work here: the proxy is a fixed host:port, not a list of names.
      if (k == "state") onWorker { refreshNotification() }
      sink?.invoke(k)
    }
  }

  private val networkCallback = object : ConnectivityManager.NetworkCallback() {
    override fun onAvailable(network: Network) = pushInterfaces()
    override fun onLost(network: Network) = pushInterfaces()
    override fun onLinkPropertiesChanged(network: Network, linkProperties: android.net.LinkProperties) = pushInterfaces()
  }

  override fun onCreate() {
    super.onCreate()
    instance = this
    createChannel()
  }

  // The system can start us three ways: the module (ACTION_START), always-on VPN (action
  // "android.net.VpnService"), or a restart after the process died (null intent, START_STICKY).
  // All three bring the engine up; only ACTION_STOP turns it off.
  override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
    if (intent?.action == ACTION_STOP) {
      shutdown()
      return START_NOT_STICKY
    }
    try {
      val notification = buildNotification(null)
      if (Build.VERSION.SDK_INT >= 34) {
        startForeground(NOTIFICATION_ID, notification, ServiceInfo.FOREGROUND_SERVICE_TYPE_SYSTEM_EXEMPTED)
      } else {
        startForeground(NOTIFICATION_ID, notification)
      }
    } catch (e: Exception) {
      Log.e(TAG, "startForeground failed", e)
      stopSelf()
      return START_NOT_STICKY
    }
    shuttingDown = false
    if (!bringUpScheduled) {
      bringUpScheduled = true
      onWorker { bringUp() }
    }
    return START_STICKY
  }

  // Another VPN took over (or the person revoked us in settings): the system closes the tunnel.
  override fun onRevoke() {
    shutdown()
  }

  override fun onDestroy() {
    instance = null
    try {
      worker.execute { teardown() }
    } catch (_: RejectedExecutionException) {
      teardown()
    }
    worker.shutdown()
    super.onDestroy()
  }

  private fun onWorker(block: () -> Unit) {
    try {
      worker.execute {
        try {
          block()
        } catch (e: Exception) {
          Log.e(TAG, "worker task failed", e)
        }
      }
    } catch (_: RejectedExecutionException) {
    }
  }

  private fun bringUp() {
    try {
      Tfmobile.setStateDir(stateDir(this))
      Tfmobile.setDeviceName(deviceName())
      Tfmobile.setInterfaces(interfacesAsString())
      Tfmobile.setListener(engineListener)
      // Every app's web traffic reaches the router through the system proxy (no PAC over a VPN):
      // let names no tailnet owns pass through directly. Android only; iOS never sets this.
      Tfmobile.setPassthrough(true)
      Tfmobile.start(stateDir(this))
      registerNetworkCallback()
      if (!establishTunnel()) {
        Log.e(TAG, "establish() returned null: VPN permission missing or revoked")
        shutdown()
        return
      }
      refreshNotification()
      sink?.invoke("state")
    } catch (e: Exception) {
      Log.e(TAG, "could not start the engine", e)
      shutdown()
    }
  }

  /** Stops the engine and the tunnel, then the service. Safe to call more than once. */
  fun shutdown() {
    shuttingDown = true
    onWorker {
      teardown()
      sink?.invoke("state")
      main.post {
        stopForeground(STOP_FOREGROUND_REMOVE)
        stopSelf()
      }
    }
  }

  private fun teardown() {
    bringUpScheduled = false
    if (callbackRegistered) {
      try {
        getSystemService(ConnectivityManager::class.java).unregisterNetworkCallback(networkCallback)
      } catch (_: IllegalArgumentException) {
      }
      callbackRegistered = false
    }
    val t = tunnel
    tunnel = null
    try {
      t?.close()
    } catch (_: Exception) {
    }
    Tfmobile.setListener(null)
    Tfmobile.stop()
  }

  private fun establishTunnel(): Boolean {
    val (proxyHost, proxyPort) = proxyHostPort()
    val builder = Builder()
      .setSession("Tailfuse")
      .addAddress(TUNNEL_ADDRESS, 32)
      // Without an IPv6 address, route or DNS server the VPN blocks ALL IPv6 for every app
      // (VpnService.Builder.allowFamily Javadoc). allowFamily unblocks it; it falls through to the
      // underlying network because we add no routes.
      .allowFamily(OsConstants.AF_INET6)
    // No addRoute(): the tunnel claims nothing. No addDnsServer(): the default network's DNS is used.
    // Not calling addDisallowedApplication(packageName): the engine's Go sockets already use the
    // underlying network (the tunnel has no routes, and Go ignores the system HTTP proxy), so it
    // would gain nothing, and it would take this app off the VPN network, so the app's own
    // WebView/fetch would lose the proxy and could not open tailnet names. Revisit only if a device
    // shows the engine's sockets on the VPN network.
    packageManager.getLaunchIntentForPackage(packageName)?.let {
      builder.setConfigureIntent(
        PendingIntent.getActivity(this, 0, it, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
      )
    }
    if (Build.VERSION.SDK_INT >= 29) {
      builder.setMetered(false) // inherit meteredness from the real network
      if (APPLY_SYSTEM_PROXY) builder.setHttpProxy(ProxyInfo.buildDirectProxy(proxyHost, proxyPort))
    }
    val fresh = builder.establish() ?: return false
    val old = tunnel
    tunnel = fresh
    drain(fresh)
    try {
      old?.close()
    } catch (_: Exception) {
    }
    return true
  }

  // Reads and drops whatever is written to the tun so it can never fill up and block. The fd is
  // non-blocking (the default), so poll with a timeout and stop when the tunnel is replaced or closed.
  private fun drain(pfd: ParcelFileDescriptor) {
    thread(name = "tailfuse-tun-drain", isDaemon = true) {
      val buf = ByteArray(32 * 1024)
      val fd = pfd.fileDescriptor
      try {
        while (tunnel === pfd) {
          val p = StructPollfd().apply {
            this.fd = fd
            events = OsConstants.POLLIN.toShort()
          }
          Os.poll(arrayOf(p), 1000)
          if (p.revents.toInt() and OsConstants.POLLIN != 0) {
            try {
              Os.read(fd, buf, 0, buf.size)
            } catch (e: ErrnoException) {
              if (e.errno != OsConstants.EAGAIN && e.errno != OsConstants.EINTR) break
            }
          } else if (p.revents.toInt() and (OsConstants.POLLERR or OsConstants.POLLHUP or OsConstants.POLLNVAL) != 0) {
            break
          }
        }
      } catch (_: Exception) {
        // the tunnel was closed under us
      }
    }
  }

  private fun proxyHostPort(): Pair<String, Int> {
    try {
      val proxy = JSONObject(Tfmobile.call("""{"cmd":"state"}""")).getJSONObject("result").getString("proxy")
      val i = proxy.lastIndexOf(':')
      return proxy.substring(0, i) to proxy.substring(i + 1).toInt()
    } catch (_: Exception) {
      return FALLBACK_PROXY_HOST to FALLBACK_PROXY_PORT
    }
  }

  private fun deviceName(): String {
    val name = try {
      Settings.Global.getString(contentResolver, "device_name")
    } catch (_: Exception) {
      null
    }
    return if (name.isNullOrBlank()) Build.MODEL else name
  }

  private fun registerNetworkCallback() {
    if (callbackRegistered) return
    getSystemService(ConnectivityManager::class.java).registerDefaultNetworkCallback(networkCallback)
    callbackRegistered = true
  }

  private fun pushInterfaces() {
    try {
      Tfmobile.setInterfaces(interfacesAsString())
    } catch (e: Exception) {
      Log.w(TAG, "setInterfaces failed", e)
    }
  }

  // Ported from tailscale-android, android/src/main/java/com/tailscale/ipn/App.kt (getInterfacesAsString).
  // Copyright (c) Tailscale Inc & AUTHORS. SPDX-License-Identifier: BSD-3-Clause.
  // Changes: a null/throwing getNetworkInterfaces() yields an empty list instead of crashing.
  // Format, one line per interface: "name index mtu up broadcast loopback pointToPoint multicast | addr/prefix ...".
  // Android has no supportsBroadcast(), so multicast stands in for it, as upstream does.
  private fun interfacesAsString(): String {
    val interfaces: List<NetworkInterface> = try {
      Collections.list(NetworkInterface.getNetworkInterfaces() ?: return "")
    } catch (_: Exception) {
      return ""
    }
    val sb = StringBuilder()
    for (nif in interfaces) {
      try {
        sb.append(
          String.format(
            Locale.ROOT,
            "%s %d %d %b %b %b %b %b |",
            nif.name,
            nif.index,
            nif.mtu,
            nif.isUp,
            nif.supportsMulticast(),
            nif.isLoopback,
            nif.isPointToPoint,
            nif.supportsMulticast()
          )
        )
        for (ia in nif.interfaceAddresses) {
          val parts = ia.toString().split("/", limit = 0)
          if (parts.size > 1) {
            sb.append(String.format(Locale.ROOT, "%s/%d ", parts[1], ia.networkPrefixLength))
          }
        }
      } catch (e: Exception) {
        continue
      }
      sb.append("\n")
    }
    return sb.toString()
  }

  // ---- notification ----

  private fun createChannel() {
    val channel = NotificationChannel(CHANNEL_ID, "Tailfuse status", NotificationManager.IMPORTANCE_LOW)
    channel.setShowBadge(false)
    getSystemService(NotificationManager::class.java).createNotificationChannel(channel)
  }

  private fun onlineTailnets(): Int? = try {
    val profiles = JSONObject(Tfmobile.call("""{"cmd":"state"}""")).getJSONObject("result").getJSONArray("profiles")
    (0 until profiles.length()).count { profiles.getJSONObject(it).optBoolean("online") }
  } catch (_: Exception) {
    null
  }

  private fun refreshNotification() {
    if (shuttingDown) return
    getSystemService(NotificationManager::class.java).notify(NOTIFICATION_ID, buildNotification(onlineTailnets()))
  }

  private fun buildNotification(online: Int?): Notification {
    val text = when {
      online == null || online == 0 -> "Tailfuse is on"
      online == 1 -> "1 tailnet online"
      else -> "$online tailnets online"
    }
    val open = packageManager.getLaunchIntentForPackage(packageName)?.let {
      PendingIntent.getActivity(this, 0, it, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
    }
    val stopIntent = PendingIntent.getService(
      this, 1, Intent(this, TailfuseVpnService::class.java).setAction(ACTION_STOP),
      PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
    )
    val icon = R.drawable.ic_tailfuse_status
    val builder = Notification.Builder(this, CHANNEL_ID)
      .setSmallIcon(icon)
      .setContentTitle("Tailfuse")
      .setContentText(text)
      .setOngoing(true)
      .setOnlyAlertOnce(true)
      .setCategory(Notification.CATEGORY_SERVICE)
      .setContentIntent(open)
      .addAction(Notification.Action.Builder(Icon.createWithResource(this, icon), "Turn off", stopIntent).build())
    if (Build.VERSION.SDK_INT >= 31) {
      builder.setForegroundServiceBehavior(Notification.FOREGROUND_SERVICE_IMMEDIATE)
    }
    return builder.build()
  }
}
