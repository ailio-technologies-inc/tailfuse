package expo.modules.tailfuseengine

import android.Manifest
import android.app.Activity
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.net.VpnService
import android.os.Build
import android.provider.Settings
import expo.modules.kotlin.exception.CodedException
import expo.modules.kotlin.functions.Coroutine
import expo.modules.kotlin.modules.Module
import expo.modules.kotlin.modules.ModuleDefinition
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeoutOrNull
import tfmobile.Tfmobile

private const val REQUEST_VPN = 4701
private const val START_TIMEOUT_MS = 30_000L

/** The JS surface in modules/tailfuse-engine/src/TailfuseEngine.types.ts; see ../../README.md. */
class TailfuseEngineModule : Module() {
  @Volatile private var consent: CompletableDeferred<Boolean>? = null
  private var askedForNotifications = false

  private val context: Context
    get() = appContext.reactContext ?: throw CodedException("ERR_NO_CONTEXT", "the app is not ready", null)

  override fun definition() = ModuleDefinition {
    Name("TailfuseEngine")
    Events("change")

    OnCreate {
      // So `call` works while Tailfuse is off (API.md: the app's own process runs the engine).
      appContext.reactContext?.let { Tfmobile.setStateDir(TailfuseVpnService.stateDir(it)) }
      TailfuseVpnService.sink = { kind -> sendEvent("change", mapOf("kind" to kind)) }
    }

    OnDestroy {
      TailfuseVpnService.sink = null
    }

    // The result of the system's "Connection request" dialog from VpnService.prepare().
    OnActivityResult { _, payload ->
      if (payload.requestCode == REQUEST_VPN) {
        consent?.complete(payload.resultCode == Activity.RESULT_OK)
      }
    }

    AsyncFunction("start") Coroutine { ->
      if (!hasVpnConsent()) {
        throw CodedException("ERR_VPN_DECLINED", "vpn permission was declined", null)
      }
      askForNotificationsOnce()
      TailfuseVpnService.start(context)
      val up = withTimeoutOrNull(START_TIMEOUT_MS) {
        while (!Tfmobile.running()) delay(100)
        true
      }
      if (up == null) throw CodedException("ERR_START_TIMEOUT", "tailfuse did not start within 30 seconds", null)
    }

    AsyncFunction("stop") Coroutine { ->
      withContext(Dispatchers.IO) { TailfuseVpnService.stop(context) }
    }

    AsyncFunction("isUp") { -> Tfmobile.running() }

    AsyncFunction("call") Coroutine { request: String ->
      // Tfmobile.call blocks (it may wait on the network) and never throws.
      withContext(Dispatchers.IO) { Tfmobile.call(request) }
    }

    // Android lets only the person turn always-on on, in system settings. We report it when the
    // service is running (VpnService.isAlwaysOn is an instance method, API 29+) and false otherwise.
    AsyncFunction("getAlwaysOn") { ->
      val service = TailfuseVpnService.instance
      Build.VERSION.SDK_INT >= 29 && service != null && service.isAlwaysOn
    }

    // An app cannot set it; open the VPN settings screen where the person can.
    AsyncFunction("setAlwaysOn") { _: Boolean ->
      val intent = Intent(Settings.ACTION_VPN_SETTINGS).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
      context.startActivity(intent)
    }
  }

  /** True when the VPN permission is held, asking for it (system dialog) if needed. */
  private suspend fun hasVpnConsent(): Boolean {
    val prepare = VpnService.prepare(context) ?: return true
    val pending = CompletableDeferred<Boolean>()
    consent = pending
    try {
      withContext(Dispatchers.Main) {
        appContext.throwingActivity.startActivityForResult(prepare, REQUEST_VPN)
      }
      return pending.await()
    } finally {
      consent = null
    }
  }

  // Android 13+: the notification needs a runtime permission, but the VPN works without it, so ask
  // once per process and never wait for the answer.
  private fun askForNotificationsOnce() {
    if (askedForNotifications || Build.VERSION.SDK_INT < 33) return
    askedForNotifications = true
    val activity = appContext.currentActivity ?: return
    if (activity.checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) {
      activity.requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), REQUEST_VPN + 1)
    }
  }
}
