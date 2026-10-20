#!/bin/sh
# Regenerate every Agent Zefirca asset. Usage: tools/run.sh <design_dir>
set -eu
D="$1"; T="$(cd "$(dirname "$0")" && pwd)"
IC="/Applications/Xcode.app/Contents/Applications/Icon Composer.app/Contents/Executables/ictool"
mkdir -p "$D/work/ic" "$D/assets"
[ -x "$T/cutout" ] || swiftc -O "$T/cutout.swift" -o "$T/cutout"
( cd "$D/work" && "$T/cutout" "$D/mascot-zefirca-original.jpeg" work-cut )
( cd "$T" && python3 cut.py "$D" && python3 build.py "$D" )
for r in Default Dark TintedDark ClearLight; do
  "$IC" "$D/assets/icons/zefirca.icon" --export-image --output-file "$D/work/ic/$r.png" \
    --platform iOS --rendition $r --width 1024 --height 1024 --scale 1 >/dev/null
done
( cd "$T" && python3 preview.py "$D" )
