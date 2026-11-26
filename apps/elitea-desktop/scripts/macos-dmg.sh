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
# reads the record back (the background alias, the window with every bar off, both
# icon positions) before
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
# Its own "images written" line is noise; its error is not (a picture it cannot read).
if ! tiffutil -cathidpicheck "$inputs/background.png" "$inputs/background@2x.png" -out "$stage/.background/background.tiff" > "$work/tiffutil.log" 2>&1; then
  cat "$work/tiffutil.log" >&2
  echo "::error::tiffutil could not merge the brand's background.png and background@2x.png (660x400 and 1320x800 PNGs)" >&2; exit 1
fi

# A writable image with room for Finder's .DS_Store; HFS+, whose aliases Finder resolves
# for the background picture.
megabytes=$(( $(du -sm "$stage" | cut -f1) + 32 ))
# `hdiutil create` fails now and then on a hosted runner ("Resource busy" while it
# copies the source folder in), and `-quiet` closes stderr, so such a failure said
# nothing. Its output goes to a log instead, it is tried three times, and the last
# attempt's log is shown when all fail.
created=""
for attempt in 1 2 3; do
  rm -f "$work/rw.dmg"
  if hdiutil create -srcfolder "$stage" -volname "$vol" -fs HFS+ -fsargs "-c c=64,a=16,e=16" \
    -format UDRW -size "${megabytes}m" "$work/rw.dmg" > "$work/create.log" 2>&1; then
    created=1; break
  fi
  echo "hdiutil create failed (attempt $attempt of 3): $(tr '\n' ' ' < "$work/create.log")" >&2
  sleep $(( attempt * 5 ))
done
if [ -z "$created" ]; then
  cat "$work/create.log" >&2
  echo "::error::hdiutil could not create the writable image in 3 attempts" >&2; exit 1
fi
attach_out="$(hdiutil attach -readwrite -noverify -noautoopen "$work/rw.dmg")"
# The whole disk (/dev/diskN, the first line), not the HFS slice: detaching it
# detaches the image, so `hdiutil convert` below never meets a busy file.
device="$(printf '%s\n' "$attach_out" | awk 'NR == 1 { print $1; exit }')"
mount="$(printf '%s\n' "$attach_out" | awk -F'\t' '/Apple_HFS/ { print $NF; exit }')"
case "$device" in /dev/disk[0-9]*) ;; *) device="";; esac
[ -n "$device" ] && [ -d "$mount" ] || { echo "::error::the writable image did not mount: $attach_out" >&2; exit 1; }
[ "$mount" = "/Volumes/$vol" ] || { echo "::error::the image mounted at $mount, not /Volumes/$vol" >&2; exit 1; }

width="$(setting window.width)"
height="$(setting window.height)"
# The window's bounds are its frame: the title bar sits above the picture.
titlebar="$(setting window.titleBar)"
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
        set background color of opts to {65535, 65535, 65535}
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
  # A fresh .DS_Store per attempt: a file a failed attempt left must not pass the wait below.
  rm -f "$mount/.DS_Store"
  if osascript "$script" "$vol" "$app_name" "$left" "$top" "$((left + width))" "$((top + height + titlebar))" \
      "$(setting iconSize)" "$(setting textSize)" \
      "$(setting appPosition.x)" "$(setting appPosition.y)" \
      "$(setting applicationsPosition.x)" "$(setting applicationsPosition.y)"; then
    # Finder writes the .DS_Store on its own time after the window closes, and may
    # write it more than once: wait until it exists and has stopped changing.
    last=""
    for _ in $(seq 1 60); do
      if [ -f "$mount/.DS_Store" ]; then
        now="$(stat -f '%z %m' "$mount/.DS_Store")"
        [ "$now" = "$last" ] && break
        last="$now"
      fi
      sleep 1
    done
    # The volume's icon: .VolumeIcon.icns at the root with the custom-icon flag on it.
    # After Finder, which deletes an icon it finds while it lays the window out (and
    # `hdiutil create -srcfolder` leaves that name out).
    cp "$inputs/volume.icns" "$mount/.VolumeIcon.icns"
    SetFile -a C "$mount"
    sync
    if [ "$(GetFileInfo -aC "$mount")" != 1 ]; then
      echo "::warning::the volume's custom-icon flag did not stick"
    # Finder cannot hide the tab bar, the path bar or the sidebar by AppleScript, and
    # a Finder that shows tabs saves the window with them: the window record is
    # rewritten with every bar off (ds-store.mjs), then read back with the rest.
    elif ! node "$here/ds-store.mjs" hide-chrome "$mount/.DS_Store"; then
      echo "::warning::the window settings could not be rewritten"
    elif node "$here/dmg-verify.mjs" "$mount" "$settings"; then
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

sync
detach "$device"
device=""

# Finder may write the volume's .DS_Store once more as it lets go of it, and that
# write can put a bar back (a runner saw ShowPathbar return after the record had been
# read back). So the window record is rewritten a last time, and the whole layout
# read back, on a mount Finder never sees (-nobrowse) and never lets go of, before
# the image is converted and signed (the action checks the finished .dmg the same way).
check="$work/check"
mkdir -p "$check"
check_out="$(hdiutil attach -readwrite -nobrowse -noautoopen -noverify -mountpoint "$check" "$work/rw.dmg")"
device="$(printf '%s\n' "$check_out" | awk 'NR == 1 { print $1; exit }')"
case "$device" in /dev/disk[0-9]*) ;; *) device="";; esac
[ -n "$device" ] && [ -f "$check/.DS_Store" ] || { echo "::error::the image did not mount out of Finder's sight: $check_out" >&2; exit 1; }
if ! node "$here/ds-store.mjs" hide-chrome "$check/.DS_Store"; then
  echo "::error::the window settings could not be rewritten once Finder let go of the volume" >&2; exit 1
fi
# Nothing Finder or Spotlight left behind travels; the content is read-only to others.
rm -rf "$check/.fseventsd" "$check/.Trashes" "$check/.Spotlight-V100"
chmod -Rf go-w "$check" || true
sync
if ! node "$here/dmg-verify.mjs" "$check" "$settings"; then
  echo "::error::the layout did not survive the image's detach" >&2; exit 1
fi
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
