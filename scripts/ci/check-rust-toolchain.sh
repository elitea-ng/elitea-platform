#!/usr/bin/env bash
# check-rust-toolchain.sh — one Rust version everywhere: cargo, CI and images.
#
# PR #1297. The repository builds Rust in three places that nothing compared:
#
#   - cargo, which takes its compiler from the rust-toolchain.toml nearest the
#     current directory. There were eight such files, one per subtree, and none
#     at the root, so a script that ran cargo from the root
#     (scripts/runtime/test_sandbox_ledger.py) got the runner's default stable
#     compiler and rebuilt every dependency with it;
#   - CI, which printed `rustc --version` and asserted nothing;
#   - the images, which pull `rust:<version>-trixie`. One Containerfile pinned
#     the digest, five did not, so two builds of the "same" tag could use two
#     different compilers.
#
# THE ONE VALUE. The repository-root rust-toolchain.toml `channel` is the pin.
#
# WHAT IT ASSERTS
#
#   (a) Every tracked rust-toolchain.toml (root included) names that exact
#       channel. The subtree files stay: an image build copies only its
#       subtree, so the root file is not in its context.
#   (b) Every `FROM rust:` line in every tracked Containerfile, Dockerfile and
#       bake file uses `rust:<pin>-trixie@sha256:<RUST_IMAGE_DIGEST>`, the one
#       digest below, in every stage (builders and runtime-libraries alike).
#   (c) Every non-vendored Cargo.toml states `rust-version` equal to the MSRV
#       below (directly or by inheriting the libs/rust workspace value), the
#       MSRV is <= the pin, and every vendored crate's own upstream value is
#       <= the pin. Manifests that are not built as Rust crates by any of our
#       toolchains are exempt below, each with its reason.
#   (d) Every read that comes back empty FAILS, and so does a file list that
#       shrank below its floor. "Found no pin, so nothing disagreed" is the
#       shape this repository keeps mistaking for a pass.
#
# HOW TO RAISE THE VERSION
#
#   1. Set `channel` in rust-toolchain.toml at the root AND in every file this
#      script lists (it prints them). One value per file.
#   2. Set `rust:<new>-trixie@sha256:<digest>` in every Containerfile it lists,
#      and RUST_IMAGE_DIGEST below to the same digest, read from the registry:
#        docker buildx imagetools inspect rust:<new>-trixie   # "Digest:" line
#   3. Leave MSRV alone unless code needs a newer language or library feature;
#      it is the floor, not the pin.
#   4. Run this script. It names every place that still disagrees.
#
# It needs no network, no toolchain and no cluster:
#   bash scripts/ci/check-rust-toolchain.sh
# Point it at another tree (the negative tests do) with RUST_TOOLCHAIN_ROOT.
set -euo pipefail

REPO_ROOT="${RUST_TOOLCHAIN_ROOT:-$(cd "$(dirname "$0")/../.." && pwd)}"
cd "$REPO_ROOT"

# The image index digest of rust:<pin>-trixie (all platforms). Moves with the pin.
RUST_IMAGE_DIGEST="sha256:b1b3c9c0d921d7fa0a6d1f9ec7e4eab87f8c8ec97644c3d791450f131dec813f"
# The minimum supported Rust version of our own crates: the documented floor,
# deliberately below the pin (see the PR #1297 body). Must be <= the pin.
MSRV="1.95"

# Floors: fewer files than this means a pattern stopped matching.
MIN_TOOLCHAIN_FILES=9
MIN_RUST_FROM_LINES=12
MIN_CRATES=30

# Cargo.toml files no Rust toolchain of ours builds as a crate with an MSRV.
# Path, whitespace, reason. An entry here is a decision, not a silence.
EXEMPT_CRATES=$(cat <<'EXEMPTIONS'
libs/rust/office-core/fuzz/Cargo.toml  cargo-fuzz harness built only by nightly cargo-fuzz, never by CI or an image
services/elitea-code-runner/adapters/rust/Cargo.toml  template compiled inside the sandbox for user code (include_str! in src/rust_profile.rs); not a crate of ours
scripts/runtime/fixtures/code-data-processing/Cargo.toml  locked acceptance fixture of user code; its manifest is a test input
EXEMPTIONS
)

