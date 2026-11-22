#!/usr/bin/env bash
# macos-dmg.sh — the desktop app's direct download: a styled disk image (owner
# decision 2026-11-22). Opening it shows one fixed window: the brand's background,
# the app on the left, an alias of Applications on the right, an arrow between.
# The volume is named `<Brand> <version>` and carries the brand's icon.
#
#   macos-dmg.sh <path/to/App.app> <inputs-dir> <out.dmg>
#
# <inputs-dir> is what scripts/dmg-settings.mjs wrote: settings.json, background.png,
# background@2x.png and volume.icns, all from the brand.
#
# WHY NOT TAURI'S DMG BUNDLER (bundle.macOS.dmg). It names the volume after the
# product alone (no version), cannot set the icon size, skips the styling when CI=true
# (tauri#592) and does not notarise the image. Its script (a create-dmg fork) styles
# through Finder too, so it would not be more deterministic.
#
# DETERMINISTIC STYLING. The layout lives in the .DS_Store Finder writes, and Finder
# is driven by AppleScript, which a headless runner can time out. So: the AppleScript
# runs up to three times, each attempt waits for the .DS_Store, and dmg-verify.mjs
# reads the record back (the background alias, the window, both icon positions) before
# the image is sealed. A layout that did not apply fails the build; it never ships
# plain.
#
# Signed when DMG_SIGNING_IDENTITY is set ("Developer ID Application: <Name> (<TEAMID>)"):
# the identity comes from APPLE_CERTIFICATE (with APPLE_CERTIFICATE_PASSWORD) through a
# temporary keychain (macos-keychain.sh); the image is codesigned with a secure
# timestamp, notarised (notarytool, App Store Connect key: APPLE_API_KEY,
# APPLE_API_ISSUER, APPLE_API_KEY_PATH) and stapled. Unset: an unsigned image (the
# pull request's dry run, which still proves the styling).
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=macos-keychain.sh
. "$here/macos-keychain.sh"

app="${1:?usage: macos-dmg.sh <App.app> <inputs-dir> <out.dmg>}"
inputs="${2:?inputs-dir}"
dmg="${3:?out.dmg}"

[ -d "$app" ] || { echo "::error::no app bundle at $app" >&2; exit 1; }
settings="$inputs/settings.json"
setting() { node -e 'const s=require(process.argv[1]); const v=process.argv[2].split(".").reduce((o,k)=>o[k],s); process.stdout.write(String(v))' "$settings" "$1"; }
vol="$(setting volumeName)"
app_name="$(basename "$app")"
if [ "$app_name" != "$(setting app)" ]; then
  echo "::error::the app is $app_name, the brand's settings name $(setting app)" >&2; exit 1
fi
# Finder addresses the volume by name: another mounted volume of that name would get the layout.
if [ -e "/Volumes/$vol" ]; then
  echo "::error::a volume named \"$vol\" is mounted already; eject it first" >&2; exit 1
fi

work="$(mktemp -d)"
device=""
detach() {
  local i
  for i in 1 2 3 4 5; do
    hdiutil detach "$1" -quiet && return 0
    sleep "$i"
  done
  hdiutil detach "$1" -force -quiet
}
cleanup() {
  if [ -n "$device" ]; then detach "$device" || true; fi
  macos_keychain_delete
  rm -rf "$work"
}
trap cleanup EXIT

# The volume's content: the app, the Applications alias, the hidden background and icon.
stage="$work/stage"
mkdir -p "$stage/.background"
ditto "$app" "$stage/$app_name"
ln -s /Applications "$stage/Applications"
# One TIFF holding the 1x and @2x pictures: Finder picks the one for the display.
tiffutil -cathidpicheck "$inputs/background.png" "$inputs/background@2x.png" -out "$stage/.background/background.tiff" >/dev/null 2>&1

# A writable image with room for Finder's .DS_Store; HFS+, whose aliases Finder resolves
# for the background picture.
megabytes=$(( $(du -sm "$stage" | cut -f1) + 32 ))
hdiutil create -quiet -srcfolder "$stage" -volname "$vol" -fs HFS+ -fsargs "-c c=64,a=16,e=16" \
  -format UDRW -size "${megabytes}m" "$work/rw.dmg"
attach_out="$(hdiutil attach -readwrite -noverify -noautoopen "$work/rw.dmg")"
device="$(printf '%s\n' "$attach_out" | awk -F'\t' '/Apple_HFS/ { gsub(/[[:space:]]+$/, "", $1); print $1; exit }')"
mount="$(printf '%s\n' "$attach_out" | awk -F'\t' '/Apple_HFS/ { print $NF; exit }')"
[ -n "$device" ] && [ -d "$mount" ] || { echo "::error::the writable image did not mount: $attach_out" >&2; exit 1; }
[ "$mount" = "/Volumes/$vol" ] || { echo "::error::the image mounted at $mount, not /Volumes/$vol" >&2; exit 1; }

