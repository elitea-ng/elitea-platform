import CoreGraphics
import CoreText
import Foundation
import ImageIO
import UniformTypeIdentifiers
import Vision

// The document scan pipeline's native half, used by the DocumentScanner inline module
// (DocumentScanner.swift). Core Graphics, Image I/O and Vision only, no UIKit: the same file
// runs in a macOS harness (scripts/scan-kit-harness.swift) so the PDF and the text layer can
// be checked without a device.

struct ScanKitError: LocalizedError {
  let message: String
  init(_ message: String) { self.message = message }
  var errorDescription: String? { message }
}

/// One line of recognised text; `box` is normalised (0...1), origin at the bottom left (as
/// Vision reports it, and as a PDF page draws).
struct RecognisedLine: Codable, Equatable {
  let text: String
  let box: CGRect
}

enum DocumentScanKit {
  /// US Letter width in points; each page keeps its own aspect ratio.
  static let pageWidth: CGFloat = 612

  /// Text recognition ON THE DEVICE (Vision has no server mode): the page's lines, top to
  /// bottom, left to right.
  static func recognise(_ image: CGImage) throws -> [RecognisedLine] {
    let request = VNRecognizeTextRequest()
    request.recognitionLevel = .accurate
    request.usesLanguageCorrection = true
    if #available(iOS 16.0, macOS 13.0, *) {
      request.automaticallyDetectsLanguage = true
    }
    try VNImageRequestHandler(cgImage: image, options: [:]).perform([request])
    let lines = (request.results ?? []).compactMap { observation -> RecognisedLine? in
      guard let text = observation.topCandidates(1).first?.string, !text.isEmpty else { return nil }
      return RecognisedLine(text: text, box: observation.boundingBox)
    }
    // Reading order: rows from the top (a row is lines whose middles are within half a line
    // height of each other), each row left to right.
    return lines.sorted { a, b in
      let tolerance = min(a.box.height, b.box.height) / 2
      if abs(a.box.midY - b.box.midY) > tolerance { return a.box.midY > b.box.midY }
      return a.box.minX < b.box.minX
    }
  }

  static func text(of lines: [RecognisedLine]) -> String {
    lines.map(\.text).joined(separator: "\n")
  }

  /// Every page's lines as JSON: recognised once per scan, then handed back to `writePdf` for
  /// each size step (JS keeps it opaque), so a scan that needs several steps is not recognised
  /// again per step.
  static func encodeLayout(_ pages: [[RecognisedLine]]) throws -> String {
    guard let json = String(data: try JSONEncoder().encode(pages), encoding: .utf8) else {
      throw ScanKitError("The recognised text could not be kept.")
    }
    return json
  }

  static func decodeLayout(_ json: String) throws -> [[RecognisedLine]] {
    do {
      return try JSONDecoder().decode([[RecognisedLine]].self, from: Data(json.utf8))
    } catch {
      throw ScanKitError("The recognised text could not be read back.")
    }
  }

  /// Writes the recognised text as UTF-8 with Complete protection (unreadable while the device
  /// is locked), as the pages and the PDF are; answers its size in bytes.
  static func writeProtectedText(_ text: String, to url: URL) throws -> Int {
    let data = Data(text.utf8)
    do {
      try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
      try data.write(to: url, options: [.atomic, .completeFileProtection])
    } catch {
      throw ScanKitError("The recognised text could not be saved.")
    }
    return data.count
  }

  static func loadImage(_ url: URL) throws -> CGImage {
    guard let source = CGImageSourceCreateWithURL(url as CFURL, nil),
      let image = CGImageSourceCreateImageAtIndex(source, 0, [kCGImageSourceShouldCache: true] as CFDictionary)
    else {
      throw ScanKitError("A scanned page could not be read.")
    }
    return image
  }

  /// The page fitted to `maxEdge` px and re-encoded as JPEG. A CGImage backed by JPEG data is
  /// embedded in a PDF as DCT data, so the PDF stays the size of its JPEGs.
  static func jpegPage(_ url: URL, maxEdge: Int, quality: Double) throws -> CGImage {
    guard let source = CGImageSourceCreateWithURL(url as CFURL, nil) else {
      throw ScanKitError("A scanned page could not be read.")
    }
    let options: [CFString: Any] = [
      kCGImageSourceCreateThumbnailFromImageAlways: true,
      kCGImageSourceCreateThumbnailWithTransform: true,
      kCGImageSourceThumbnailMaxPixelSize: maxEdge,
    ]
    guard let fitted = CGImageSourceCreateThumbnailAtIndex(source, 0, options as CFDictionary) else {
      throw ScanKitError("A scanned page could not be encoded.")
    }
    let data = NSMutableData()
    guard let destination = CGImageDestinationCreateWithData(data, UTType.jpeg.identifier as CFString, 1, nil) else {
      throw ScanKitError("A scanned page could not be encoded.")
    }
    // Only the pixels: no EXIF, GPS or other metadata is written.
    CGImageDestinationAddImage(destination, fitted, [kCGImageDestinationLossyCompressionQuality: quality] as CFDictionary)
    guard CGImageDestinationFinalize(destination),
      let provider = CGDataProvider(data: data as CFData),
      let jpeg = CGImage(jpegDataProviderSource: provider, decode: nil, shouldInterpolate: true, intent: .defaultIntent)
    else {
      throw ScanKitError("A scanned page could not be encoded.")
    }
    return jpeg
  }

  /// Writes the pages into one PDF. With `lines` (one array per page), each page carries its
  /// recognised text as an invisible text layer over the image, so the workspace can read the
  /// PDF as text (a scanned page with no text layer reaches the model as nothing).
  static func writePdf(pages: [URL], lines: [[RecognisedLine]]?, to output: URL, maxEdge: Int, quality: Double) throws {
    guard !pages.isEmpty else { throw ScanKitError("There are no pages to save.") }
    var firstBox = CGRect(x: 0, y: 0, width: pageWidth, height: pageWidth * 11 / 8.5)
    // No author, creator or title (Quartz adds only its producer and the dates).
    guard let context = CGContext(output as CFURL, mediaBox: &firstBox, [:] as CFDictionary) else {
      throw ScanKitError("The PDF could not be created.")
    }
    defer { context.closePDF() }
    for (index, page) in pages.enumerated() {
      let image = try jpegPage(page, maxEdge: maxEdge, quality: quality)
      let height = (pageWidth * CGFloat(image.height) / CGFloat(max(image.width, 1))).rounded()
      var box = CGRect(x: 0, y: 0, width: pageWidth, height: height)
      context.beginPage(mediaBox: &box)
      context.interpolationQuality = .high
      context.draw(image, in: box)
      if let pageLines = lines, index < pageLines.count {
        drawTextLayer(pageLines[index], in: box, context: context)
      }
      context.endPage()
    }
  }

  /// Invisible text (render mode 3), each line stretched over its box: selectable and
  /// extractable, never drawn.
  private static func drawTextLayer(_ lines: [RecognisedLine], in page: CGRect, context: CGContext) {
    context.saveGState()
    context.setTextDrawingMode(.invisible)
    for line in lines {
      let rect = CGRect(
        x: line.box.minX * page.width,
        y: line.box.minY * page.height,
        width: line.box.width * page.width,
        height: line.box.height * page.height
      )
      guard rect.width > 1, rect.height > 1 else { continue }
      let font = CTFontCreateWithName("Helvetica" as CFString, rect.height * 0.8, nil)
      let attributed = NSAttributedString(string: line.text, attributes: [NSAttributedString.Key(kCTFontAttributeName as String): font])
      let ctLine = CTLineCreateWithAttributedString(attributed)
      let width = CTLineGetTypographicBounds(ctLine, nil, nil, nil)
      guard width > 0 else { continue }
      context.textMatrix = CGAffineTransform(scaleX: rect.width / CGFloat(width), y: 1)
      context.textPosition = CGPoint(x: rect.minX, y: rect.minY + rect.height * 0.2)
      CTLineDraw(ctLine, context)
    }
    context.restoreGState()
  }
}
