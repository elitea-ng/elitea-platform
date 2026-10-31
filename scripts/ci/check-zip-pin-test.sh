#!/usr/bin/env bash
# check-zip-pin-test.sh — the zip pin gate is proved red.
#
# Each case copies the tracked files the gate reads into a throwaway git tree,
# breaks ONE thing, and requires check-zip-pin.sh to fail with the message that
# names it. A gate that cannot be shown to fail measures nothing. The first
# case is the unbroken copy, which must pass: otherwise every red below could
# be the copy, not the breakage.
#
# The cases cover every assertion of the gate: a manifest requirement that is
# not an exact pin (the caret that Gap 2 of PR #1546 closed), manifests that
# disagree, a manifest that states nothing, an unlisted manifest that declares
# zip, a lock on zip 8.6.0 (the CVE version), a lock on a later 9.x (what a
# caret resolves), a lock with two zip versions, a patched crate that resolved
# from the registry (a DROPPED [patch.crates-io] entry), a patched crate at the
# wrong version, and every non-vacuity floor.
#
#   bash scripts/ci/check-zip-pin-test.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
GATE="${REPO_ROOT}/scripts/ci/check-zip-pin.sh"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/zip-pin-gate.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

# The inputs the gate reads, from the real tree.
inputs="$(cd "$REPO_ROOT" && git ls-files -- '*Cargo.toml' '*Cargo.lock')"
[ -n "$inputs" ] || { echo "ERROR: found no gate inputs to copy" >&2; exit 1; }

passed=0
failed=0

fresh() {
  rm -rf "$WORK/tree"
  mkdir -p "$WORK/tree"
  (cd "$REPO_ROOT" && printf '%s\n' "$inputs" | while IFS= read -r f; do
    mkdir -p "$WORK/tree/$(dirname "$f")"
    cp "$f" "$WORK/tree/$f"
  done)
  git -C "$WORK/tree" init -q
  git -C "$WORK/tree" add -A
}

# expect <name> <pass|fail> <message substring or ''>
expect() {
  local name="$1" want="$2" needle="$3" output status
  set +e
  output="$(ZIP_PIN_ROOT="$WORK/tree" bash "$GATE" 2>&1)"
  status=$?
  set -e
  if [ "$want" = pass ] && [ "$status" -eq 0 ]; then
    echo "ok   ${name}"; passed=$((passed + 1)); return
  fi
  if [ "$want" = fail ] && [ "$status" -ne 0 ] && printf '%s' "$output" | grep -qF -- "$needle"; then
    echo "ok   ${name}"; passed=$((passed + 1)); return
  fi
  echo "FAIL ${name}: wanted ${want} (message: ${needle:-none}), got exit ${status}:" >&2
  printf '%s\n' "$output" | grep '^ERROR' | head -4 | sed 's/^/       /' >&2
  failed=$((failed + 1))
}

edit() { # edit <file> <sed expression>
  sed -i.bak -E "$2" "$WORK/tree/$1" && rm -f "$WORK/tree/$1.bak"
  git -C "$WORK/tree" add -A
}

put() { # put <file> <printf format>: write and stage a file in the copy
  mkdir -p "$WORK/tree/$(dirname "$1")"
  printf '%b' "$2" > "$WORK/tree/$1"
  git -C "$WORK/tree" add -A
}

