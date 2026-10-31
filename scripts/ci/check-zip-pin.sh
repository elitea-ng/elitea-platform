#!/usr/bin/env bash
# check-zip-pin.sh — one zip everywhere, and the zip-9 patches really applied.
#
# PR #1546 moved every Rust build graph in this repository onto `zip` 9.1.3.
# zip 2.x and zip 8.6.0 carry CVE-2026-115905 (CRITICAL, fixed in 9.0.5) and
# CVE-2026-115907, CVE-2026-115908 and CVE-2026-115909 (HIGH, fixed in 9.0.0).
# The published `calamine`, `zavora-xlsx`, `zavora-docx-opc`,
# `zavora-slide-opc` and `zavora-slide-xlsx` crates all request zip 2 or zip 8,
# so each one is vendored under libs/rust/vendor/ on zip 9 and applied with a
# `[patch.crates-io]` path entry in every host manifest.
#
# WHY A GATE AT ALL. The whole fix rests on those patch entries APPLYING.
# Cargo drops a patch that no dependant accepts and only prints
# `patch ... was not used in the crate graph`; nothing fails. That already
# happened during PR #1546: the vendored calamine is 0.36.2, the locks held
# 0.36.1, and the patch was dropped until `cargo update -p calamine` ran in
# each host. The regression this gate catches is the same shape: raise a
# dependant (for example `xberg`) to a release that needs
# `calamine ^0.36.5`, the vendored 0.36.2 leaves the range, cargo stops
# applying the patch, resolution falls back to the registry calamine on
# `zip ^8.6`, and the CRITICAL CVE returns to elitea-doc-extractor,
# elitea-inventory-engine and the desktop sidecar. Every test still passes.
# The nightly image scan finds it the next morning, if at all.
#
# THE ONE VALUE. The pin is the `zip` requirement the manifests state. Every
# manifest in ZIP_PIN_MANIFESTS below must state the SAME exact requirement
# (`=X.Y.Z`); that value is the expected version. Raising the pin is one edit
# per manifest and no edit here, as check-rust-toolchain.sh does for the
# compiler.
#
# WHAT IT ASSERTS
#
#   (a) Every manifest in ZIP_PIN_MANIFESTS states exactly one `zip`
#       requirement, in the exact `=X.Y.Z` form, and they all agree. A caret
#       (`"9.1.3"`, which means `^9.1.3`) fails: the two workspaces that take
#       zip only through vendored calamine would otherwise resolve 9.2.0 or
#       any later 9.x on the next `cargo update`. A tracked Cargo.toml that
#       declares `zip` and is NOT in the list fails too, with its value.
#   (b) Every tracked Cargo.lock resolves `zip` at exactly ONE version, and
#       that version equals the pin. A lock with no `zip` at all is fine and
#       is reported as such; which locks those are is not hardcoded.
#   (c) The patches are APPLIED, not merely declared. In every lock where one
#       of PATCHED_CRATES appears, the package must resolve to the vendored
#       path copy and to the vendored version. In a Cargo.lock a path-patched
#       package carries NO `source` line; a registry package carries
#       `source = "registry+https://github.com/rust-lang/crates.io-index"`.
#       The expected version is read from the vendored crate's own Cargo.toml,
#       so a re-vendor moves it in one place.
#   (d) Non-vacuity. The run FAILS if it read no lockfiles, if the lockfile
#       count fell below its floor, if no lock held a `zip` entry, if the
#       patched-package count fell below its floor, or if a manifest or a
#       vendored crate gave no readable version. "Found nothing, so nothing
#       disagreed" is the shape this repository keeps mistaking for a pass
#       (issue #423; check-rust-toolchain.sh point (d)).
#
# THE REAL COUNTS TODAY (2026-10-31). 12 tracked Cargo.lock files; 6 of them
# hold `zip`, all at 9.1.3 (libs/rust, services/elitea-worker-rust,
# services/elitea-doc-extractor, services/elitea-inventory-engine,
# apps/elitea-desktop/doc-extract-sidecar, apps/elitea-desktop/src-tauri); the
# other 6 hold no `zip`. 12 patched package entries in all. 8 manifests state
# the pin. `find . -name Cargo.lock -not -path '*/target/*'` reports 11: it
# does not match the tracked
# services/elitea-code-runner/adapters/rust/platform-v1.Cargo.lock, which this
# gate does read.
#
# DEV-DEPENDENCIES. A dev-dependency of the crate a workspace builds IS
# resolved and DOES enter that workspace's Cargo.lock, so this gate reads it:
# `services/elitea-doc-extractor` declares `zip` exactly there, for the tests'
# own fixtures. A dev-dependency of a PATH dependency is never resolved
# (libs/rust/vendor/README.md), so a vendored crate's dev-dependencies reach no
# lock and no image, and the gate skips them. That is why
# `libs/rust/vendor/zavora-slide/Cargo.toml`, which requests `zip` 8.1 as a
# dev-dependency of upstream's own test suite, is not in scope: it resolves
# nowhere, and no lock in this repository holds zip 8.
#
# WORKSPACE INHERITANCE. No manifest in this repository has a
# `[workspace.dependencies]` table, so no crate writes
# `zip = { workspace = true }`. The gate reads a literal requirement only. A
# first inherited `zip` needs the inherited table read here as well; until
# then the assertion (a) list and the lockfile assertions (b) and (c) cover
# every path a zip version can take.
#
# NO PIPE INTO AN EARLY-EXIT READER. This script reads a variable with a bash
# here-string (`awk ... <<< "$var"`), never with `printf | awk`. A here-string
# is backed by a temporary file, so the reader can `exit` on its first match at
# no risk. A pipe cannot: the kernel pipe buffer holds 65536 bytes, `awk ...
# exit` or `grep -q` closes the read end on the first match, and a writer with
# more data then gets EPIPE. `set -euo pipefail` turns that into a red run.
# That defect shipped in this gate: `lock_packages libs/rust/Cargo.lock` gave
# 64900 bytes, 636 bytes below the buffer, so it passed on the branch and
# failed on the merge commit, where the lockfile was larger. Keep the
# here-strings. check-zip-pin-test.sh case "a large lockfile does not break the
# reader pipe" holds the line.
#
# It needs no network, no toolchain and no cargo:
#   bash scripts/ci/check-zip-pin.sh
# Point it at another tree (the self-test does) with ZIP_PIN_ROOT.
# scripts/ci/check-zip-pin-test.sh proves it red, case by case.
set -euo pipefail

