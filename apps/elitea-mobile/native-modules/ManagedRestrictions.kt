package app.managed

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.content.RestrictionsManager
import android.os.Build
import android.os.Bundle
import expo.modules.kotlin.modules.Module
import expo.modules.kotlin.modules.ModuleDefinition

/**
 * Expo inline module (app.config.ts `experiments.inlineModules`): Android managed
 * configuration (ADR-0052 decision 7, W1b). Do not rename: the class name, the file name
 * and JS `requireOptionalNativeModule('ManagedRestrictions')` must agree.
 *
 * - `read()` returns the app restrictions an EMM (Intune, Workspace ONE, Test DPC) set for
 *   this app, as a plain map of the keys plugins/withManagedConfiguration.js declares in
 *   `app_restrictions.xml`. Values keep their types (string, boolean, integer); the JS
 *   parser (src/security/managedConfig.ts) ignores any value of the wrong type.
 * - `onChange` fires on ACTION_APPLICATION_RESTRICTIONS_CHANGED while JS listens, with the
 *   new map: also when the EMM removes the configuration (an empty map).
 *
 * Nothing is cached here and nothing is written: Android keeps the restrictions.
 */
class ManagedRestrictions : Module() {
  private var receiver: BroadcastReceiver? = null

  private fun context(): Context? = appContext.reactContext?.applicationContext

  private fun restrictions(): Map<String, Any?> {
    val ctx = context() ?: return emptyMap()
    val manager = ctx.getSystemService(Context.RESTRICTIONS_SERVICE) as? RestrictionsManager ?: return emptyMap()
    return toMap(manager.applicationRestrictions ?: Bundle.EMPTY)
  }

  /** Strings, booleans and integers as they are; anything else (bundles, arrays) is dropped. */
  private fun toMap(bundle: Bundle): Map<String, Any?> {
    val out = HashMap<String, Any?>()
    for (key in bundle.keySet()) {
      @Suppress("DEPRECATION")
      when (val value = bundle.get(key)) {
        is String, is Boolean, is Int -> out[key] = value
        is Long -> out[key] = value.toDouble()
        else -> Unit
      }
    }
    return out
  }

  override fun definition() = ModuleDefinition {
    Name("ManagedRestrictions")
    Events("onChange")

    Function("read") { restrictions() }

    OnStartObserving {
      val ctx = context() ?: return@OnStartObserving
      if (receiver != null) return@OnStartObserving
      val r = object : BroadcastReceiver() {
        override fun onReceive(context: Context?, intent: Intent?) {
          sendEvent("onChange", restrictions())
        }
      }
      val filter = IntentFilter(Intent.ACTION_APPLICATION_RESTRICTIONS_CHANGED)
      // A protected system broadcast; NOT_EXPORTED keeps other apps from sending it to us.
      if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
        ctx.registerReceiver(r, filter, Context.RECEIVER_NOT_EXPORTED)
      } else {
        ctx.registerReceiver(r, filter)
      }
      receiver = r
    }

    OnStopObserving {
      val r = receiver ?: return@OnStopObserving
      receiver = null
      try {
        context()?.unregisterReceiver(r)
      } catch (_: IllegalArgumentException) {
        // Already gone with the context.
      }
    }
  }
}
