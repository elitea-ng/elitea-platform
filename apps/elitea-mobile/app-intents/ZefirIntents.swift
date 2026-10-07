import AppIntents
internal import ExpoAppIntents

// Agent Zefir's App Intents are OPEN-APP intents only: each one opens the app and hands a
// small invocation to JS (src/features/intents/), which opens a screen behind the device
// lock. Nothing is answered to Siri or Shortcuts: no dialog, no value, no workspace data.
// `authenticationPolicy` also asks iOS for an unlocked device before the intent runs.

struct NewChatIntent: AppIntent {
  static let title: LocalizedStringResource = "New Chat"
  static let description = IntentDescription("Opens Agent Zefir on a new chat, with the agent you pick.")
  static let openAppWhenRun: Bool = true
  static let authenticationPolicy: IntentAuthenticationPolicy = .requiresAuthentication

  @Parameter(title: "Agent")
  var agent: AgentEntity?

  static var parameterSummary: some ParameterSummary {
    Summary("New chat with \(\.$agent)")
  }

  @MainActor
  func perform() async throws -> some IntentResult {
    var params: AppIntentParams = [:]
    if let agent { params["agentId"] = .string(agent.id) }
    await AppIntentDispatcher.shared.dispatch(name: "newChat", params: params)
    return .result()
  }
}

// "Ask Agent" never makes Siri collect the question: a required String (or a
// requestValueDialog) has Siri ask "What do you want to ask?" and transcribe the spoken
// answer with its own recogniser, which may run on Apple's servers, and the app's speech
// is on the device only (src/features/speech/dictation.ts). By voice it opens the composer
// empty for the user to type or dictate (on the device); a question the user typed into a
// Shortcut still reaches the composer (scripts/app-intents-no-siri-dictation.test.mjs).
struct AskAgentIntent: AppIntent {
  static let title: LocalizedStringResource = "Ask Agent"
  static let description = IntentDescription(
    "Opens Agent Zefir on a new chat, with the question from your shortcut in the composer. Nothing is sent until you tap Send."
  )
  static let openAppWhenRun: Bool = true
  static let authenticationPolicy: IntentAuthenticationPolicy = .requiresAuthentication

  @Parameter(title: "Agent")
  var agent: AgentEntity?

  @Parameter(title: "Question", description: "Typed text for the composer. Siri never asks for it.")
  var question: String?

  static var parameterSummary: some ParameterSummary {
    Summary("Ask \(\.$agent) \(\.$question)")
  }

  @MainActor
  func perform() async throws -> some IntentResult {
    var params: AppIntentParams = [:]
    if let question { params["text"] = .string(question) }
    if let agent { params["agentId"] = .string(agent.id) }
    await AppIntentDispatcher.shared.dispatch(name: "askAgent", params: params)
    return .result()
  }
}

struct OpenNotificationsIntent: AppIntent {
  static let title: LocalizedStringResource = "Open Notifications"
  static let description = IntentDescription("Opens your Agent Zefir notifications.")
  static let openAppWhenRun: Bool = true
  static let authenticationPolicy: IntentAuthenticationPolicy = .requiresAuthentication

  @MainActor
  func perform() async throws -> some IntentResult {
    await AppIntentDispatcher.shared.dispatch(name: "openNotifications")
    return .result()
  }
}

struct ZefirShortcuts: AppShortcutsProvider {
  static var appShortcuts: [AppShortcut] {
    AppShortcut(
      intent: NewChatIntent(),
      phrases: [
        "New chat in \(.applicationName)",
        "Start a chat in \(.applicationName)",
        "New chat with \(\.$agent) in \(.applicationName)",
      ],
      shortTitle: "New Chat",
      systemImageName: "square.and.pencil"
    )
    AppShortcut(
      intent: AskAgentIntent(),
      phrases: [
        "Ask \(.applicationName)",
        "Ask \(\.$agent) in \(.applicationName)",
      ],
      shortTitle: "Ask Agent",
      systemImageName: "text.bubble"
    )
    AppShortcut(
      intent: OpenNotificationsIntent(),
      phrases: [
        "Open \(.applicationName) notifications",
        "Show my notifications in \(.applicationName)",
      ],
      shortTitle: "Notifications",
      systemImageName: "bell"
    )
  }
}
