# macos-keychain.sh — sourced by macos-pkg.sh and macos-dmg.sh: a temporary keychain
# holding one Developer ID identity from a base64 .p12, put in the user search list so
# productbuild and codesign find it. Not executable on its own.
#
#   trim_identity <value>                         prints it without surrounding blanks
#   macos_keychain_import <work-dir> <cert-b64> <password> <identity> <secret-name> <kind> <tool>
#     sets MACOS_KEYCHAIN to the keychain's path; the caller deletes it
#     (macos_keychain_delete) on exit. <secret-name> and <kind> ("Developer ID
#     Installer") only word the error when the identity is not in the .p12.
#   macos_keychain_delete                         removes MACOS_KEYCHAIN, if any

MACOS_KEYCHAIN=""

# A secret pasted with a trailing newline or space names no identity: trim it.
trim_identity() {
  local v="${1:-}"
  v="${v#"${v%%[![:space:]]*}"}"
  v="${v%"${v##*[![:space:]]}"}"
  printf '%s' "$v"
}

macos_keychain_import() {
  local work="$1" cert="$2" password="$3" identity="$4" secret="$5" kind="$6" tool="$7"
  local keychain="$work/signing.keychain-db" kc_password line
  kc_password="$(uuidgen)"
  security create-keychain -p "$kc_password" "$keychain"
  MACOS_KEYCHAIN="$keychain"
  security set-keychain-settings -lut 3600 "$keychain"
  security unlock-keychain -p "$kc_password" "$keychain"
  printf '%s' "$cert" | base64 --decode > "$work/cert.p12"
  security import "$work/cert.p12" -k "$keychain" -P "$password" -T "$tool" >/dev/null
  rm -f "$work/cert.p12"
  security set-key-partition-list -S apple-tool:,apple: -s -k "$kc_password" "$keychain" >/dev/null
  # productbuild and codesign look the identity up in the search list.
  local existing=()
  while IFS= read -r line; do
    line="${line#"${line%%[![:space:]]*}"}"; line="${line#\"}"; line="${line%\"}"
    [ -n "$line" ] && existing+=("$line")
  done < <(security list-keychains -d user)
  security list-keychains -d user -s "$keychain" "${existing[@]}"
  security find-identity "$keychain" > "$work/identities.txt"
  security find-identity -v "$keychain" > "$work/valid.txt"
  if ! grep -qF "$identity" "$work/valid.txt"; then
    # Why, without printing a name: absent from the .p12, or present but not valid
    # (the reason security gives, e.g. CSSMERR_TP_NOT_TRUSTED for a missing
    # Developer ID intermediate, or an expired certificate).
    local installers apps reason
    installers="$(grep -c '"Developer ID Installer: ' "$work/valid.txt" || true)"
    apps="$(grep -c '"Developer ID Application: ' "$work/valid.txt" || true)"
    reason="$(grep -F "$identity" "$work/identities.txt" | head -n 1 | sed -nE 's/.*" \((.*)\)$/\1/p' || true)"
    if grep -qF "$identity" "$work/identities.txt"; then
      echo "::error::the certificate holds the identity ${secret} names, but macOS does not accept it as valid (${reason:-no reason given})" >&2
    else
      echo "::error::the certificate holds no identity named by ${secret} (a ${kind} identity is needed). Valid identities in it: ${installers} Developer ID Installer, ${apps} Developer ID Application. Export both identities into the APPLE_CERTIFICATE .p12, and set the secret to the full name \`security find-identity -v\` prints." >&2
    fi
    return 1
  fi
}

macos_keychain_delete() {
  if [ -n "$MACOS_KEYCHAIN" ]; then security delete-keychain "$MACOS_KEYCHAIN" 2>/dev/null || true; fi
  MACOS_KEYCHAIN=""
}
