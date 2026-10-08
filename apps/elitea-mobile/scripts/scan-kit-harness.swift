// macOS harness for native-modules/DocumentScanKit.swift (the document scan's PDF and
// on-device text recognition), so they can be checked without a device or the simulator:
//
//   swiftc -O -parse-as-library -o /tmp/scan-kit native-modules/DocumentScanKit.swift scripts/scan-kit-harness.swift
//   /tmp/scan-kit /tmp/scan.pdf assets/dev/scan/page-1.jpg assets/dev/scan/page-2.jpg assets/dev/scan/page-3.jpg
//
// Writes the PDF with its invisible text layer, reads it back with PDFKit (as a text
// extractor would) and fails unless every page's recognised text is in it.
import Foundation
import PDFKit

@main
struct ScanKitHarness {
  static func main() {
    let args = CommandLine.arguments
    guard args.count >= 3 else {
      FileHandle.standardError.write("usage: scan-kit <out.pdf> <page>...\n".data(using: .utf8)!)
      exit(2)
    }
    let output = URL(fileURLWithPath: args[1])
    let pages = args.dropFirst(2).map { URL(fileURLWithPath: $0) }

    do {
      let recognised = try pages.map { try DocumentScanKit.recognise(DocumentScanKit.loadImage($0)) }
      // As the app does it: recognised once, the layout kept as JSON and read back for each
      // size step.
      let layout = try DocumentScanKit.encodeLayout(recognised)
      let lines = try DocumentScanKit.decodeLayout(layout)
      guard lines == recognised else {
        print("FAIL: the layout does not survive its JSON round trip")
        exit(1)
      }
      for (edge, quality) in [(1024, 0.35), (2048, 0.7)] {
        try DocumentScanKit.writePdf(pages: pages, lines: lines, to: output, maxEdge: edge, quality: quality)
      }
      let size = (try FileManager.default.attributesOfItem(atPath: output.path)[.size] as? NSNumber)?.intValue ?? 0
      guard let document = PDFDocument(url: output), document.pageCount == pages.count else {
        print("FAIL: the PDF does not have \(pages.count) pages")
        exit(1)
      }
      print("PDF \(output.path): \(document.pageCount) pages, \(size) bytes")
      var failed = false
      for (index, pageLines) in lines.enumerated() {
        let recognised = DocumentScanKit.text(of: pageLines)
        let extracted = document.page(at: index)?.string ?? ""
        print("--- page \(index + 1): recognised \(pageLines.count) lines ---\n\(recognised)")
        let squash = { (s: String) in s.components(separatedBy: .whitespacesAndNewlines).joined() }
        if recognised.isEmpty || !squash(extracted).contains(squash(pageLines.first?.text ?? "")) || squash(extracted).count < squash(recognised).count * 9 / 10 {
          print("FAIL: page \(index + 1)'s text layer does not carry the recognised text:\n\(extracted)")
          failed = true
        }
      }
      // The "Text only" file: written whole, with Complete protection asked for.
      let textFile = output.deletingPathExtension().appendingPathExtension("txt")
      let text = lines.map(DocumentScanKit.text(of:)).joined(separator: "\n\n")
      let written = try DocumentScanKit.writeProtectedText(text, to: textFile)
      if written != Data(text.utf8).count || (try? String(contentsOf: textFile, encoding: .utf8)) != text {
        print("FAIL: the text file was not written whole")
        failed = true
      }
      exit(failed ? 1 : 0)
    } catch {
      print("FAIL: \(error.localizedDescription)")
      exit(1)
    }
  }
}
