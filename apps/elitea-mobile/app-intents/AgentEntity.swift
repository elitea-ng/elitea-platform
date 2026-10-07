import AppIntents
internal import ExpoAppIntents
import Foundation

/// An agent of the active workspace's selected project, for "New chat with <agent>" and
/// "Ask <agent>". The catalog is published from JS (src/features/intents/agentCatalog.ts)
/// and holds names only when the workspace policy allows titles outside the app
/// (allow_system_surfaces); otherwise it is empty and the intents open the agent picker.
/// The id is `<workspace slot>:<project id>:<agent id>`: an intent saved for one workspace
/// never opens another's agent.
struct AgentEntity: AppEntity {
  static let typeDisplayRepresentation: TypeDisplayRepresentation = "Agent"
  static let defaultQuery = AgentQuery()

  var id: String
  @Property(title: "Name")
  var name: String

  var displayRepresentation: DisplayRepresentation {
    DisplayRepresentation(title: "\(name)")
  }

  init(record: AppIntentEntityRecord) {
    self.id = record.id
    self.name = record.title
  }

  func matches(_ search: String) -> Bool {
    let needle = search.folding(options: [.caseInsensitive, .diacriticInsensitive], locale: .current)
      .trimmingCharacters(in: .whitespacesAndNewlines)
    guard !needle.isEmpty else { return false }
    let hay = name.folding(options: [.caseInsensitive, .diacriticInsensitive], locale: .current)
    return hay.contains(needle) || needle.contains(hay)
  }
}

struct AgentQuery: EntityStringQuery {
  func entities(for identifiers: [String]) async throws -> [AgentEntity] {
    try await AppIntentEntityStore.shared.entities(ofKind: "agent", matching: identifiers)
      .map(AgentEntity.init(record:))
  }

  func suggestedEntities() async throws -> [AgentEntity] {
    try await AppIntentEntityStore.shared.entities(ofKind: "agent").map(AgentEntity.init(record:))
  }

  func entities(matching string: String) async throws -> [AgentEntity] {
    try await suggestedEntities().filter { $0.matches(string) }
  }
}
