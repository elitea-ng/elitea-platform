#!/usr/bin/env bash
#
# The two FROZEN analysis-engine closures still resolve.
#
# WHAT THIS CLOSES. `services/elitea-deepwiki` and `services/elitea-inventory`
# each vendor a byte-frozen copy of an analysis engine under `src/*/engine/`,
# and each pins that copy's resolved dependency closure in an `engine` extra.
# Nothing in CI installs those extras and nothing in CI builds the `-engine`
# images: ADR-0022 keeps DeepWiki's out of the default bake group because it is
# torch-sized, and `deepwiki-real-engine.yml` — the one job that does build it —
# is weekly plus dispatch. So a change INSIDE a closure has no per-change gate
# at all, and "no gate reported anything" reads as "nothing is wrong".
#
# It happened twice:
#
#   * eight Dependabot bumps (#677-#679, #757-#761) landed inside DeepWiki's
#     closure and made `pip install '.[engine,storage-postgres]'`
#     ResolutionImpossible. The `-engine` image could not be built at all, for
#     days, before the weekly run said so. #799 restored it.
#   * #740 and #741 did the same to Inventory's, and that one was STILL broken
#     on main at 67f1e147 — found by running this script.
#
# .github/dependabot.yml now gives both packages their own pip entry with
# `ignore: "*"` and `open-pull-requests-limit: 0`, so that particular route in
# is shut. This script is the gate for every OTHER route: a hand edit, a
# re-copy of the engine, an `elitea-sdk` release, or the world changing under a
# requirement that names no upper bound.
#
# WHAT IT DOES, AND WHAT IT DELIBERATELY DOES NOT.
#
# It RESOLVES. It does not install, and it does not build an image: the
# DeepWiki engine image is ~2 GB and needs tens of minutes and >35 GB of disk,
# which is the whole reason its build is not a per-change gate. Resolution is
# what the failure class actually is — every one of the ten bumps above turned
# a satisfiable requirement set into an unsatisfiable one, and a resolver says
# so in seconds without fetching a single torch wheel.
#
# Each package is resolved with the resolver ITS OWN image uses, because a gate
# that passes with a resolver the build does not use proves nothing about the
# build:
#
#   deepwiki   pip. services/elitea-deepwiki/Containerfile runs `pip wheel`
#              over the extra directly. `pip install --dry-run
#              --ignore-installed` performs the same resolution and stops
#              before installing; modern PyPI serves per-wheel metadata
#              (PEP 658), so pip reads torch's metadata without downloading
#              torch. MEASURED: ~10 s warm, ~3 s to REJECT the #760 bump.
#   inventory  uv. services/elitea-inventory/Containerfile states, measured
#              over six image builds, that pip cannot resolve this closure in
#              any reasonable time (the blanket SDK extras were 356 packages)
#              and hands the job to `uv pip compile`. So does this gate, at the
#              same pinned uv version the Containerfile installs.
#
# THE THIRD CHECK: the closure and the copy move TOGETHER. The pins in an
# `engine` extra are the resolution OF a particular frozen copy. Moving one
# without the other is how a copy ends up running against versions nobody
# resolved it against, and neither the copy's own digest test nor the
# resolution above can see it. So each pyproject carries a `closure-stamp`
# line naming the sha256 of the engine copy's COPY_MANIFEST.json, and this
# script refuses a tree where the two disagree.
# `scripts/ci/refresh-engine-closure.py` is what re-resolves and re-stamps.
#
# Run it locally exactly as CI does:
#
#     bash scripts/ci/check-engine-closures.sh
#
set -euo pipefail

cd "$(dirname "$0")/../.."

# The version services/elitea-inventory/Containerfile installs. Pinned here for
# the same reason it is pinned there: a resolver is a program, and "the latest
# one" is not a reproducible answer to "does this set resolve".
UV_VERSION=0.9.5

# uv resolves for a declared target rather than for the runner. Without these
# the answer depends on whether a maintainer ran the script on macOS
# (faiss-cpu 1.13.2 publishes macosx_14_0_arm64 wheels but no
# manylinux_2_17_x86_64 one, so the default platform tag alone changes the
# verdict). manylinux_2_28 is what the python:3.12-slim runtime images accept.
UV_PYTHON_VERSION=3.12
UV_PLATFORM=x86_64-manylinux_2_28

PYTHON=${ENGINE_CLOSURE_PYTHON:-python3}

fail=0

# uv writes its output atomically: it creates a temporary file in the TARGET's
# directory and renames it. `-o /dev/null` therefore tries to create
# `/dev/.tmpXXXX` and dies with "Operation not permitted" — which this script
# would then have reported as "the closure does not resolve", a false red of
# exactly the kind this gate exists to prevent. A real directory it owns
# instead. `mktemp -d` with an explicit template, because BSD mktemp (macOS)
# requires one.
WORK=$(mktemp -d "${TMPDIR:-/tmp}/engine-closures.XXXXXX")
trap 'rm -rf "$WORK"' EXIT

note() { printf '%s\n' "$*"; }
bad() { printf '::error::%s\n' "$*" >&2; fail=1; }

# ── The interpreter ──────────────────────────────────────────────────────────
#
# pip resolves for the interpreter it runs on, and both packages declare
# `requires-python = ">=3.12,<3.13"`. On 3.13 pip would refuse the package
# itself and the run would be red for a reason that has nothing to do with the
# closure; on 3.11 it would resolve a DIFFERENT set. Check rather than assume.
python_version=$("$PYTHON" -c 'import sys; print("%d.%d" % sys.version_info[:2])')
if [ "$python_version" != "3.12" ]; then
  echo "FAIL: ENGINE_CLOSURE_PYTHON (${PYTHON}) is Python ${python_version}." >&2
  echo "Both packages declare requires-python >=3.12,<3.13, and pip resolves for" >&2
  echo "the interpreter it runs on. Point ENGINE_CLOSURE_PYTHON at a 3.12." >&2
  exit 1
