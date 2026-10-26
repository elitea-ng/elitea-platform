#!/usr/bin/env bash
#
# Every dependency manifest in this repository (composite actions included) is either scanned by
# .github/dependabot.yml or exempt here with a reason.
#
# WHY A GATE AT ALL. A dependabot configuration is a list of directories, and
# nothing in GitHub tells you when that list stops matching the tree. A module
# added without an entry is simply never scanned, and the symptom is silence —
# no error, no missing report, just a manifest nobody is watching. That is the
# same shape as every other gate in this repository that reported success while
# measuring nothing, so it gets the same treatment: check the checker.
#
# It fails in BOTH directions, and the second one matters as much as the first:
#
#   1. a tracked manifest with no dependabot entry and no exemption;
#   2. a dependabot entry naming a directory that holds no manifest. That is
#      what a moved or deleted module looks like, and dependabot itself does
#      not complain about it — it just scans nothing.
#
# It also refuses a run that found no manifests at all, because a broken
# `git ls-files` pattern would otherwise satisfy rule 1 vacuously.
set -euo pipefail

cd "$(dirname "$0")/../.."

CONFIG=.github/dependabot.yml

# Manifests that are deliberately NOT scanned. Each line is a path, then
# whitespace, then the reason. An entry here is a decision, not a backlog item.
#
# A newline-delimited string rather than an associative array: macOS ships bash
# 3.2, `declare -A` is a syntax error there, and a gate that only runs on the CI
# runner cannot be checked by the person writing it.
EXEMPT=$(cat <<'EXEMPTIONS'
apps/elitea-mobile/package.json Expo SDK app: its versions are chosen by `npx expo install` against the SDK version table (apps/elitea-mobile/AGENTS.md), and an SDK moves as one reviewed upgrade; a version bump outside that table breaks native builds that no Linux CI job compiles. Security alerts still come from the dependency graph
apps/elitea-web/tools/lint-rules/fixtures/knip/bad/package.json  knip lint-rule fixture: its dependencies are deliberately wrong, and bumping them breaks the rule it exercises
apps/elitea-web/tools/lint-rules/fixtures/knip/good/package.json knip lint-rule fixture, same reason
conformance/provider/pyproject.toml              test harness: a pytest runner and an HTTP client, shipping in no image, run in CI against a container someone else built
libs/ts/client-core/package.json shared client TypeScript with no dependencies at all (no dependencies, devDependencies or lockfile): it runs on node --test and borrows tsc from apps/elitea-web, whose entry scans that compiler. A first dependency here needs a /libs/ts/client-core entry in dependabot.yml and this line removed
libs/rust/vendor/adk-agent/Cargo.toml patched ADK 2.2.0 source (libs/rust/vendor/README.md), patched into the worker and the libs/rust workspace: their Cargo.lock files scan its dependency closure; upstream revision and local patches move together
libs/rust/vendor/adk-runner/Cargo.toml patched ADK 2.2.0 source (libs/rust/vendor/README.md), patched into the worker and the libs/rust workspace: their Cargo.lock files scan its dependency closure; upstream revision and local patches move together
libs/rust/vendor/adk-sandbox/Cargo.toml patched ADK 2.2.0 source (libs/rust/vendor/README.md), patched into the worker only (its optional sandbox-supervisor feature; the libs/rust workspace does not link it): the worker Cargo.lock scans its dependency closure; upstream revision and local patches move together
libs/rust/vendor/zavora-slide/Cargo.toml vendored zavora-slide 0.1.1 with the Elitea patches (libs/rust/vendor/README.md), patched into the worker and the libs/rust workspace: their Cargo.lock files scan its dependency closure; the zavora pins are exact on purpose (ADR-0032 decision 5) and a re-vendor is a reviewed PR that reruns the hostile corpus and the patch tests
libs/rust/vendor/zavora-slide-pdf/Cargo.toml vendored zavora-slide-pdf 0.1.1 with the Elitea patches (libs/rust/vendor/README.md), patched into the worker and the libs/rust workspace: their Cargo.lock files scan its dependency closure; the zavora pins are exact on purpose (ADR-0032 decision 5) and a re-vendor is a reviewed PR that reruns the hostile corpus and the patch tests
libs/rust/vendor/zavora-slide-render/Cargo.toml vendored zavora-slide-render 0.1.1 with the Elitea patches (libs/rust/vendor/README.md), patched into the worker and the libs/rust workspace: their Cargo.lock files scan its dependency closure; the zavora pins are exact on purpose (ADR-0032 decision 5) and a re-vendor is a reviewed PR that reruns the hostile corpus and the patch tests
libs/rust/vendor/leiden-rs/Cargo.toml vendored leiden-rs 0.8.1, the crates.io archive minus its Cargo.lock, pinned by digest (libs/rust/vendor/README.md): each consuming service Cargo.lock scans its dependency closure; it moves by re-vendoring, never one pin at a time
libs/rust/adk-gateway/Cargo.toml member of the libs/rust workspace: the /libs/rust entry updates the workspace root, whose Cargo.lock holds the dependencies of every member
libs/rust/agent-runtime/Cargo.toml member of the libs/rust workspace: the /libs/rust entry updates the workspace root, whose Cargo.lock holds the dependencies of every member
libs/rust/code-parsers/Cargo.toml member of the libs/rust workspace: the /libs/rust entry updates the workspace root, whose Cargo.lock holds the dependencies of every member
libs/rust/engine-core/Cargo.toml member of the libs/rust workspace: the /libs/rust entry updates the workspace root, whose Cargo.lock holds the dependencies of every member
libs/rust/engine-sidecar/Cargo.toml member of the libs/rust workspace: the /libs/rust entry updates the workspace root, whose Cargo.lock holds the dependencies of every member
libs/rust/egress-guard/Cargo.toml member of the libs/rust workspace: the /libs/rust entry updates the workspace root, whose Cargo.lock holds the dependencies of every member
libs/rust/graph-algos/Cargo.toml member of the libs/rust workspace: the /libs/rust entry updates the workspace root, whose Cargo.lock holds the dependencies of every member
libs/rust/inventory-core/Cargo.toml member of the libs/rust workspace: the /libs/rust entry updates the workspace root, whose Cargo.lock holds the dependencies of every member
libs/rust/llm-wire/Cargo.toml member of the libs/rust workspace: the /libs/rust entry updates the workspace root, whose Cargo.lock holds the dependencies of every member
libs/rust/local-embed/Cargo.toml member of the libs/rust workspace: the /libs/rust entry updates the workspace root, whose Cargo.lock holds the dependencies of every member
libs/rust/local-index/Cargo.toml member of the libs/rust workspace: the /libs/rust entry updates the workspace root, whose Cargo.lock holds the dependencies of every member
libs/rust/local-tools/Cargo.toml member of the libs/rust workspace: the /libs/rust entry updates the workspace root, whose Cargo.lock holds the dependencies of every member
libs/rust/local-tools/tests/fixtures/projects/cargo/Cargo.toml run_tests detection fixture (#1224): a dependency-free crate the project detector and a real `cargo test` read; it ships in nothing
libs/rust/local-tools/tests/fixtures/projects/go/go.mod run_tests detection fixture (#1224): a marker the project detector reads, no dependencies, ships in nothing
libs/rust/local-tools/tests/fixtures/projects/jest/package.json run_tests detection fixture (#1224): a marker the project detector reads; never installed
libs/rust/local-tools/tests/fixtures/projects/pytest/pyproject.toml run_tests detection fixture (#1224): a marker the project detector reads; never installed
libs/rust/local-tools/tests/fixtures/projects/vitest/package.json run_tests detection fixture (#1224): a marker the project detector reads; never installed
libs/rust/workspace-runner/Cargo.toml member of the libs/rust workspace: the /libs/rust entry updates the workspace root, whose Cargo.lock holds the dependencies of every member
libs/rust/workspace-store/Cargo.toml member of the libs/rust workspace: the /libs/rust entry updates the workspace root, whose Cargo.lock holds the dependencies of every member
libs/rust/model-client/Cargo.toml member of the libs/rust workspace: the /libs/rust entry updates the workspace root, whose Cargo.lock holds the dependencies of every member
libs/rust/connectors/Cargo.toml member of the libs/rust workspace: the /libs/rust entry updates the workspace root, whose Cargo.lock holds the dependencies of every member
libs/rust/content-source/Cargo.toml member of the libs/rust workspace: the /libs/rust entry updates the workspace root, whose Cargo.lock holds the dependencies of every member
libs/rust/conversation/Cargo.toml member of the libs/rust workspace: the /libs/rust entry updates the workspace root, whose Cargo.lock holds the dependencies of every member
libs/rust/doc-extract/Cargo.toml member of the libs/rust workspace: the /libs/rust entry updates the workspace root, whose Cargo.lock holds the dependencies of every member
libs/rust/doc-chunk/Cargo.toml member of the libs/rust workspace: the /libs/rust entry updates the workspace root, whose Cargo.lock holds the dependencies of every member
libs/rust/office-core/Cargo.toml member of the libs/rust workspace: the /libs/rust entry updates the workspace root, whose Cargo.lock holds the dependencies of every member; the zavora pins are exact on purpose (ADR-0032 decision 5) and move only by a reviewed PR that reruns the hostile corpus
libs/rust/office-core/fuzz/Cargo.toml cargo-fuzz harness of office-core, not a workspace member and never built by CI (needs nightly and libFuzzer); it has no dependencies beyond libfuzzer-sys and office-core itself
libs/rust/pg-migrate/Cargo.toml member of the libs/rust workspace: the /libs/rust entry updates the workspace root, whose Cargo.lock holds the dependencies of every member
libs/rust/repo-ingest/Cargo.toml member of the libs/rust workspace: the /libs/rust entry updates the workspace root, whose Cargo.lock holds the dependencies of every member
libs/rust/vector-index/Cargo.toml member of the libs/rust workspace: the /libs/rust entry updates the workspace root, whose Cargo.lock holds the dependencies of every member
libs/rust/model-rpc/Cargo.toml member of the libs/rust workspace: the /libs/rust entry updates the workspace root, whose Cargo.lock holds the dependencies of every member
libs/rust/platform-rpc/Cargo.toml member of the libs/rust workspace: the /libs/rust entry updates the workspace root, whose Cargo.lock holds the dependencies of every member
scripts/runtime/fixtures/code-data-processing/Cargo.toml locked acceptance fixture: package versions are test inputs; runner and worker scan shipping dependencies
EXEMPTIONS
)

is_exempt() {
  printf '%s\n' "$EXEMPT" | grep -q "^$1[[:space:]]"
}

# A composite action's action.yml is the github-actions manifest: the `uses:`
# pins inside it are dependencies like any other, and the `/` entry (which
# covers .github/workflows) does not reach them.
manifests=$(git ls-files \
  | grep -E '(^|/)(go\.mod|Cargo\.toml|package\.json|pyproject\.toml)$|^\.github/actions/(.+/)?action\.ya?ml$' \
  | grep -v node_modules || true)

if [ -z "$manifests" ]; then
  echo "FAIL: no dependency manifests were found at all." >&2
  echo "The git ls-files pattern in this script matched nothing, so the coverage" >&2
  echo "check below would have passed without checking anything." >&2
  exit 1
fi

# The directories dependabot names, normalised to a repo-relative path with no
# leading slash. "/" becomes the empty string, which is the repository root.
#
# [[:space:]] rather than \s, deliberately. BSD sed (macOS) does not know \s
# and leaves the line untouched, so every path here would have kept its
# "    directory: " prefix and matched nothing — both directions of the check
# would have failed on every entry. That is a loud failure rather than a silent
# pass, which is the only reason it was caught on the first run.
# Both spellings: `directory: /x` and a `directories:` list of `- /x` items.
# Multi-directory entries keep one entry per ecosystem; the multi-ecosystem group in dependabot.yml folds them into one pull request.
configured=$(awk '
  /^[[:space:]]+directory:/      { sub(/^[[:space:]]+directory:[[:space:]]*/, ""); print; next }
  /^[[:space:]]+directories:/    { inlist = 1; next }
  # A comment inside the list (a note on one directory) does not end it.
  inlist && /^[[:space:]]*#/     { next }
  inlist && /^[[:space:]]+-[[:space:]]*\//  { sub(/^[[:space:]]+-[[:space:]]*/, ""); print; next }
  inlist && !/^[[:space:]]+-/    { inlist = 0 }
' "$CONFIG" | sed -E 's|^/?||; s|/$||')

fail=0

for manifest in $manifests; do
  if is_exempt "$manifest"; then
    continue
  fi
  dir=$(dirname "$manifest")
  [ "$dir" = "." ] && dir=""
  if ! printf '%s\n' "$configured" | grep -qxF "$dir"; then
    echo "FAIL: $manifest has no entry in $CONFIG and no exemption in this script." >&2
    fail=1
  fi
done

# Direction 2: an entry that points nowhere.
while IFS= read -r dir; do
  [ -z "$dir" ] && continue   # the repository root, for github-actions
  # ANY one of them is enough. The first version of this asked `ls` for
  # all four at once, and `ls` exits non-zero when ANY operand is missing — so
  # every real directory failed, because no directory holds all four.
  found=0
  for name in go.mod Cargo.toml package.json pyproject.toml action.yml action.yaml; do
    if [ -f "$dir/$name" ]; then
      found=1
      break
    fi
  done
  if [ "$found" -eq 0 ]; then
    echo "FAIL: $CONFIG scans '$dir', which holds no dependency manifest." >&2
    echo "      A moved or deleted module leaves an entry that scans nothing." >&2
    fail=1
  fi
done <<< "$configured"

if [ "$fail" -ne 0 ]; then
  exit 1
fi

echo "OK: $(printf '%s\n' "$manifests" | wc -l | tr -d ' ') manifests, all scanned or exempt."
