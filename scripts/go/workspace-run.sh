#!/usr/bin/env bash
# Run one Go command over EVERY module of the go.work workspace, then report a
# single status that is true.
#
# WHY THIS SCRIPT EXISTS (#409)
#
# `task test`, `task vet`, `task coverage` and the ci-go.yml "Run tests
# (workspace)" step each inlined the same shell loop:
#
#     for mod in $(go list -m -f '{{.Dir}}'); do
#       (cd "$mod" && go test -race -count=1 ./...)
#     done
#
# A shell loop takes the exit status of its LAST iteration. So a failed module
# followed by a passed module read as a pass. The loop only failed at all
# because the caller happened to run it under `set -e`. That is an accident of
# the caller, not a property of the loop. The coverage variant was worse: each
# subshell ended with `[ -f coverage.out ] && strip-generated.sh`, and that
# trailing test owned the subshell status.
#
# `task test` is the command every agent in this repository uses to decide
# whether its work is correct. While it can exit 0 over a failed module, every
# verification claim in this repository is unsound. So this script states its
# own rules and does not depend on the caller:
#
#   1. It runs EVERY module. It does not stop at the first failure, because a
#      partial run tells you less than a complete one.
#   2. It records each module status, then exits non-zero if ANY status is
#      non-zero. The last module does not own the result.
#   3. It NAMES every failed module, at the point of failure and in the summary.
#   4. No trailing command owns the status. Every exit is explicit.
#   5. An empty module list is a FAILURE, not an empty loop that passes. A
#      discovery command that fails is a FAILURE for the same reason.
#   6. It names the Go modules on disk that this run did NOT cover, so a green
#      summary can never be read as "the whole repository was tested".
#   7. It NAMES every test that skipped itself, and how many (#423). `go test`
#      prints `ok` for a package whose tests all called `t.Skip`, so a suite
#      that never runs and a suite that passes look the same in the log.
#      Eighteen suites did exactly that. See the skip ledger below.
#
# Usage:
#   scripts/go/workspace-run.sh test              # go test -race -count=1 ./...
#   scripts/go/workspace-run.sh vet               # go vet ./...
#   scripts/go/workspace-run.sh lint              # golangci-lint run ./...
#   scripts/go/workspace-run.sh coverage          # go test -coverprofile, then strip
#   scripts/go/workspace-run.sh coverage --race   # the same, with the race detector
#
# `test` always enables the race detector. `coverage` enables it only with
# `--race`, because CI wants one run that does both and a local profile run
# does not need to pay for it. The flag is written at the call site on purpose:
# an environment variable would drift out of sight.
#
# WHY `lint` IS HERE TOO (#427)
#
# The ci-go.yml Lint job named two modules in a hand-written matrix, and
# `task lint` ran in one module. The workspace holds nine. So an errcheck,
# staticcheck or unused finding in gen/go, libs/go/**, libs/proto/gen/go or
# tools/uictl passed every gate in this repository. The head of .golangci.yml
# said "Applied to every module". That was a statement of intent, not of fact.
#
# The same six rules above now hold for lint as well. Point 6 matters most: a
# hand-written matrix cannot name what it left out, so a module added to
# go.work stayed unlinted in silence. This list is read from go.work at run
# time, and the modules outside it are printed by name.
#
# Environment:
#   ELITEA_GO_TEST_TIMEOUT        per-package `go test -timeout` (default below)
#   GOLANGCI_LINT                 golangci-lint binary to use (default: golangci-lint)
#   ELITEA_REQUIRE_DECLARED_SKIPS 1 makes an undeclared skip FAIL the run.
#                                 CI sets it. Locally the ledger only reports,
#                                 because a developer without PostgreSQL,
#                                 NATS or the storage emulators should still
#                                 be able to run `task test`. Same shape as
#                                 CONTRACT_REQUIRE_PARITY in ci-contract.yml.
#
# `set -e` is deliberately absent: this script must survive a module failure to
# run the next module. Every status is therefore checked by hand.
set -uo pipefail

