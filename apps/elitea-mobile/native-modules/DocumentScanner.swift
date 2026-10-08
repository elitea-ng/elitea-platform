internal import ExpoModulesCore
import Foundation
import UIKit
import VisionKit

/// Expo inline module (app.config.ts `experiments.inlineModules`): document scanning for
/// attachments (src/features/chat/attachments/scan/). Do not rename: the class name, the
/// file name and the JS `requireOptionalNativeModule('DocumentScanner')` must agree.
///
/// - `scan` presents VisionKit's document camera and writes each page as a JPEG into a
///   directory the app passes (inside the app's Caches directory). Nothing goes to the photo
///   library: VisionKit hands the pages to the app only.
/// - `recognizeLayout` recognises the pages' text ON THE DEVICE once per scan, and returns it
///   per page plus its layout (lines and boxes, as JSON) for `makePdf`.
/// - `makePdf` writes the pages into one PDF, with that layout as an invisible text layer.
///   It is called once per size step; the layout is reused, never recognised again.
/// - `recognizeText` returns each page's text, recognised on the device (Vision has no server
///   mode).
/// - `writeText` writes the recognised text file ("Text only") with Complete protection, like
///   the pages and the PDF (expo-file-system would give it the default class).
///
/// Every path must lie inside the app's Caches directory.
final class DocumentScanner: Module {
  private var session: ScanSession?

  func definition() -> ModuleDefinition {
    Name("DocumentScanner")

    /// VisionKit's own answer. The iOS 27 simulator says true, then fails to capture
    /// (AVFoundation -11800), hence `isSimulator`.
    Constant("isSupported") {
      VNDocumentCameraViewController.isSupported
    }

    Constant("isSimulator") {
      runsOnSimulator
    }

    AsyncFunction("scan") { (directory: String, promise: Promise) in
      try self.present(into: directory, promise: promise)
    }.runOnQueue(.main)

    AsyncFunction("recognizeLayout") { (pages: [String]) throws -> [String: Any] in
      do {
        let lines = try pages.map { try DocumentScanKit.recognise(DocumentScanKit.loadImage(try cachesURL($0))) }
        return ["layout": try DocumentScanKit.encodeLayout(lines), "text": lines.map(DocumentScanKit.text(of:))]
      } catch let error as ScanKitError {
        throw ScanException(error.message)
      }
    }

    AsyncFunction("makePdf") { (pages: [String], output: String, maxEdge: Int, quality: Double, layout: String?) throws -> [String: Any] in
      do {
        let urls = try pages.map(cachesURL)
        let out = try cachesURL(output)
        try FileManager.default.createDirectory(at: out.deletingLastPathComponent(), withIntermediateDirectories: true)
        let lines = try layout.map(DocumentScanKit.decodeLayout)
        try DocumentScanKit.writePdf(pages: urls, lines: lines, to: out, maxEdge: maxEdge, quality: quality)
        try FileManager.default.setAttributes([.protectionKey: FileProtectionType.complete], ofItemAtPath: out.path)
        // The size only (a resource value, not a file-timestamp API).
        let size = try out.resourceValues(forKeys: [.fileSizeKey]).fileSize ?? 0
        return ["uri": out.absoluteString, "size": size]
      } catch let error as ScanKitError {
        throw ScanException(error.message)
      }
    }

    AsyncFunction("writeText") { (output: String, text: String) throws -> Int in
      do {
        return try DocumentScanKit.writeProtectedText(text, to: try cachesURL(output))
      } catch let error as ScanKitError {
        throw ScanException(error.message)
      }
    }

    AsyncFunction("recognizeText") { (pages: [String]) throws -> [String] in
      do {
        return try pages.map { DocumentScanKit.text(of: try DocumentScanKit.recognise(DocumentScanKit.loadImage(try cachesURL($0)))) }
      } catch let error as ScanKitError {
        throw ScanException(error.message)
      }
    }
  }
}

extension DocumentScanner {
  /// Shows the document camera (main queue); `promise` settles once it is dismissed.
  fileprivate func present(into directory: String, promise: Promise) throws {
    guard session == nil else { throw ScanException("A scan is already open.") }
    guard VNDocumentCameraViewController.isSupported else { throw ScanException("This device cannot scan documents.") }
    guard !runsOnSimulator else { throw ScanException("The simulator has no camera to scan with.") }
    let target = try cachesURL(directory)
    guard let presenter = appContext?.utilities?.currentViewController() else {
      throw ScanException("There is no screen to show the scanner on.")
    }
    let camera = VNDocumentCameraViewController()
    let session = ScanSession(directory: target) { [weak self, weak camera] result in
      self?.session = nil
      let settle = {
        switch result {
        case .success(let pages): promise.resolve(pages)
        case .failure(let error): promise.reject(ScanException(error.localizedDescription))
        }
      }
      if let camera { camera.dismiss(animated: true, completion: settle) } else { settle() }
    }
    self.session = session
    camera.delegate = session
    camera.modalPresentationStyle = .fullScreen
    presenter.present(camera, animated: true)
  }
}

#if targetEnvironment(simulator)
  private let runsOnSimulator = true
#else
  private let runsOnSimulator = false
#endif

private final class ScanException: Exception, @unchecked Sendable {
  private let message: String
  init(_ message: String) {
    self.message = message
    super.init()
  }
  override var reason: String { message }
}

/// A file URL (or path) the module may touch: inside the app's Caches directory only.
private func cachesURL(_ value: String) throws -> URL {
  let parsed = value.hasPrefix("file://") ? URL(string: value) : URL(fileURLWithPath: value)
  let caches = FileManager.default.urls(for: .cachesDirectory, in: .userDomainMask)[0].standardizedFileURL.path
  guard let url = parsed?.standardizedFileURL, url.isFileURL, url.path.hasPrefix(caches + "/") else {
    throw ScanException("Scans are kept in the app's caches only.")
  }
  return url
}

/// The document camera's delegate: writes the pages, then hands back their file URLs.
private final class ScanSession: NSObject, VNDocumentCameraViewControllerDelegate {
  private let directory: URL
  private let finish: (Result<[String], Error>) -> Void

  init(directory: URL, finish: @escaping (Result<[String], Error>) -> Void) {
    self.directory = directory
    self.finish = finish
  }

  func documentCameraViewController(_ controller: VNDocumentCameraViewController, didFinishWith scan: VNDocumentCameraScan) {
    do {
      try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
      var pages: [String] = []
      for index in 0..<scan.pageCount {
        guard let data = scan.imageOfPage(at: index).jpegData(compressionQuality: 0.9) else {
          throw ScanKitError("A scanned page could not be saved.")
        }
        let url = directory.appendingPathComponent("page-\(index + 1).jpg")
        // Complete protection: unreadable while the device is locked.
        try data.write(to: url, options: [.atomic, .completeFileProtection])
        pages.append(url.absoluteString)
      }
      finish(.success(pages))
    } catch {
      finish(.failure(error))
    }
  }

  func documentCameraViewControllerDidCancel(_ controller: VNDocumentCameraViewController) {
    finish(.success([]))
  }

  func documentCameraViewController(_ controller: VNDocumentCameraViewController, didFailWithError error: Error) {
    finish(.failure(error))
  }
}
