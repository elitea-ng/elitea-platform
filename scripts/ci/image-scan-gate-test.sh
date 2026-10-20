#!/usr/bin/env bash
# image-scan-gate-test.sh — prove that scripts/ci/image-scan-gate.sh goes RED.
#
# ISSUE #501 asks for exactly this: "Prove the gate goes red. Do not assert it."
# A release gate is only worth its runtime if a real finding stops the release,
# and the way that claim usually rots is that nobody ever sees the gate fail.
#
# This test drives the gate with synthetic trivy reports. It needs no registry,
# no image, no network and no trivy — the gate reads a JSON report, so a report
# is all a test needs. Every arm is exercised:
#
#   * a clean report passes;
#   * one HIGH finding fails a blocking image;
#   * one CRITICAL finding fails a blocking image;
#   * a missing report fails         (the scan did not run);
#   * an empty report fails          (the same, written differently);
#   * a report that is not JSON fails (the scan did not finish);
#   * a report without the expected target type fails, EVEN WHEN IT HOLDS NO
#     FINDING — a dead scan must not look like a clean scan;
#   * an exempt image reports its findings and passes;
#   * an exempt image with zero findings FAILS, because the exemption is stale;
#   * the dormancy guards block the exempt image too;
#   * a `blocking` value that is neither true nor false is refused.
#
# Run it directly:
#   bash scripts/ci/image-scan-gate-test.sh
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
GATE="${REPO_ROOT}/scripts/ci/image-scan-gate.sh"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/image-scan-gate-test.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

CHECKS=0
FAILURES=0
good() { CHECKS=$((CHECKS + 1)); echo "  ok: $1"; }
poor() { CHECKS=$((CHECKS + 1)); FAILURES=$((FAILURES + 1)); echo "  FAIL: $1" >&2; }

[ -f "$GATE" ] || { echo "FAIL: ${GATE} is missing" >&2; exit 1; }

# expects <wanted-exit> <description> <report> <image> <expect-type> <blocking>
expects() {
  local wanted="$1" description="$2"
  shift 2
  local output status
  output="$(bash "$GATE" "$@" 2>&1)"
  status=$?
  if [ "$status" -eq "$wanted" ]; then
    good "$description (exit ${status})"
  else
    poor "$description: wanted exit ${wanted}, got ${status}"
    printf '%s\n' "$output" | sed 's/^/        /' >&2
  fi
}

# ── Synthetic reports ────────────────────────────────────────────────────────
#
# The shape is trivy's: Results[] with a Target, a Type and Vulnerabilities[].

cat > "$WORK/clean-gobinary.json" <<'JSON'
{"Results":[{"Target":"/elitea-main","Type":"gobinary","Vulnerabilities":[]}]}
JSON

cat > "$WORK/one-high.json" <<'JSON'
{"Results":[{"Target":"/elitea-main","Type":"gobinary","Vulnerabilities":[
  {"Severity":"HIGH","PkgName":"stdlib","InstalledVersion":"v1.26.4","VulnerabilityID":"CVE-2026-0001","FixedVersion":"1.26.6"}
]}]}
JSON

cat > "$WORK/one-critical.json" <<'JSON'
{"Results":[{"Target":"/elitea-main","Type":"gobinary","Vulnerabilities":[
  {"Severity":"CRITICAL","PkgName":"stdlib","InstalledVersion":"v1.26.4","VulnerabilityID":"CVE-2026-0002"}
]}]}
JSON

# The base layers were read and the application content was not. This is the
# dead-scan shape: it holds no finding, so a count-only gate calls it clean.
cat > "$WORK/os-layers-only.json" <<'JSON'
{"Results":[{"Target":"debian (debian 12.5)","Type":"debian","Vulnerabilities":[]}]}
JSON

cat > "$WORK/exempt-findings.json" <<'JSON'
{"Results":[{"Target":"python-packages","Type":"python-pkg","Vulnerabilities":[
  {"Severity":"HIGH","PkgName":"urllib3","InstalledVersion":"1.0","VulnerabilityID":"CVE-2026-0003"},
  {"Severity":"CRITICAL","PkgName":"pillow","InstalledVersion":"1.0","VulnerabilityID":"CVE-2026-0004"}
]}]}
JSON

cat > "$WORK/exempt-clean.json" <<'JSON'
{"Results":[{"Target":"python-packages","Type":"python-pkg","Vulnerabilities":[]}]}
JSON

: > "$WORK/empty.json"
printf 'not json at all\n' > "$WORK/garbage.json"

# ── A blocking image ─────────────────────────────────────────────────────────
echo "== a blocking image =="
expects 0 "a clean report passes" \
  "$WORK/clean-gobinary.json" elitea-main gobinary true
expects 1 "one HIGH finding stops the release" \
  "$WORK/one-high.json" elitea-main gobinary true
expects 1 "one CRITICAL finding stops the release" \
  "$WORK/one-critical.json" elitea-main gobinary true

# ── The scan must have run, and must have reached the application ────────────
echo "== a scan that did not run, or did not reach the image content =="
expects 1 "a missing report fails" \
  "$WORK/no-such-report.json" elitea-main gobinary true
expects 1 "an empty report fails" \
  "$WORK/empty.json" elitea-main gobinary true
expects 1 "a report that is not JSON fails" \
  "$WORK/garbage.json" elitea-main gobinary true
expects 1 "a report with no gobinary target fails, though it holds no finding" \
  "$WORK/os-layers-only.json" elitea-main gobinary true

# ── An exempt image ──────────────────────────────────────────────────────────
echo "== an exempt image reports and does not block =="
expects 0 "findings are reported and do not stop the release" \
  "$WORK/exempt-findings.json" an-exempt-image python-pkg false
expects 1 "zero findings fail: the exemption has outlived its cause" \
  "$WORK/exempt-clean.json" an-exempt-image python-pkg false
expects 1 "the dormancy guard blocks the exempt image too" \
  "$WORK/os-layers-only.json" an-exempt-image python-pkg false
expects 1 "a missing report blocks the exempt image too" \
  "$WORK/no-such-report.json" an-exempt-image python-pkg false

# ── The gate refuses input it cannot act on ──────────────────────────────────
echo "== input the gate must refuse =="
expects 2 "a blocking value that is neither true nor false is refused" \
  "$WORK/clean-gobinary.json" elitea-main gobinary yes
expects 2 "an empty expected type is refused" \
  "$WORK/clean-gobinary.json" elitea-main "" true

echo ""
echo "${CHECKS} checks, ${FAILURES} failed."
[ "$FAILURES" -eq 0 ] || exit 1
# A run that measured nothing is not a pass. The count below moves only when
# this file gains or loses a check.
if [ "$CHECKS" -lt 13 ]; then
  echo "ERROR: only ${CHECKS} checks ran; this file states 13. A check that stopped running" >&2
  echo "       reports a pass for an arm nobody measured." >&2
  exit 1
fi