# Per-package `go test -timeout`. Go's own default is 10m.
#
# WHY THE VALUE IS 45m (#409 item 2)
#
# A `-timeout` exists to stop a HANG. A hang is unbounded, so any value catches
# it. Slowness is bounded by the machine, so a value that also polices slowness
# turns a slow machine into a mystery failure. The value below is therefore far
# above every measured duration, on purpose.
#
# services/elitea-main/internal/infra/db/repos runs 89 PostgreSQL integration
# tests. Each test runs CREATE DATABASE, and 78 of them used to replay the full
# embedded migration set. The package was expensive by construction. #409 named
# it. It is not alone.
#
# Package durations from a full `task test`, macOS arm64, `-race -count=1`,
# PostgreSQL 16 in podman. The right column is the SAME command while a second
# complete Go suite ran at the same time:
#
#   package                                   load ~5     load 50-60
#   internal/infra/db/repos                     638 s     1500 s (hit a 25m cap)
#   internal/api/v2/admin                       555 s     1384 s
#   internal/api/v2/eliteacore                  413 s     1055 s
#   internal/application/projectprovisioning    287 s      771 s
#   internal/api/v2/mcp                         278 s      637 s
#   ---------------------------------------------------------------
#   Go default per-package timeout               600 s      600 s
#
# Read the left column first. On a QUIET machine, `repos` already takes 638 s
# against a 600 s default. It does not sit near the cap. It is past it. And
# `admin` at 555 s is 92 percent of the way there. Under load, five packages
# pass 600 s. #409 reported 353 s alone and 525 s to 601 s in a full run, which
# is the same package on a faster day.
#
# 45m clears the worst observation with room. A hung test still fails, later,
# with the same goroutine dump.
#
# WHY 45m STAYS AFTER #425
#
# #425 removed the replay. `repos` and `projectprovisioning` now copy one
# template database instead. Both packages halved on CI, measured against three
# packages that #425 does not touch:
#
#   package                                   base median     after
#   internal/infra/db/repos                      89.7 s      44.6 s
#   internal/application/projectprovisioning     19.3 s       9.7 s
#   internal/api/v2/admin       (control)        34.2 s      31.1 s
#   internal/api/v2/eliteacore  (control)        20.7 s      18.1 s
#   internal/api/v2/mcp         (control)        14.7 s      12.7 s
#
# Apply that halving to the table above and the two `repos` observations become
# about 320 s and 750 s. 750 s is still past the 600 s Go default, so the
# default is still not safe. 45m is about 3.6x the new worst case, which is the
# margin this comment asks for. Keep it. A large timeout costs nothing when the
# tests pass, and #409 measured what a small one costs when they are slow.
: "${ELITEA_GO_TEST_TIMEOUT:=45m}"

usage() {
    cat >&2 <<'USAGE'
usage: scripts/go/workspace-run.sh <test|vet|lint|coverage> [--race]

  test      go test -race -count=1 ./...    in every workspace module
  vet       go vet ./...                    in every workspace module
  lint      golangci-lint run ./...         in every workspace module
  coverage  go test -coverprofile=coverage.out ./..., then strip generated code
  --race    add -race to `coverage` (`test` already uses it)
USAGE
}

mode="${1:-}"
case "$mode" in
test | vet | lint | coverage) ;;
*)
    usage
    exit 2
    ;;
esac
shift

race_flags=()
while [ "$#" -gt 0 ]; do
    case "$1" in
    --race)
        if [ "$mode" = "vet" ] || [ "$mode" = "lint" ]; then
            echo "workspace-run: --race does not apply to ${mode}" >&2
            exit 2
        fi
        race_flags=(-race)
        ;;
    *)
        echo "workspace-run: unknown option: $1" >&2
        usage
        exit 2
        ;;
    esac
    shift
done

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -z "$script_dir" ]; then
    echo "workspace-run: cannot resolve the script directory" >&2
    exit 1
fi
repo_root="$(cd "$script_dir/../.." && pwd)"
if [ -z "$repo_root" ]; then
    echo "workspace-run: cannot resolve the repository root" >&2
    exit 1
fi