width="$(setting window.width)"
height="$(setting window.height)"
# The window's bounds are its frame: the title bar sits above the picture.
titlebar=28
left=200
top=120
script="$work/layout.applescript"
cat > "$script" <<'APPLESCRIPT'
on run argv
  set {volName, appName, l, t, r, b, iconSz, textSz, ax, ay, fx, fy} to argv
  with timeout of 180 seconds
    tell application "Finder"
      tell disk volName
        open
        set current view of container window to icon view
        set toolbar visible of container window to false
        set statusbar visible of container window to false
        set the bounds of container window to {l as integer, t as integer, r as integer, b as integer}
        set opts to the icon view options of container window
        set arrangement of opts to not arranged
        set icon size of opts to iconSz as integer
        set text size of opts to textSz as integer
        set label position of opts to bottom
        set shows item info of opts to false
        set shows icon preview of opts to false
        set background picture of opts to file ".background:background.tiff"
        set position of item appName of container window to {ax as integer, ay as integer}
        set position of item "Applications" of container window to {fx as integer, fy as integer}
        close
        open
        update without registering applications
        delay 2
        close
      end tell
    end tell
  end timeout
end run
APPLESCRIPT

styled=false
for attempt in 1 2 3; do
  if osascript "$script" "$vol" "$app_name" "$left" "$top" "$((left + width))" "$((top + height + titlebar))" \
      "$(setting iconSize)" "$(setting textSize)" \
      "$(setting appPosition.x)" "$(setting appPosition.y)" \
      "$(setting applicationsPosition.x)" "$(setting applicationsPosition.y)"; then
    # Finder writes the .DS_Store on its own time after the window closes.
    for _ in $(seq 1 30); do
      [ -f "$mount/.DS_Store" ] && break
      sleep 1
    done
    # The volume's icon: .VolumeIcon.icns at the root with the custom-icon flag on it.
    # After Finder, which deletes an icon it finds while it lays the window out (and
    # `hdiutil create -srcfolder` leaves that name out).
    cp "$inputs/volume.icns" "$mount/.VolumeIcon.icns"
    SetFile -a C "$mount"
    sync
    if node "$here/dmg-verify.mjs" "$mount" "$settings"; then
      styled=true
      break
    fi
  fi
  echo "::warning::Finder layout attempt $attempt did not apply; retrying"
  sleep $((attempt * 5))
done
if [ "$styled" != true ]; then
  echo "::error::Finder did not apply the disk image layout in 3 attempts" >&2; exit 1
fi

# Nothing Finder or Spotlight left behind travels; the content is read-only to others.
rm -rf "$mount/.fseventsd" "$mount/.Trashes" "$mount/.Spotlight-V100"
chmod -Rf go-w "$mount" || true
sync
detach "$device"
device=""

mkdir -p "$(dirname "$dmg")"
rm -f "$dmg"
hdiutil convert -quiet "$work/rw.dmg" -format UDZO -imagekey zlib-level=9 -o "$dmg"

DMG_SIGNING_IDENTITY="$(trim_identity "${DMG_SIGNING_IDENTITY:-}")"
if [ -z "$DMG_SIGNING_IDENTITY" ]; then
  echo "unsigned disk image: $dmg"
  exit 0
fi

: "${APPLE_CERTIFICATE:?APPLE_CERTIFICATE}" "${APPLE_CERTIFICATE_PASSWORD:?APPLE_CERTIFICATE_PASSWORD}"
macos_keychain_import "$work" "$APPLE_CERTIFICATE" "$APPLE_CERTIFICATE_PASSWORD" "$DMG_SIGNING_IDENTITY" APPLE_SIGNING_IDENTITY "Developer ID Application" /usr/bin/codesign
codesign --sign "$DMG_SIGNING_IDENTITY" --keychain "$MACOS_KEYCHAIN" --timestamp "$dmg"

: "${APPLE_API_KEY:?APPLE_API_KEY}" "${APPLE_API_ISSUER:?APPLE_API_ISSUER}" "${APPLE_API_KEY_PATH:?APPLE_API_KEY_PATH}"
xcrun notarytool submit "$dmg" --key "$APPLE_API_KEY_PATH" --key-id "$APPLE_API_KEY" --issuer "$APPLE_API_ISSUER" --wait --timeout 30m
xcrun stapler staple "$dmg"
echo "signed, notarised disk image: $dmg"