cd "${ZIP_PIN_ROOT:-"$(dirname "$0")/../.."}"

# (a) Every manifest that declares `zip`. The host manifests state the pin for
# their own workspace; the five vendored crates state it for every workspace
# that takes zip only through them.
ZIP_PIN_MANIFESTS="
libs/rust/agent-runtime/Cargo.toml
libs/rust/office-core/Cargo.toml
libs/rust/vendor/calamine/Cargo.toml
libs/rust/vendor/zavora-docx-opc/Cargo.toml
libs/rust/vendor/zavora-slide-opc/Cargo.toml
libs/rust/vendor/zavora-slide-xlsx/Cargo.toml
libs/rust/vendor/zavora-xlsx/Cargo.toml
services/elitea-doc-extractor/Cargo.toml
"

# (c) The vendored crates whose patch entry carries the zip-9 fix. Each one
# must resolve to its path copy in every lock that holds it.
PATCHED_CRATES="calamine zavora-docx-opc zavora-slide-opc zavora-slide-xlsx zavora-xlsx"

# (d) Floors. Real counts are in the header; a floor below the real count
# still turns a pattern that stopped matching into a red run.
MIN_LOCKFILES=12
MIN_ZIP_LOCKS=5
MIN_PATCHED_ENTRIES=10

fail=0
note() { printf 'ERROR: %s\n' "$1" >&2; fail=1; }

# ── readers ──────────────────────────────────────────────────────────────────

