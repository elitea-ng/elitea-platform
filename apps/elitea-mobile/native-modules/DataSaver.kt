package app.datasaver

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.net.ConnectivityManager
import android.os.Build
import expo.modules.kotlin.modules.Module
import expo.modules.kotlin.modules.ModuleDefinition

/**
 * Expo inline module (app.config.ts `experiments.inlineModules`): the system's Data Saver
 * state, which `expo-network` does not expose (ADR-0050 decision 6, #1650). Do not rename:
 * the class name, the file name and JS `requireOptionalNativeModule('DataSaver')` must agree.
 *
 * - `isLowData()` is true while Data Saver is on for this app:
 *   ConnectivityManager.getRestrictBackgroundStatus() is ENABLED (the user turned Data Saver
 *   on and did not allow this app through). WHITELISTED (allowed through) is not low data.
 *   The status is read for the app's own network use, so no metered check is added: a
 *   metered link with Data Saver off is not a request to save data.
 * - `onChange` fires with `{ lowData }` on ACTION_RESTRICT_BACKGROUND_CHANGED while JS listens.
 */
class DataSaver : Module() {
  private var receiver: BroadcastReceiver? = null

  private fun context(): Context? = appContext.reactContext?.applicationContext

  private fun lowData(): Boolean {
    val manager = context()?.getSystemService(Context.CONNECTIVITY_SERVICE) as? ConnectivityManager ?: return false
    return manager.restrictBackgroundStatus == ConnectivityManager.RESTRICT_BACKGROUND_STATUS_ENABLED
  }

  private fun unregister() {
    val r = receiver ?: return
    receiver = null
    try {
      context()?.unregisterReceiver(r)
    } catch (_: IllegalArgumentException) {
      // Already gone with the context.
    }
  }

  override fun definition() = ModuleDefinition {
    Name("DataSaver")
    Events("onChange")

    Function("isLowData") { lowData() }

    OnStartObserving {
      val ctx = context() ?: return@OnStartObserving
      if (receiver != null) return@OnStartObserving
      val r = object : BroadcastReceiver() {
        override fun onReceive(context: Context?, intent: Intent?) {
          sendEvent("onChange", mapOf("lowData" to lowData()))
        }
      }
      val filter = IntentFilter(ConnectivityManager.ACTION_RESTRICT_BACKGROUND_CHANGED)
      // A protected system broadcast; NOT_EXPORTED keeps other apps from sending it to us.
      if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
        ctx.registerReceiver(r, filter, Context.RECEIVER_NOT_EXPORTED)
      } else {
        ctx.registerReceiver(r, filter)
      }
      receiver = r
    }

    OnStopObserving { unregister() }

    // A JS reload destroys the module without OnStopObserving: never leave the receiver behind.
    OnDestroy { unregister() }
  }
}
