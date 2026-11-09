package app.managed

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.content.RestrictionsManager
import android.os.Build
import android.os.Bundle
import androidx.enterprise.feedback.KeyedAppState
import androidx.enterprise.feedback.KeyedAppStatesReporter
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
 * - `reportStates(states)` sends MDM feedback (W6.1, src/security/managedFeedback.ts) as
 *   keyed app states to the device or profile owner and Play. The JS side builds every
 *   state from fixed codes and never passes user content; this side only clips to the
 *   API's limits (key 100, message and data 1000 characters).
 *
 * Nothing is cached here: Android keeps the restrictions and the reported states.
 */
class ManagedRestrictions : Module() {
  private var receiver: BroadcastReceiver? = null
  /** One reporter for the app: it keeps its bindings (KeyedAppStatesReporter.create). */
  private var reporter: KeyedAppStatesReporter? = null

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
    Name("ManagedRestrictions")
    Events("onChange")

    Function("read") { restrictions() }

    Function("reportStates") { states: List<Map<String, Any?>> ->
      val ctx = context() ?: return@Function
      val r = reporter ?: KeyedAppStatesReporter.create(ctx).also { reporter = it }
      val built = states.mapNotNull { s ->
        val key = (s["key"] as? String)?.take(100) ?: return@mapNotNull null
        KeyedAppState.builder()
          .setKey(key)
          .setSeverity(if (s["severity"] == "error") KeyedAppState.SEVERITY_ERROR else KeyedAppState.SEVERITY_INFO)
          .setMessage((s["message"] as? String)?.take(1000))
          .setData((s["data"] as? String)?.take(1000))
          .build()
      }
      if (built.isNotEmpty()) r.setStates(built, null)
    }

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

    OnStopObserving { unregister() }

    // A JS reload destroys the module without OnStopObserving: never leave the receiver behind.
    OnDestroy { unregister() }
  }
}
