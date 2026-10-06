#!/usr/bin/env bash
# image-scan-gate.sh — read one trivy report and decide whether it blocks.
#
# TWO WORKFLOWS CALL THIS, AND THAT IS THE POINT.
#
#   * .github/workflows/ci-image-scan.yml builds each image and scans it, on
#     every pull request, every push to main, and daily at 05:17 UTC.
#   * .github/workflows/publish.yml scans the image the RELEASE PUSHED, before
#     the manifest job creates the `latest` tag (#501).
#
# The rule the two must share is which report blocks. A second copy of that
# rule drifts, and a release gate that is a little more forgiving than the
# merge gate is worse than no release gate: it reports the same words for a
# weaker test.
#
# ── THREE FAILURES, NOT ONE ──────────────────────────────────────────────────
#
#   1. No report at all: the scan did not run.
#   2. A report without the expected target type: the scan ran, and never
#      reached what the image ships. `expect_type` names the APPLICATION
#      content (a Go binary, the site-packages of a python image, the package
#      database of an alpine runtime), so a scan that reads the base layers and
#      stops short fails instead of reading as clean.
#   3. Any HIGH or CRITICAL finding, with the list.
#
# Failures 1 and 2 BLOCK FOR EVERY IMAGE, the exempt ones included. An
# exemption covers a known finding set this repository cannot fix. It does not
# cover a scan that stopped running. That distinction is the whole subject of
# #433: a dead `FROM aquasec/trivy` stage was pruned from every build graph, so
# the scan never ran once and nothing said so.
#
# ── THE EXEMPT ARM ───────────────────────────────────────────────────────────
#
# `blocking=false` reports the finding count and does not fail. Two files must
# agree before an image carries it: the `blocking` field in the scan matrix and
# `.github/image-scan-exempt.txt`, which records the cause and the issue that
# ends it. The coverage job in ci-image-scan.yml fails when they differ.
#
# An exempt image that reports ZERO findings FAILS. The cause of the exemption
# is then gone, and a waiver that outlives its reason is the next gate nobody
# reads.
#
# ── USE ──────────────────────────────────────────────────────────────────────
#
#   scripts/ci/image-scan-gate.sh <report.json> <image-name> <expect-type> <blocking>
#
# `blocking` is the string `true` or `false`. Any other value is refused: a
# typo must not silently turn a blocking image into an exempt one.
#
# It writes a summary block to $GITHUB_STEP_SUMMARY when that variable is set,
# and it needs only jq. scripts/ci/image-scan-gate-test.sh drives every arm
# with synthetic reports, so the gate is proved to go red rather than asserted.
set -euo pipefail

if [ "$#" -ne 4 ]; then
  echo "usage: $0 <trivy-report.json> <image-name> <expect-type> <true|false>" >&2
  exit 2
fi

REPORT="$1"
IMAGE_NAME="$2"
EXPECT_TYPE="$3"
BLOCKING="$4"

case "$BLOCKING" in
  true | false) ;;
  *)
    echo "::error::blocking must be \"true\" or \"false\", got \"${BLOCKING}\". A typo must not" >&2
    echo "         turn a blocking image into an exempt one." >&2
    exit 2
    ;;
esac

if [ -z "$EXPECT_TYPE" ]; then
  echo "::error::no expected target type for ${IMAGE_NAME}. Without it a report that never" >&2
  echo "         reached the application content reads as clean." >&2
  exit 2
fi

command -v jq >/dev/null || { echo "::error::jq is not on PATH; this gate reads a JSON report" >&2; exit 2; }

# ── 1. The scan ran ──────────────────────────────────────────────────────────
if [ ! -s "$REPORT" ]; then
  echo "::error::trivy wrote no report for ${IMAGE_NAME} — the scan did not run"
  exit 1
fi
if ! jq -e . "$REPORT" > /dev/null 2>&1; then
  echo "::error::the ${IMAGE_NAME} report is not valid JSON — the scan did not finish"
  exit 1
fi

echo "Scanned targets:"
jq -r '.Results[]? | "  \(.Target) [\(.Type)]"' "$REPORT"

# ── 2. The scan reached what the image ships ─────────────────────────────────
if ! jq -e --arg t "$EXPECT_TYPE" '[.Results[]? | select(.Type == $t)] | length > 0' "$REPORT" > /dev/null; then
  echo "::error::the ${IMAGE_NAME} report holds no \"${EXPECT_TYPE}\" target — the scan did not reach what the image ships"
  exit 1
fi

# ── 3. The findings ──────────────────────────────────────────────────────────
FINDINGS="$(mktemp "${TMPDIR:-/tmp}/image-scan-findings.XXXXXX")"
trap 'rm -f "$FINDINGS"' EXIT

jq -r '
  .Results[]? | .Target as $target
  | .Vulnerabilities[]?
  | "\(.Severity)\t\($target)\t\(.PkgName) \(.InstalledVersion)\t\(.VulnerabilityID)\tfixed in \(.FixedVersion // "nothing yet")"
' "$REPORT" | sort -u | tee "$FINDINGS"

count="$(grep -c . "$FINDINGS" || true)"
critical="$(grep -c '^CRITICAL' "$FINDINGS" || true)"
high="$(grep -c '^HIGH' "$FINDINGS" || true)"

if [ "$BLOCKING" = "true" ]; then
  if [ "$count" -gt 0 ]; then
    echo "::error::${count} HIGH or CRITICAL vulnerability line(s) in ${IMAGE_NAME} — raise the pinned versions, do not lower this threshold"
    exit 1
  fi
  echo "No HIGH or CRITICAL vulnerability in ${IMAGE_NAME}."
  exit 0
fi

# ── The exempt arm ───────────────────────────────────────────────────────────
if [ "$count" -eq 0 ]; then
  echo "::error::${IMAGE_NAME} reports 0 findings, so its exemption is no longer needed — delete the entry in .github/image-scan-exempt.txt or set blocking: true in .github/workflows/ci-image-scan.yml"
  exit 1
fi

echo "::warning::${IMAGE_NAME}: ${count} HIGH or CRITICAL line(s) (${critical} CRITICAL, ${high} HIGH). This image is EXEMPT and does not block. Its cause, and the issue that ends the exemption, are recorded in .github/image-scan-exempt.txt."
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  {
    echo "### Scan ${IMAGE_NAME} — EXEMPT, reports but does not block"
    echo ""
    echo "| Severity | Lines |"
    echo "|---|---|"
    echo "| CRITICAL | ${critical} |"
    echo "| HIGH | ${high} |"
    echo "| total | ${count} |"
    echo ""
    echo "\`.github/image-scan-exempt.txt\` records why this image is exempt and"
    echo "which issue ends the exemption. Read it before you add a second one."
    echo ""
    echo "The threshold stays at HIGH,CRITICAL. No CVE is suppressed, and the"
    echo "dormancy guards above still block: a missing report, or a report"
    echo "without a \`${EXPECT_TYPE}\` target, fails this job like any other."
  } >> "$GITHUB_STEP_SUMMARY"
fi
exit 0