# ── 0. Preflight for `lint` ──────────────────────────────────────────────────
#
# A missing linter must STOP the run. It must not print a note and continue,
# because "the linter was absent" and "the linter found nothing" then produce
# the same green step, which is the defect this script exists to remove.
#
# The config path is passed to golangci-lint explicitly. Without it the tool
# walks up from each module directory and picks the first config it meets, so
# a config dropped into one module would quietly change what that module is
# held to. .golangci.yml says "Applied to every module"; naming the file makes
# that sentence a fact instead of a hope.
: "${GOLANGCI_LINT:=golangci-lint}"
golangci_config="$repo_root/.golangci.yml"
if [ "$mode" = "lint" ]; then
    if ! command -v "$GOLANGCI_LINT" >/dev/null 2>&1; then
        echo "workspace-run: ${GOLANGCI_LINT} is not on PATH; refusing to report success" >&2
        echo "workspace-run: install it, or set GOLANGCI_LINT to the binary" >&2
        exit 1
    fi
    if [ ! -f "$golangci_config" ]; then
        echo "workspace-run: ${golangci_config} is missing; refusing to lint without it" >&2
        exit 1
    fi
    "$GOLANGCI_LINT" --version
fi

# ── 1. Discover the workspace modules ────────────────────────────────────────
#
# go.work wildcards must resolve within a module root, so `./services/...`
# fails with "directory prefix does not contain modules". Ask the go tool for
# the real module directories instead of guessing a glob.
#
# Read the status of `go list` on its own line. `x="$(cmd)"` reports the status
# of cmd only because there is nothing else on the line. Keep it that way.
#
# stderr is NOT merged into stdout here. `go` writes progress lines such as
# "go: downloading ..." to stderr, and a merge would put them in the module
# list. Let them go to the terminal instead.
module_output="$(cd "$repo_root" && go list -m -f '{{.Dir}}')"
discovery_status=$?
if [ "$discovery_status" -ne 0 ]; then
    echo "workspace-run: module discovery failed (go list -m exit ${discovery_status})" >&2
    exit 1
fi

# bash 3.2 runs this on macOS, so no `mapfile` and no associative arrays.
modules=()
while IFS= read -r module_dir; do
    [ -n "$module_dir" ] || continue
    # Every entry must be a real directory. A line that is not one means the
    # output is not the list this script expects, and a wrong list produces a
    # wrong verdict.
    if [ ! -d "$module_dir" ]; then
        echo "workspace-run: go list -m named '${module_dir}', which is not a directory" >&2
        exit 1
    fi
    modules+=("$module_dir")
done <<<"$module_output"

# An empty list must never read as "every module passed". That is the same
# defect in another costume: an absence of failure taken for a pass.
if [ "${#modules[@]}" -eq 0 ]; then
    echo "workspace-run: go list -m named no modules; refusing to report success" >&2
    exit 1
fi

# ── 2. Name the Go modules this run does NOT cover ───────────────────────────
#
# services/elitea-llm-gateway is on a newer Go release and sits outside
# go.work on purpose. It builds with GOWORK=off and has its own ci-gateway.yml.
# That separation is correct. A summary that says "all modules passed" after
# never opening it is not. This block finds every go.mod on disk, subtracts the
# workspace modules, and prints the difference. A new out-of-workspace module
# is therefore named on the day somebody adds it.
found_output="$(find "$repo_root" -name go.mod -type f \
    -not -path '*/node_modules/*' \
    -not -path '*/.git/*' \
    -not -path '*/vendor/*' 2>/dev/null | sort)"
find_status=$?
if [ "$find_status" -ne 0 ]; then
    echo "workspace-run: cannot list the go.mod files on disk (find exit ${find_status})" >&2
    exit 1
fi

on_disk=()
while IFS= read -r module_file; do
    [ -n "$module_file" ] || continue
    module_dir="$(cd "$(dirname "$module_file")" && pwd)" || continue
    on_disk+=("$module_dir")
done <<<"$found_output"

# The workspace modules each own a go.mod, so `find` must see at least as many
# as `go list -m` named. Fewer means the search itself is broken, and a broken
# search reports "nothing uncovered", which is a false green.
if [ "${#on_disk[@]}" -lt "${#modules[@]}" ]; then
    echo "workspace-run: found ${#on_disk[@]} go.mod file(s) but the workspace names ${#modules[@]} module(s)" >&2
    echo "workspace-run: the coverage report would be wrong; refusing to continue" >&2
    exit 1
