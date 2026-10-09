import Combine
import struct Foundation.URL

protocol UserService: AnyObject {
    associatedtype Output
    func fetch(id: UUID) async throws -> User
    var cacheSize: Int { get }
}

public protocol Cache {
}

actor SessionStore: ObservableObject {
    private var sessions: [UUID: Session] = [:]

    func store(_ session: Session) {
        sessions[session.id] = session
        Logger.shared.log("stored \(session.id)")
    }

    nonisolated func count() -> Int { 0 }
}

extension User: CustomStringConvertible {
    var description: String { "User(\(name))" }

    static func random() -> User {
        return User(id: UUID(), name: "rnd", email: nil)
    }
}

extension Array where Element == User {
    func sortedByName() -> [User] {
        return sorted { $0.name < $1.name }.filter { !$0.name.isEmpty }
    }
}

@propertyWrapper
struct Clamped<Value: Comparable> {
    var wrappedValue: Value
    let range: ClosedRange<Value>
}

struct Settings {
    @Clamped(wrappedValue: 5, range: 0...10) var volume: Int
    @Published var theme: String = "light"
}

final class RemoteUserService: UserService, Cache {
    typealias Output = User
    private let session: URLSession
    var cacheSize: Int { return 10 }

    func fetch(id: UUID) async throws -> User {
        let request = APIRequest(path: "/users/\(id)")
        let (data, _) = try await session.data(for: request.urlRequest())
        return try JSONDecoder().decode(User.self, from: data)
    }

    private static func backoff(attempt: Int) -> TimeInterval {
        return pow(2.0, Double(attempt))
    }
}