# set_pkg <lock> <package> <field> <value>: rewrite one field of one package
# block of a Cargo.lock. An empty value removes the field, which is what a
# path-patched package looks like.
set_pkg() {
  awk -v want="$2" -v field="$3" -v value="$4" '
    /^\[\[package\]\]/ { here = 0 }
    /^name[[:space:]]*=/ {
      n = $0; sub(/^[^"]*"/, "", n); sub(/".*$/, "", n); here = (n == want)
    }
    here && field == "version" && /^version[[:space:]]*=/ {
      print "version = \"" value "\""; next
    }
    # Another field: drop any line it already has, then write it after the
    # version line. A path-patched package has no `source` line at all, so a
    # case that gives it one has to ADD the line, not rewrite it.
    here && field != "version" && $0 ~ ("^" field "[[:space:]]*=") { next }
    here && field != "version" && /^version[[:space:]]*=/ {
      print
      if (value != "") print field " = \"" value "\""
      next
    }
    { print }
  ' "$WORK/tree/$1" > "$WORK/tree/$1.new"
  mv "$WORK/tree/$1.new" "$WORK/tree/$1"
  git -C "$WORK/tree" add -A
}

fresh; expect "the unbroken tree passes" pass ""

# (a) the pin in the manifests
fresh; edit libs/rust/vendor/calamine/Cargo.toml 's/^version = "=9\.1\.3"$/version = "9.1.3"/'
expect "(a) a vendored crate back on a caret requirement" fail "libs/rust/vendor/calamine/Cargo.toml:149: zip 9.1.3 is not an exact pin"
fresh; edit libs/rust/office-core/Cargo.toml 's/zip = \{ version = "=9\.1\.3"/zip = { version = "^9.1.3"/'
expect "(a) a host crate on a caret requirement" fail "libs/rust/office-core/Cargo.toml:19: zip ^9.1.3 is not an exact pin"
fresh; edit libs/rust/office-core/Cargo.toml 's/zip = \{ version = "=9\.1\.3"/zip = { version = "=9.2.0"/'
expect "(a) two manifests that disagree" fail "but another manifest pins 9.1.3"
fresh; edit services/elitea-doc-extractor/Cargo.toml '/^zip = \{ version = "=9\.1\.3"/d'
expect "(a) a listed manifest that states no zip" fail "services/elitea-doc-extractor/Cargo.toml: states no zip requirement"
fresh; put services/elitea-vector/extra/Cargo.toml '[package]\nname = "extra"\nversion = "0.1.0"\n\n[dependencies]\nzip = "8.6"\n'
expect "(a) a NEW manifest that declares zip must be listed" fail "services/elitea-vector/extra/Cargo.toml: declares zip"
fresh; for f in $(cd "$WORK/tree" && git ls-files -- '*Cargo.toml'); do
  edit "$f" 's/^(version = )"=9\.1\.3"$/\1"9.1.3"/; s/zip = \{ version = "=9\.1\.3"/zip = { version = "9.1.3"/'
done
expect "(a)(d) no exact pin anywhere" fail "read no exact zip pin from any manifest"

# (b) the version in the lockfiles
fresh; set_pkg services/elitea-doc-extractor/Cargo.lock zip version 8.6.0
expect "(b) a lock on the CVE version" fail "services/elitea-doc-extractor/Cargo.lock: resolves zip 8.6.0, the pin is 9.1.3"
fresh; set_pkg apps/elitea-desktop/doc-extract-sidecar/Cargo.lock zip version 9.2.0
expect "(b) a lock that a caret moved to a later 9.x" fail "apps/elitea-desktop/doc-extract-sidecar/Cargo.lock: resolves zip 9.2.0"
fresh
printf '\n[[package]]\nname = "zip"\nversion = "8.6.0"\nsource = "registry+https://github.com/rust-lang/crates.io-index"\n' \
  >> "$WORK/tree/libs/rust/Cargo.lock"
git -C "$WORK/tree" add -A
expect "(b) a lock with two zip versions" fail "libs/rust/Cargo.lock: resolves zip at 2 versions"

# (c) the patches are applied
fresh; set_pkg services/elitea-inventory-engine/Cargo.lock calamine source "registry+https://github.com/rust-lang/crates.io-index"
expect "(c) a dropped calamine patch" fail "services/elitea-inventory-engine/Cargo.lock: calamine 0.36.2 resolves from 'registry+https://github.com/rust-lang/crates.io-index'"
fresh; set_pkg services/elitea-worker-rust/Cargo.lock zavora-xlsx source "registry+https://github.com/rust-lang/crates.io-index"
expect "(c) a dropped zavora patch" fail "services/elitea-worker-rust/Cargo.lock: zavora-xlsx 0.1.2 resolves from"
fresh; set_pkg services/elitea-doc-extractor/Cargo.lock calamine version 0.36.1
expect "(c) a calamine at another version" fail "services/elitea-doc-extractor/Cargo.lock: calamine 0.36.1, but libs/rust/vendor/calamine is 0.36.2"
fresh; edit libs/rust/vendor/calamine/Cargo.toml '0,/^version = "0\.36\.2"$/{/^version = "0\.36\.2"$/d}'
expect "(c)(d) a vendored crate with no version" fail "libs/rust/vendor/calamine/Cargo.toml: read no '[package] version'"

# (d) non-vacuity
fresh; git -C "$WORK/tree" rm -qf services/elitea-vector/Cargo.lock
expect "(d) one lockfile fewer than the floor" fail "Cargo.lock file(s), expected at least 12"
fresh; for f in $(cd "$WORK/tree" && git ls-files -- '*Cargo.lock'); do edit "$f" 's/^name = "zip"$/name = "zipp"/'; done
expect "(d) no zip entry in any lockfile" fail "found no zip entry in any of the 12 lockfile(s)"
fresh; git -C "$WORK/tree" rm -qf libs/rust/Cargo.lock services/elitea-worker-rust/Cargo.lock
expect "(d) patched entries below the floor" fail "patched package entr(ies), expected at least 10"
fresh; git -C "$WORK/tree" rm -qf -r --cached .
expect "(d) an empty tree is refused" fail "found no tracked Cargo.lock at all"

total=$((passed + failed))
expected_cases="$(grep -cE '^fresh(;|$)' "$0")"
echo "${passed}/${total} gate cases behave (${expected_cases} declared)"
if [ "$failed" -ne 0 ] || [ "$total" -ne "$expected_cases" ] || [ "$total" -lt 17 ]; then
  echo "ERROR: the zip pin gate did not behave in every case" >&2
  exit 1
fi