# 1 when a manifest's dev-dependencies resolve nowhere: a vendored path crate.
skip_dev_for() {
  case "$1" in
    libs/rust/vendor/*) printf '1' ;;
    *) printf '0' ;;
  esac
}

# Every `zip` requirement in a manifest, one `<line>\t<requirement>` per line.
# Both spellings: an inline `zip = { version = "..." }` table and a
# `[dependencies.zip]`-style section with its own `version = "..."`. A
# vendored crate's `[dev-dependencies]` is skipped (see DEV-DEPENDENCIES
# above); every other dependency kind is read.
zip_requirements() {
  awk -v skip_dev="$2" '
    /^[[:space:]]*\[/ {
      section = $0
      sub(/^[[:space:]]*\[/, "", section)
      sub(/\].*$/, "", section)
      in_dev = (section ~ /(^|[-.])dev-dependencies(\.|$)/)
      skip = (in_dev && skip_dev == 1)
      in_zip_table = (section ~ /dependencies\.zip$/) && !skip
      next
    }
    in_zip_table && /^[[:space:]]*version[[:space:]]*=[[:space:]]*"/ {
      v = $0
      sub(/^[^"]*"/, "", v)
      sub(/".*$/, "", v)
      print NR "\t" v
      next
    }
    !skip && /^[[:space:]]*zip[[:space:]]*=/ {
      line = $0
      if (match(line, /version[[:space:]]*=[[:space:]]*"[^"]*"/)) {
        v = substr(line, RSTART, RLENGTH)
      } else if (match(line, /=[[:space:]]*"[^"]*"/)) {
        v = substr(line, RSTART, RLENGTH)
      } else {
        next
      }
      sub(/^[^"]*"/, "", v)
      sub(/".*$/, "", v)
      print NR "\t" v
    }
  ' "$1"
}

# Every package of a Cargo.lock as `<name>\t<version>\t<source>`. Paragraph
# mode: a Cargo.lock package block holds no blank line, and blocks are
# separated by one.
lock_packages() {
  awk '
    BEGIN { RS = ""; FS = "\n" }
    /^\[\[package\]\]/ {
      name = ""; version = ""; source = ""
      for (i = 1; i <= NF; i++) {
        if ($i ~ /^name[[:space:]]*=[[:space:]]*"/)         { name = $i }
        else if ($i ~ /^version[[:space:]]*=[[:space:]]*"/) { version = $i }
        else if ($i ~ /^source[[:space:]]*=[[:space:]]*"/)  { source = $i }
        else { continue }
      }
      for (f = 0; f < 3; f++) {
        v = (f == 0 ? name : (f == 1 ? version : source))
        sub(/^[^"]*"/, "", v)
        sub(/".*$/, "", v)
        out[f] = v
      }
      print out[0] "\t" out[1] "\t" out[2]
    }
  ' "$1"
}

# The `[package] version` of a crate manifest.
package_version() {
  awk '
    /^[[:space:]]*\[/ { in_package = ($0 ~ /^[[:space:]]*\[package\]/); next }
    in_package && /^[[:space:]]*version[[:space:]]*=[[:space:]]*"/ {
      v = $0; sub(/^[^"]*"/, "", v); sub(/".*$/, "", v); print v; exit
    }
  ' "$1"
}

# ── (a) the pin ──────────────────────────────────────────────────────────────

manifests="$(printf '%s\n' "$ZIP_PIN_MANIFESTS" | grep -v '^$' || true)"
pin=""
for manifest in $manifests; do
  if [ ! -f "$manifest" ]; then
    note "$manifest: missing, so the zip pin cannot be read from it"
    continue
  fi
  found="$(zip_requirements "$manifest" "$(skip_dev_for "$manifest")")"
  if [ -z "$found" ]; then
    note "$manifest: states no zip requirement; the pin cannot be read from it"
    continue
  fi
  while IFS="$(printf '\t')" read -r number requirement; do
    [ -n "$requirement" ] || continue
    case "$requirement" in
      "="[0-9]*.[0-9]*.[0-9]*) ;;
      *)
        note "$manifest:$number: zip $requirement is not an exact pin; write \"=X.Y.Z\". A caret range lets cargo resolve an unvetted later zip in any workspace that takes zip through this crate"
        continue
        ;;
    esac
    if [ -z "$pin" ]; then
      pin="${requirement#=}"
      printf 'pin: zip %s (%s:%s)\n' "$pin" "$manifest" "$number"
    elif [ "${requirement#=}" != "$pin" ]; then
      note "$manifest:$number: zip ${requirement#=}, but another manifest pins $pin; the manifests must agree on one version"
    fi
  done <<EOF
$found
EOF
done

# Any other tracked manifest that declares zip must join the list above.
for manifest in $(git ls-files -- '*Cargo.toml'); do
  grep -qxF "$manifest" <<< "$manifests" && continue
  other="$(zip_requirements "$manifest" "$(skip_dev_for "$manifest")")"
  [ -z "$other" ] && continue
  note "$manifest: declares zip ($(printf '%s' "$other" | cut -f2 | tr '\n' ' ')) but is not in ZIP_PIN_MANIFESTS in this script; add it there, with an exact pin"
done

if [ -z "$pin" ]; then
  echo "ERROR: read no exact zip pin from any manifest; there is nothing to compare the lockfiles against" >&2
  exit 1
fi

# ── (c) the vendored versions the patches must resolve to ────────────────────

vendored_versions=""
for crate in $PATCHED_CRATES; do
  manifest="libs/rust/vendor/$crate/Cargo.toml"
  version="$([ -f "$manifest" ] && package_version "$manifest" || true)"
  if [ -z "$version" ]; then
    note "$manifest: read no '[package] version'; the version the patch must resolve to is unknown"
    continue
  fi
  vendored_versions="${vendored_versions}${crate} ${version}
"
  printf '  vendored %s %s (%s)\n' "$crate" "$version" "$manifest"
done

vendored_version_of() {
  awk -v c="$1" '$1 == c { print $2; exit }' <<< "$vendored_versions"
}

# ── (b) and (c) the lockfiles ────────────────────────────────────────────────

lockfiles="$(git ls-files -- '*Cargo.lock')"
if [ -z "$lockfiles" ]; then
  echo "ERROR: found no tracked Cargo.lock at all; there is nothing to check" >&2
  exit 1
fi

lock_count=0
zip_locks=0
zip_entries=0
patched_entries=0

for lock in $lockfiles; do
  lock_count=$((lock_count + 1))
  packages="$(lock_packages "$lock")"

  zip_versions="$(printf '%s\n' "$packages" | awk -F'\t' '$1 == "zip" { print $2 }' | sort -u)"
  count="$(printf '%s' "$zip_versions" | grep -c . || true)"
  if [ "$count" -eq 0 ]; then
    printf '  %s: no zip\n' "$lock"
  else
    zip_locks=$((zip_locks + 1))
    zip_entries=$((zip_entries + count))
    if [ "$count" -gt 1 ]; then
      note "$lock: resolves zip at $count versions ($(printf '%s' "$zip_versions" | tr '\n' ' ')); exactly one version must be in the graph, and it must be the pin $pin"
    fi
    for version in $zip_versions; do
      if [ "$version" != "$pin" ]; then
        note "$lock: resolves zip $version, the pin is $pin. zip below 9.0.5 carries CVE-2026-115905 (CRITICAL); run 'cargo update -p zip@$version --precise $pin' in that workspace and find out which requirement moved"
      fi
    done
    [ "$count" -eq 1 ] && [ "$zip_versions" = "$pin" ] && printf '  %s: zip %s\n' "$lock" "$pin"
  fi

  for crate in $PATCHED_CRATES; do
    entry="$(awk -F'\t' -v c="$crate" '$1 == c { print $2 "\t" $3; exit }' <<< "$packages")"
    [ -z "$entry" ] && continue
    patched_entries=$((patched_entries + 1))
    version="$(printf '%s' "$entry" | cut -f1)"
    source="$(printf '%s' "$entry" | cut -f2)"
    expected="$(vendored_version_of "$crate")"
    if [ -n "$source" ]; then
      note "$lock: $crate $version resolves from '$source', not from the vendored path copy. The [patch.crates-io] entry was DROPPED, so this lock no longer carries the zip-9 fix"
    elif [ -n "$expected" ] && [ "$version" != "$expected" ]; then
      note "$lock: $crate $version, but libs/rust/vendor/$crate is $expected; the lock does not hold the vendored copy"
    else
      printf '  %s: %s %s (vendored)\n' "$lock" "$crate" "$version"
    fi
  done
done

# ── (d) non-vacuity ──────────────────────────────────────────────────────────

if [ "$lock_count" -lt "$MIN_LOCKFILES" ]; then
  note "read $lock_count Cargo.lock file(s), expected at least $MIN_LOCKFILES; the pattern stopped matching"
fi
if [ "$zip_entries" -lt 1 ]; then
  note "found no zip entry in any of the $lock_count lockfile(s); the lock reader stopped matching, so this run compared nothing"
fi
if [ "$zip_locks" -lt "$MIN_ZIP_LOCKS" ]; then
  note "found zip in $zip_locks lockfile(s), expected at least $MIN_ZIP_LOCKS; the lock reader or the build graph changed"
fi
if [ "$patched_entries" -lt "$MIN_PATCHED_ENTRIES" ]; then
  note "found $patched_entries patched package entr(ies), expected at least $MIN_PATCHED_ENTRIES; a [patch.crates-io] entry was dropped or the reader stopped matching"
fi

if [ "$fail" -ne 0 ]; then
  echo "" >&2
  echo "The zip CVE pin does not hold. See the header of scripts/ci/check-zip-pin.sh." >&2
  exit 1
fi

printf 'One zip everywhere: zip %s in %s of %s lockfile(s), %s patched package entr(ies).\n' \
  "$pin" "$zip_locks" "$lock_count" "$patched_entries"
