internal import ExpoModulesCore
import Foundation
import Network

/// Expo inline module (app.config.ts `experiments.inlineModules`): the system's low-data
/// state, which `expo-network` does not expose (ADR-0050 decision 6, #1650). Do not rename:
/// the class name, the file name and JS `requireOptionalNativeModule('DataSaver')` must agree.
///
/// - `isLowData()` is true while the current network path is constrained (Low Data Mode) or
///   expensive (cellular, a personal hotspot): NWPath.isConstrained || isExpensive.
/// - `onChange` fires with `{ lowData }` when that answer changes, while JS listens.
///
/// The monitor starts when JS first listens or asks and stops with the last listener; it
/// reads no traffic and holds no state beyond the last answer.
final class DataSaver: Module {
  private let queue = DispatchQueue(label: "app.datasaver.path")
  private var monitor: NWPathMonitor?
  private var lowData = false
  private var observing = false

  private func start() {
    if monitor != nil { return }
    let monitor = NWPathMonitor()
    self.monitor = monitor
    // The first callback arrives at once with the current path.
    monitor.pathUpdateHandler = { [weak self] path in
      guard let self else { return }
      let next = path.isConstrained || path.isExpensive
      let changed = next != self.lowData
      self.lowData = next
      if changed && self.observing {
        self.sendEvent("onChange", ["lowData": next])
      }
    }
    monitor.start(queue: queue)
    // A synchronous read right after start: the path is already known to the monitor.
    let path = monitor.currentPath
    lowData = path.isConstrained || path.isExpensive
  }

  private func stop() {
    monitor?.cancel()
    monitor = nil
  }

  func definition() -> ModuleDefinition {
    Name("DataSaver")
    Events("onChange")

    Function("isLowData") { () -> Bool in
      self.start()
      return self.lowData
    }

    OnStartObserving {
      self.observing = true
      self.start()
    }

    OnStopObserving {
      self.observing = false
      self.stop()
    }

    OnDestroy { self.stop() }
  }
}