failures=0
fail() { echo "ERROR: $1" >&2; failures=$((failures + 1)); }
is_exempt() { printf '%s\n' "$EXEMPT_CRATES" | grep -q "^$1[[:space:]]"; }

# Version comparison: is $1 <= $2? Both "X.Y" or "X.Y.Z"; missing parts are 0.
version_le() {
  local IFS=.
  # shellcheck disable=SC2206
  local a=($1) b=($2)
  local i
  for i in 0 1 2; do
    local x="${a[$i]:-0}" y="${b[$i]:-0}"
    if [ "$x" -lt "$y" ]; then return 0; fi
    if [ "$x" -gt "$y" ]; then return 1; fi
  done
  return 0
}

tracked() { git ls-files "$@" 2>/dev/null || true; }
if [ -z "$(tracked)" ]; then
  echo "ERROR: ${REPO_ROOT} is not a git work tree with tracked files; there is nothing to check" >&2
  exit 1
fi

# ── (a) toolchain files ───────────────────────────────────────────────────────
[ -f rust-toolchain.toml ] || { echo "ERROR: rust-toolchain.toml is missing at the repository root; there is no pin" >&2; exit 1; }
channel_of() {
  sed -nE 's/^[[:space:]]*channel[[:space:]]*=[[:space:]]*"([^"]*)"[[:space:]]*$/\1/p' "$1"
}
PIN="$(channel_of rust-toolchain.toml)"
if ! printf '%s' "$PIN" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
  echo "ERROR: read no exact 'channel = \"X.Y.Z\"' from rust-toolchain.toml (got '${PIN}')." >&2
  echo "       Every comparison below would hold against an empty string." >&2
  exit 1
fi
echo "pin: rust ${PIN} (rust-toolchain.toml)"

toolchains="$(tracked 'rust-toolchain.toml' '*/rust-toolchain.toml' 'rust-toolchain' '*/rust-toolchain' | sort -u)"
toolchain_count="$(printf '%s\n' "$toolchains" | grep -c . || true)"
[ "$toolchain_count" -ge "$MIN_TOOLCHAIN_FILES" ] ||
  fail "found ${toolchain_count} rust-toolchain file(s), expected at least ${MIN_TOOLCHAIN_FILES}; the pattern stopped matching"
while IFS= read -r file; do
  [ -n "$file" ] || continue
  case "$file" in
    *.toml) ;;
    *) fail "${file}: a legacy rust-toolchain file without .toml; use rust-toolchain.toml"; continue ;;
  esac
  channel="$(channel_of "$file")"
  if [ -z "$channel" ]; then
    fail "${file}: read no 'channel = \"...\"'"
  elif [ "$channel" != "$PIN" ]; then
    fail "${file}: channel ${channel}, the pin is ${PIN}"
  else
    echo "  ${file}: ${channel}"
  fi
done <<EOF
$toolchains
EOF

