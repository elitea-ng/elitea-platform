import Foundation

import SwiftUI

/// A user of the app.
/// Users are identified by `id`.
public struct User: Codable, Identifiable, Equatable {
    public let id: UUID
    var name: String
    private(set) var email: String?
    static let guest = User(id: UUID(), name: "Guest", email: nil)

    var displayName: String {
        name.isEmpty ? "Anonymous" : name
    }
}

/// Where a payment comes from.
enum PaymentMethod: String, CaseIterable {
    case card
    case cash
}

indirect enum Expr {
    case value(Int)
    case add(Expr, Expr)
}

@objc
open class BaseController: NSObject, UITableViewDelegate {
    @IBOutlet weak var tableView: UITableView!
    lazy var formatter: DateFormatter = DateFormatter()

    open override func viewDidLoad() {
        super.viewDidLoad()
        tableView.reloadData()
    }
}

final class ProfileController: BaseController, ProfileViewDelegate where Self: Sendable {
    private let service: UserService

    init(service: UserService) {
        self.service = service
        super.init()
    }

    @MainActor
    func render(user: User) async throws -> String {
        let summary = Summary(user: user)
        return summary.text.uppercased()
    }

    class func make() -> ProfileController {
        return ProfileController(service: UserService.shared)
    }
}

typealias Completion<T> = (Result<T, Error>) -> Void
public typealias UserID = UUID
