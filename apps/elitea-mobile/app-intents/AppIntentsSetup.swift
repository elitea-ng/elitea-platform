internal import ExpoAppIntents
internal import ExpoModulesCore

/// Registered Expo inline module that wires app-target App Intents code to expo-app-intents.
/// Do not change the name of this class.
final class AppIntentsSetup: Module {
  public func definition() -> ExpoModulesCore.ModuleDefinition {
    Name("AppIntentsSetup")

    OnCreate {
      // The agent catalog (AgentEntity) is published from JS; retrain the phrases when it changes.
      Task {
        await AppIntentDispatcher.shared.setShortcutsRefreshHandler {
          ZefirShortcuts.updateAppShortcutParameters()
        }
        ZefirShortcuts.updateAppShortcutParameters()
      }
    }
  }
}