fi

uncovered=()
for module_dir in "${on_disk[@]}"; do
    in_workspace=0
    for known in "${modules[@]}"; do
        if [ "$known" = "$module_dir" ]; then
            in_workspace=1
            break
        fi
    done
    if [ "$in_workspace" -eq 0 ]; then
        uncovered+=("$module_dir")
    fi
done

# ── 3. Prepare the skip ledger ───────────────────────────────────────────────
#
# WHY (#423)
#
# `go test ./...` prints `ok` for a package whose every test called `t.Skip`.
# Eighteen suites did that on every CI run: the job was green and the coverage
# was zero. `-v` would show the skips, but it also prints a line per passing
# test across the whole workspace, which is why nobody turned it on.
#
# `go test -json` carries a `skip` action per test, so the run can name every
# skip while the human log stays the size it was. skip-ledger.py rebuilds the
# non-verbose log from the event stream and appends each skip, with its
# reason, to one ledger shared by every module. skip-gate.py reports the
# ledger and, under ELITEA_REQUIRE_DECLARED_SKIPS=1, fails on a skip that
# scripts/go/declared-skips.txt does not explain.
#
# `vet` and `lint` produce no test events, so they use neither. The test is
# positive, not a list of what to leave out: a mode added later that runs no
# test must not be made to show a ledger it cannot fill.
skip_ledger=""
skip_filter=""
skip_gate=""
declared_skips="$repo_root/scripts/go/declared-skips.txt"
if [ "$mode" = "test" ] || [ "$mode" = "coverage" ]; then
    skip_filter="$script_dir/skip-ledger.py"
    skip_gate="$script_dir/skip-gate.py"
    for helper in "$skip_filter" "$skip_gate"; do
        if [ ! -f "$helper" ]; then
            echo "workspace-run: ${helper} is missing; the run could not report its skips" >&2
            exit 1
        fi
    done
    if [ ! -f "$declared_skips" ]; then
        echo "workspace-run: ${declared_skips} is missing; the run could not judge its skips" >&2
        exit 1
    fi
    # A missing python3 must stop the run, not silently drop the ledger. A
    # gate that quietly does nothing is the defect this ledger exists to find.
    if ! command -v python3 >/dev/null 2>&1; then
        echo "workspace-run: python3 is required to report skipped tests (#423)" >&2
        exit 1
    fi
    # A caller may name the ledger file to keep it (CI can then upload it).
    # Otherwise it is a temporary file this script removes on the way out.
    if [ -n "${ELITEA_SKIP_LEDGER:-}" ]; then
        skip_ledger="$ELITEA_SKIP_LEDGER"
        : >"$skip_ledger"
        if [ ! -f "$skip_ledger" ]; then
            echo "workspace-run: cannot write the skip ledger at ${skip_ledger}" >&2
            exit 1
        fi
    else
        skip_ledger="$(mktemp -t elitea-skip-ledger.XXXXXX)"
        if [ -z "$skip_ledger" ] || [ ! -f "$skip_ledger" ]; then
            echo "workspace-run: cannot create the skip ledger file" >&2
            exit 1
        fi
        trap 'rm -f "$skip_ledger"' EXIT
    fi
    export ELITEA_SKIP_LEDGER="$skip_ledger"
fi

