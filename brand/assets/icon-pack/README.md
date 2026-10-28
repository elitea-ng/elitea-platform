# Elitea AI — LE-03 icon pack

Mark: LE-03 dot middle — top and bottom layers full, middle layer split into a bar + separate square dot. E is guessed, not literal. Avoids Ericsson (three parallel bars, no spine/offset).

## Variants (4)
- colored-light — cyan #00BFE6 / violet #7A5CFF / dot orange #FF6B1A / indigo #4A4DE8, on white
- colored-dark — same colours, on dark navy #0B0B18
- bw — black on white
- inverted — white on dark navy #0B0B18

## Per variant
- macos/ — squircle PNGs 1024, 512, 256, 128, 64, 32, 16 (1024 is the master, for Tauri / AppIcon)
- windows/ — elitea-<variant>.ico (256, 128, 64, 48, 32, 24, 16) + individual PNGs
- android/ — adaptive icon foreground 432 (transparent, safe-zone sized) + background 432 (solid), + legacy launcher PNGs 512, 192, 144, 96, 72, 48
- ios/ — square PNGs 1024, 180, 120 (system rounds the corners)
- github/ — avatar 1024 + 500, social preview 1280x640 (mark centred, no text)
- web/ — favicon / PWA PNGs 512, 192, 32, 16

## Recommended picks
- Desktop / GitHub org: colored-dark (pops on dark, matches next.elitea.ai)
- Light backgrounds / print: colored-light or bw
- Android adaptive: use android-foreground-432 + android-background-432 in res/mipmap-anydpi-v26/ic_launcher.xml
