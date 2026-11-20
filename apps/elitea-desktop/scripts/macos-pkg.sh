#!/usr/bin/env bash
# macos-pkg.sh — the desktop app as a macOS installer package for device management
# (ADR-0052 decision 8, W2b.1). It installs into /Applications with no UI, so Jamf,
# Intune for macOS, Kandji or `sudo installer -pkg <file> -target /` deploy it silently.
# Apple silicon only (owner decision, 2026-11-20): the app is arm64, and the package's
# distribution says `hostArchitectures="arm64"`, so the installer refuses an Intel Mac
# instead of installing an app that cannot start there.
#
#   macos-pkg.sh <path/to/App.app> <identifier> <version> <out-dir>
#
# Signed when INSTALLER_IDENTITY is set ("Developer ID Installer: <Name> (<TEAMID>)"):
# the identity comes from APPLE_CERTIFICATE (a .p12 that holds both Developer ID
# identities) or from INSTALLER_CERTIFICATE (a .p12 of its own; same password), is
# imported into a temporary keychain, signs the package with productbuild, and the
# package is notarised (notarytool, App Store Connect key: APPLE_API_KEY,
# APPLE_API_ISSUER, APPLE_API_KEY_PATH) and stapled. Unset: an unsigned package.
#
# The app must be signed and stapled already (tauri build does it) when signing.
set -euo pipefail

app="${1:?usage: macos-pkg.sh <App.app> <identifier> <version> <out-dir>}"
identifier="${2:?identifier}"
version="${3:?version}"
out="${4:?out-dir}"

[ -d "$app" ] || { echo "::error::no app bundle at $app" >&2; exit 1; }
name="$(basename "$app" .app)"
mkdir -p "$out"
work="$(mktemp -d)"
keychain=""
cleanup() {
  if [ -n "$keychain" ]; then security delete-keychain "$keychain" 2>/dev/null || true; fi
  rm -rf "$work"
}
trap cleanup EXIT

# The payload: the app under Applications/, nothing else.
mkdir -p "$work/root/Applications"
ditto "$app" "$work/root/Applications/$(basename "$app")"
# No extended attributes in the payload: pkgbuild would carry each as an AppleDouble
# `._` file into /Applications (the signature lives in Contents/_CodeSignature, not in
# an attribute; the quarantine and provenance attributes must not travel).
xattr -cr "$work/root"

# Not relocatable: an MDM install must land in /Applications even when a copy of the
# app sits elsewhere (pkgbuild would otherwise "upgrade" that copy in place).
pkgbuild --analyze --root "$work/root" "$work/component.plist" >/dev/null
/usr/libexec/PlistBuddy -c 'Set :0:BundleIsRelocatable false' "$work/component.plist"

pkgbuild \
  --root "$work/root" \
  --component-plist "$work/component.plist" \
  --identifier "$identifier.pkg" \
  --version "$version" \
  --install-location / \
  "$work/component.pkg"

# The product's distribution, synthesized from the component and pinned to Apple
# silicon: `productbuild --synthesize` writes one `<options …/>` element, the
# architecture goes on it (replacing any it carries), and the edit is checked.
productbuild --synthesize --package "$work/component.pkg" "$work/distribution.xml" >/dev/null
sed -E -i '' \
  -e 's/ hostArchitectures="[^"]*"//' \
  -e 's/<options /<options hostArchitectures="arm64" /' \
  "$work/distribution.xml"
if [ "$(grep -c 'hostArchitectures="arm64"' "$work/distribution.xml")" != 1 ]; then
  echo "::error::the synthesized distribution has no single <options> element to pin to arm64" >&2
  cat "$work/distribution.xml" >&2
  exit 1
fi
build_product() {
  productbuild --distribution "$work/distribution.xml" --package-path "$work" "$@" "$pkg"
}

pkg="$out/${name}-${version}-arm64.pkg"
if [ -z "${INSTALLER_IDENTITY:-}" ]; then
  build_product
  echo "unsigned package: $pkg"
  exit 0
fi

# Signing: a temporary keychain with the installer identity.
cert="${INSTALLER_CERTIFICATE:-${APPLE_CERTIFICATE:-}}"
[ -n "$cert" ] || { echo "::error::INSTALLER_IDENTITY is set but no certificate (APPLE_CERTIFICATE) to sign with" >&2; exit 1; }
: "${APPLE_CERTIFICATE_PASSWORD:?the certificate password}"
keychain="$work/installer.keychain-db"
kc_password="$(uuidgen)"
security create-keychain -p "$kc_password" "$keychain"
security set-keychain-settings -lut 3600 "$keychain"
security unlock-keychain -p "$kc_password" "$keychain"
printf '%s' "$cert" | base64 --decode > "$work/cert.p12"
security import "$work/cert.p12" -k "$keychain" -P "$APPLE_CERTIFICATE_PASSWORD" -T /usr/bin/productbuild >/dev/null
rm -f "$work/cert.p12"
security set-key-partition-list -S apple-tool:,apple: -s -k "$kc_password" "$keychain" >/dev/null
# productbuild looks the identity up in the search list.
existing=()
while IFS= read -r line; do
  line="${line#"${line%%[![:space:]]*}"}"; line="${line#\"}"; line="${line%\"}"
  [ -n "$line" ] && existing+=("$line")
done < <(security list-keychains -d user)
security list-keychains -d user -s "$keychain" "${existing[@]}"
if ! security find-identity -v "$keychain" | grep -qF "$INSTALLER_IDENTITY"; then
  echo "::error::the certificate holds no identity \"$INSTALLER_IDENTITY\" (a Developer ID Installer identity is needed)" >&2
  exit 1
fi

build_product --sign "$INSTALLER_IDENTITY" --keychain "$keychain"

: "${APPLE_API_KEY:?APPLE_API_KEY}" "${APPLE_API_ISSUER:?APPLE_API_ISSUER}" "${APPLE_API_KEY_PATH:?APPLE_API_KEY_PATH}"
xcrun notarytool submit "$pkg" --key "$APPLE_API_KEY_PATH" --key-id "$APPLE_API_KEY" --issuer "$APPLE_API_ISSUER" --wait --timeout 30m
xcrun stapler staple "$pkg"
echo "signed, notarised package: $pkg"