# ── (b) images ────────────────────────────────────────────────────────────────
image_files="$(tracked '*Containerfile*' '*Dockerfile*' '*.hcl' | sort -u)"
from_count=0
while IFS= read -r file; do
  [ -n "$file" ] || continue
  # Every reference to the official rust image: FROM lines, and any other
  # rust:<tag> reference (bake contexts, COPY --from=rust:...).
  refs="$(grep -nE '(^|[^A-Za-z0-9_./-])rust:[^[:space:]"]*' "$file" | grep -vE '^[0-9]+:[[:space:]]*#' || true)"
  [ -n "$refs" ] || continue
  while IFS= read -r ref; do
    [ -n "$ref" ] || continue
    line="${ref%%:*}"
    image="$(printf '%s' "${ref#*:}" | grep -oE '(^|[^A-Za-z0-9_./-])rust:[^[:space:]"]*' | head -1 | sed -E 's/^[^r]//')"
    from_count=$((from_count + 1))
    expected="rust:${PIN}-trixie@${RUST_IMAGE_DIGEST}"
    if [ "$image" != "$expected" ]; then
      fail "${file}:${line}: ${image:-<unreadable>}, expected ${expected}"
    else
      echo "  ${file}:${line}: ${image%@*}@…${RUST_IMAGE_DIGEST: -8}"
    fi
  done <<REFS
$refs
REFS
done <<EOF
$image_files
EOF
[ "$from_count" -ge "$MIN_RUST_FROM_LINES" ] ||
  fail "found ${from_count} rust image reference(s), expected at least ${MIN_RUST_FROM_LINES}; the pattern stopped matching"

# ── (c) MSRV ──────────────────────────────────────────────────────────────────
printf '%s' "$MSRV" | grep -Eq '^[0-9]+\.[0-9]+(\.[0-9]+)?$' || { echo "ERROR: MSRV '${MSRV}' is not a version" >&2; exit 1; }
version_le "$MSRV" "$PIN" || fail "MSRV ${MSRV} is above the pin ${PIN}; the pinned compiler could not build our crates"
echo "MSRV: ${MSRV} (floor of our crates; vendored crates keep their own, <= ${PIN})"

value_of() {
  sed -nE 's/^[[:space:]]*rust-version[[:space:]]*=[[:space:]]*"([^"]*)"[[:space:]]*$/\1/p' "$1" | head -1
}
inherits() { grep -Eq '^[[:space:]]*rust-version\.workspace[[:space:]]*=[[:space:]]*true' "$1"; }
WORKSPACE_MSRV="$(value_of libs/rust/Cargo.toml 2>/dev/null || true)"

crates="$(tracked '*Cargo.toml' | grep -E '(^|/)Cargo\.toml$|\.Cargo\.toml$' | sort -u)"
crate_count=0
while IFS= read -r file; do
  [ -n "$file" ] || continue
  grep -Eq '^\[package\]' "$file" || continue
  crate_count=$((crate_count + 1))
  if is_exempt "$file"; then
    echo "  ${file}: exempt"
    continue
  fi
  case "$file" in
    */vendor/*)
      value="$(value_of "$file")"
      if [ -z "$value" ]; then
        fail "${file}: vendored crate with no readable rust-version"
      elif ! version_le "$value" "$PIN"; then
        fail "${file}: vendored crate needs rust ${value}, above the pin ${PIN}"
      else
        echo "  ${file}: ${value} (vendored, <= ${PIN})"
      fi
      continue
      ;;
  esac
  if inherits "$file"; then
    case "$file" in
      libs/rust/*) value="$WORKSPACE_MSRV" ;;
      *) fail "${file}: inherits rust-version from a workspace this check does not read"; continue ;;
    esac
    how="inherited from libs/rust"
  else
    value="$(value_of "$file")"
    how="stated"
  fi
  if [ -z "$value" ]; then
    fail "${file}: no readable rust-version (state \"${MSRV}\" or inherit the workspace value)"
  elif [ "$value" != "$MSRV" ]; then
    fail "${file}: rust-version ${value} (${how}), the MSRV is ${MSRV}"
  else
    echo "  ${file}: ${value} (${how})"
  fi
done <<EOF
$crates
EOF
[ "$crate_count" -ge "$MIN_CRATES" ] ||
  fail "found ${crate_count} crate manifest(s), expected at least ${MIN_CRATES}; the pattern stopped matching"
while IFS= read -r exempt; do
  [ -n "$exempt" ] || continue
  path="${exempt%%[[:space:]]*}"
  [ -f "$path" ] || fail "MSRV exemption ${path} names no file; remove it"
done <<EOF
$EXEMPT_CRATES
EOF

if [ "$failures" -ne 0 ]; then
  echo "" >&2
  echo "${failures} Rust toolchain disagreement(s). See 'HOW TO RAISE THE VERSION' in $0." >&2
  exit 1
fi
echo "One Rust everywhere: rust ${PIN}, image digest …${RUST_IMAGE_DIGEST: -12}, MSRV ${MSRV}."
