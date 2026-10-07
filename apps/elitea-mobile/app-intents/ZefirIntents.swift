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

struct AskAgentIntent: AppIntent {
  static let title: LocalizedStringResource = "Ask Agent"
  static let description = IntentDescription(
    "Opens Agent Zefir with your question in the composer. Nothing is sent until you tap Send."
  )
  static let openAppWhenRun: Bool = true
  static let authenticationPolicy: IntentAuthenticationPolicy = .requiresAuthentication

  @Parameter(title: "Agent")
  var agent: AgentEntity?

  @Parameter(title: "Question", requestValueDialog: "What do you want to ask?")
  var question: String

  static var parameterSummary: some ParameterSummary {
    Summary("Ask \(\.$agent) \(\.$question)")
  }

  @MainActor
  func perform() async throws -> some IntentResult {
    var params: AppIntentParams = ["text": .string(question)]
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
