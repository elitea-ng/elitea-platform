// Usage: swift cutout.swift <input> <outPrefix>
// Writes <outPrefix>-mask.png (soft mask, grayscale) and <outPrefix>-instances.txt
import Foundation
import Vision
import CoreImage
import CoreImage.CIFilterBuiltins
import ImageIO
import UniformTypeIdentifiers

let args = CommandLine.arguments
let input = URL(fileURLWithPath: args[1])
let prefix = args[2]
let ctx = CIContext()
guard let ci = CIImage(contentsOf: input) else { fatalError("cannot read") }
let handler = VNImageRequestHandler(url: input)
let req = VNGenerateForegroundInstanceMaskRequest()
try handler.perform([req])
guard let obs = req.results?.first else { fatalError("no foreground") }
print("instances:", obs.allInstances.map { $0 })
func write(_ img: CIImage, _ path: String) {
  let url = URL(fileURLWithPath: path)
  try! ctx.writePNGRepresentation(of: img, to: url, format: .RGBA8, colorSpace: CGColorSpace(name: CGColorSpace.sRGB)!)
}
// all instances combined
let all = try obs.generateScaledMaskForImage(forInstances: obs.allInstances, from: handler)
write(CIImage(cvPixelBuffer: all), prefix + "-mask-all.png")
for i in obs.allInstances {
  let m = try obs.generateScaledMaskForImage(forInstances: [i], from: handler)
  write(CIImage(cvPixelBuffer: m), prefix + "-mask-\(i).png")
}