fi
note "interpreter     ${PYTHON} (Python ${python_version})"

# ── uv ───────────────────────────────────────────────────────────────────────
if command -v uv >/dev/null 2>&1 && [ "$(uv --version | awk '{print $2}')" = "$UV_VERSION" ]; then
  UV=(uv)
else
  note "installing uv==${UV_VERSION}"
  "$PYTHON" -m pip install --quiet --disable-pip-version-check "uv==${UV_VERSION}"
  UV=("$PYTHON" -m uv)
fi
note "uv              $("${UV[@]}" --version)"
note ""

# ── The stamp ────────────────────────────────────────────────────────────────
#
# One line per pyproject:
#
#   # closure-stamp: COPY_MANIFEST.json sha256 <64 hex>
#
# `shasum -a 256` on macOS, `sha256sum` on the runner. Both are handled because
# a gate only a runner can run is a gate its author cannot test.
digest() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

check_stamp() {
  local package=$1 manifest=$2
  local pyproject="${package}/pyproject.toml"
  local recorded actual count

  if [ ! -f "$manifest" ]; then
    bad "${package}: no engine copy manifest at ${manifest}. This script's stamp check then measured nothing."
    return
  fi

  recorded=$(sed -n 's/^# closure-stamp: COPY_MANIFEST.json sha256 \([0-9a-f]\{64\}\)$/\1/p' "$pyproject")
  if [ -z "$recorded" ]; then
    bad "${package}/pyproject.toml carries no '# closure-stamp: COPY_MANIFEST.json sha256 <digest>' line. Run: python3 scripts/ci/refresh-engine-closure.py ${package} --stamp-only"
    return
  fi
  count=$(printf '%s\n' "$recorded" | wc -l | tr -d ' ')
  if [ "$count" != "1" ]; then
    bad "${package}/pyproject.toml carries ${count} closure-stamp lines; this reader cannot say which is authoritative."
    return
  fi

  actual=$(digest "$manifest")
  if [ "$recorded" != "$actual" ]; then
    bad "${package}: the engine copy moved and the closure did not.
    COPY_MANIFEST.json is now sha256 ${actual}
    pyproject.toml still stamps    ${recorded}
  The pins in the 'engine' extra ARE the resolution of a particular copy, so
  they move together with it and never one at a time. Re-resolve and re-stamp:
      python3 scripts/ci/refresh-engine-closure.py ${package}
  If the copy changed but the resolution did not, --stamp-only records that."
    return
  fi
  note "${package}: closure-stamp matches COPY_MANIFEST.json (${actual:0:12})"
}

# ── The resolutions ──────────────────────────────────────────────────────────

resolve_with_uv() {
  local package=$1
  shift
  local started elapsed out
  out="${WORK}/$(basename "$package").txt"
  started=$SECONDS
  if "${UV[@]}" pip compile "${package}/pyproject.toml" \
      --python-version "$UV_PYTHON_VERSION" \
      --python-platform "$UV_PLATFORM" \
      --no-annotate --no-header \
      "$@" \
      -o "$out" >/dev/null; then
    elapsed=$((SECONDS - started))
    note "${package}: uv resolved ${*} to $(grep -c . "$out") package(s) in ${elapsed}s"
  else
    elapsed=$((SECONDS - started))
    bad "${package}: uv could NOT resolve ${*} (${elapsed}s). The 'engine' extra is unsatisfiable, so the -engine image cannot be built from this revision. The pins in it are the resolution of the frozen engine copy: move them as a SET with scripts/ci/refresh-engine-closure.py, never one at a time."
  fi
}

resolve_with_pip() {
  local package=$1 extras=$2
  local started elapsed
  started=$SECONDS
  # --ignore-installed so the resolution is of the extra and not of whatever
  # the runner's environment happens to already satisfy; --dry-run so nothing
  # is downloaded past the metadata pip needs to decide.
  if "$PYTHON" -m pip install --dry-run --ignore-installed --quiet \
      --disable-pip-version-check \
      --report "${WORK}/deepwiki-report.json" \
      "./${package}${extras}"; then
    elapsed=$((SECONDS - started))
    note "${package}: pip resolved ${extras} in ${elapsed}s"
  else
    elapsed=$((SECONDS - started))
    bad "${package}: pip could NOT resolve ${extras} (${elapsed}s). This is the exact resolution ${package}/Containerfile performs, so the -engine image cannot be built from this revision. Move the 'engine' extra as a SET with scripts/ci/refresh-engine-closure.py, never one pin at a time."
  fi
}

check_stamp services/elitea-deepwiki \
  services/elitea-deepwiki/src/elitea_deepwiki/engine/COPY_MANIFEST.json
check_stamp services/elitea-inventory \
  services/elitea-inventory/src/elitea_inventory/engine/COPY_MANIFEST.json
note ""

# DeepWiki: pip is the Containerfile's resolver, so pip is the authority here.
resolve_with_pip services/elitea-deepwiki "[engine,storage-postgres]"
# Inventory: uv is the Containerfile's resolver, and its Containerfile records
# why pip is not (six killed builds).
resolve_with_uv services/elitea-inventory --extra engine
note ""

if [ "$fail" -ne 0 ]; then
  echo "FAIL: an engine closure does not resolve, or a closure-stamp is stale." >&2
  exit 1
fi

echo "OK: both frozen engine closures resolve, and both closure-stamps match their engine copy."