# ── 4. Run every module and record every status ──────────────────────────────
relative_to_root() {
    case "$1" in
    "$repo_root") echo "." ;;
    "$repo_root"/*) echo "${1#"$repo_root"/}" ;;
    *) echo "$1" ;;
    esac
}

# `-o pipefail` is set at the top of this script, so the pipeline below reports
# the `go test` status even though python3 is the last command in it. Without
# pipefail a failed test run would read as a pass, which is #409 again.
run_test() {
    go test -json -race -count=1 -timeout "$ELITEA_GO_TEST_TIMEOUT" ./... |
        python3 "$skip_filter"
}

run_vet() {
    go vet ./...
}

# golangci-lint exits 1 when it reports issues and 3 (or higher) when it cannot
# run at all — a broken config, a package that will not load. Both are failures
# here, and both name the module in the summary below.
run_lint() {
    "$GOLANGCI_LINT" run --config "$golangci_config" ./...
}

# Generated code leaves the PROFILE, not the test run: oapi-codegen's
# internal/api/generated and sqlc's internal/db/sqlcgen are about 3.4k
# instrumented blocks that nobody writes tests for. Leaving them in moves the
# number on every regeneration for reasons unrelated to the tests. The
# codegen-freshness and conformance gates hold their contract instead.
#
# The old inline form ran `go test ...` and then
# `[ -f coverage.out ] && strip-generated.sh coverage.out`. That trailing test
# became the subshell exit status, so it could overwrite a `go test` failure.
# Here the test status is read first, and a failed test always wins.
#
# `local test_status=$?` is correct — the shell expands `$?` before it runs `local`.
# It is still written on its own line, immediately after the command, so no
# later reader has to know that rule.
run_coverage() {
    go test -json "${race_flags[@]+"${race_flags[@]}"}" -count=1 -timeout "$ELITEA_GO_TEST_TIMEOUT" -coverprofile=coverage.out ./... |
        python3 "$skip_filter"
    local test_status=$?
    local strip_status=0
    if [ -f coverage.out ]; then
        "$repo_root/scripts/coverage/strip-generated.sh" coverage.out || strip_status=$?
    fi
    if [ "$test_status" -ne 0 ]; then
        return "$test_status"
    fi
    return "$strip_status"
}

statuses=()
failed=()
for module_dir in "${modules[@]}"; do
    module_name="$(relative_to_root "$module_dir")"
    echo "==> workspace-run ${mode}: ${module_name}"
    (
        cd "$module_dir" || exit 1
        case "$mode" in
        test) run_test ;;
        vet) run_vet ;;
        lint) run_lint ;;
        coverage) run_coverage ;;
        esac
    )
    module_status=$?
    statuses+=("$module_status")
    if [ "$module_status" -ne 0 ]; then
        failed+=("$module_name")
        # Say it at the point of failure as well. A summary ten minutes later
        # is easy to scroll past.
        echo "workspace-run: FAILED ${mode}: ${module_name} (exit ${module_status})" >&2
    fi
done

# ── 5. Report ────────────────────────────────────────────────────────────────
echo
echo "workspace-run summary (${mode}): ${#modules[@]} workspace module(s)"
index=0
while [ "$index" -lt "${#modules[@]}" ]; do
    module_name="$(relative_to_root "${modules[$index]}")"
    if [ "${statuses[$index]}" -eq 0 ]; then
        echo "  PASS  ${module_name}"
    else
        echo "  FAIL  ${module_name} (exit ${statuses[$index]})"
    fi
    index=$((index + 1))
done

if [ "${#uncovered[@]}" -gt 0 ]; then
    echo "  NOT COVERED by this command — each module needs its own run:"
    for module_dir in "${uncovered[@]}"; do
        module_name="$(relative_to_root "$module_dir")"
        if [ "$module_name" = "services/elitea-llm-gateway" ]; then
            echo "    ${module_name} — builds with GOWORK=off; ci-gateway.yml runs it"
        else
            echo "    ${module_name}"
        fi
    done
fi

# The skip ledger is read even when a module failed, because both answers
# matter and a reader should get them in one place.
skip_status=0
if [ -n "$skip_ledger" ]; then
    python3 "$skip_gate" "$skip_ledger" "$declared_skips"
    skip_status=$?
fi

if [ "${#failed[@]}" -gt 0 ]; then
    echo
    echo "workspace-run: ${#failed[@]} module(s) failed ${mode}:" >&2
    for module_name in "${failed[@]}"; do
        echo "  ${module_name}" >&2
    done
    exit 1
fi

if [ "$skip_status" -ne 0 ]; then
    echo "workspace-run: every module passed ${mode}, but the skip ledger did not (#423)" >&2
    exit "$skip_status"
fi

echo "workspace-run: every workspace module passed ${mode}"
exit 0
