#!/usr/bin/env python3
"""Keep ONE GitHub issue per published image in step with its nightly scan.

.github/workflows/nightly-image-scan.yml scans what `ghcr.io/<org>/<image>:latest`
resolves to every night and runs this once per image with the trivy report:

  findings at or above the threshold, no issue     -> open one
  findings, an OPEN issue with the title           -> rewrite its body; comment
                                                      only when the finding set
                                                      changed (no nightly noise)
  findings, only a CLOSED issue with the title     -> reopen it, with a comment
  no findings, an open issue                       -> comment and close it
  no findings, no open issue                       -> nothing

The title is stable, `Image scan: <image> — vulnerabilities in latest`, and it
is the dedupe key: one image, one issue, for ever.

A report that does not prove the scan ran (missing, not JSON, or without the
expected target type) is REFUSED with exit 2 and touches no issue. "No
findings" must never be read off a scan that never reached the image — that is
the #433 shape, and here it would also close an open issue on a dead scan.
scripts/ci/image-scan-gate.sh, run after this, fails the job for the same
report.

Only the standard library and the `gh` CLI (GH_TOKEN with issues: write).
scripts/ci/test_image_scan_issue.py covers every arm with a fake `gh`.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import pathlib
import subprocess
import sys
from typing import Callable, Optional, Sequence

LABELS = ("security", "image-scan")
SEVERITY_ORDER = {"CRITICAL": 0, "HIGH": 1}
FINGERPRINT_PREFIX = "<!-- image-scan-fingerprint: "
# GitHub refuses an issue body above 65536 characters. A table that long is
# cut, with the count of the rows left out, rather than failing the update.
MAX_ROWS = 300


class ReportError(Exception):
    """The report does not prove the scan ran; nothing may be concluded."""


def title_for(image: str) -> str:
    return f"Image scan: {image} — vulnerabilities in latest"


def load_findings(report_path: pathlib.Path, expect_type: str) -> list[dict]:
    if not report_path.is_file() or report_path.stat().st_size == 0:
        raise ReportError(f"no report at {report_path}: the scan did not run")
    try:
        report = json.loads(report_path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        raise ReportError(f"the report is not valid JSON ({exc}): the scan did not finish") from exc
    results = report.get("Results") or []
    if not any(r.get("Type") == expect_type for r in results):
        raise ReportError(
            f"the report holds no {expect_type!r} target: the scan did not reach what the image ships"
        )
    rows = set()
    for result in results:
        for vuln in result.get("Vulnerabilities") or []:
            rows.add(
                (
                    vuln.get("Severity", "UNKNOWN"),
                    vuln.get("VulnerabilityID", "?"),
                    vuln.get("PkgName", "?"),
                    vuln.get("InstalledVersion", "?"),
                    vuln.get("FixedVersion") or "",
                    result.get("Target", "?"),
                )
            )
    findings = [
        dict(zip(("severity", "id", "package", "installed", "fixed", "target"), row))
        for row in rows
    ]
    findings.sort(key=lambda f: (SEVERITY_ORDER.get(f["severity"], 9), f["id"], f["package"], f["target"]))
    return findings


def fingerprint(findings: Sequence[dict]) -> str:
    """Stable over the finding SET; the digest and the run do not enter it."""
    text = "\n".join(
        "|".join((f["severity"], f["id"], f["package"], f["installed"], f["fixed"]))
        for f in findings
    )
    return hashlib.sha256(text.encode("utf-8")).hexdigest()[:16]


def _cell(text: str) -> str:
    return text.replace("|", "\\|").replace("\n", " ")


def render_body(image: str, findings: Sequence[dict], *, digest: str, version: str,
                run_url: str, exempt: bool) -> str:
    critical = sum(1 for f in findings if f["severity"] == "CRITICAL")
    high = sum(1 for f in findings if f["severity"] == "HIGH")
    lines = [
        f"The nightly scan of the **latest published** `{image}` image reports "
        f"**{len(findings)}** HIGH or CRITICAL finding(s) ({critical} CRITICAL, {high} HIGH).",
        "",
        f"- Image: `{image}:latest`",
        f"- Digest scanned: `{digest}` (linux/amd64)",
        f"- Version: `{version or 'unknown'}`",
        f"- Run: {run_url}",
        "",
    ]
    if exempt:
        lines += [
            "> This image is **exempt** in `.github/image-scan-exempt.txt`: its findings do "
            "not block a release. The entry there names the cause and what ends it.",
            "",
        ]
    lines += [
        "| CVE | Package | Installed → Fixed | Severity | Target |",
        "|---|---|---|---|---|",
    ]
    for f in findings[:MAX_ROWS]:
        fixed = f["fixed"] or "no fix yet"
        lines.append(
            f"| {_cell(f['id'])} | {_cell(f['package'])} | {_cell(f['installed'])} → {_cell(fixed)} "
            f"| {f['severity']} | {_cell(f['target'])} |"
        )
    if len(findings) > MAX_ROWS:
        lines.append(f"| … | {len(findings) - MAX_ROWS} more row(s) in the run's report | | | |")
    lines += [
        "",
        "Raise the pinned versions (base image digest, toolchain, dependency) and "
        "release; the next nightly scan of `latest` closes this issue when it is clean. "
        "The threshold is not lowered and no CVE is ignored.",
        "",
        "_Opened and updated by `.github/workflows/nightly-image-scan.yml` "
        "(`scripts/ci/image_scan_issue.py`). One issue per image; edits to this body are overwritten._",
        "",
        f"{FINGERPRINT_PREFIX}{fingerprint(findings)} -->",
    ]
    return "\n".join(lines)


def body_fingerprint(body: str) -> Optional[str]:
    for line in (body or "").splitlines():
        if line.startswith(FINGERPRINT_PREFIX):
            return line[len(FINGERPRINT_PREFIX):].removesuffix(" -->").strip()
    return None


Gh = Callable[[Sequence[str], Optional[str]], str]


def real_gh(args: Sequence[str], stdin: Optional[str] = None) -> str:
    completed = subprocess.run(
        ["gh", *args], input=stdin, capture_output=True, text=True, check=False
    )
    if completed.returncode != 0:
        raise RuntimeError(f"gh {' '.join(args)} failed ({completed.returncode}): {completed.stderr.strip()}")
    return completed.stdout


def find_issue(gh: Gh, repo: str, title: str, state: str) -> Optional[dict]:
    """The issue with EXACTLY this title, or None. Search is fuzzy; the filter is not."""
    out = gh(
        [
            "issue", "list", "--repo", repo, "--state", state, "--label", "image-scan",
            "--search", f'"{title}" in:title', "--limit", "50",
            "--json", "number,title,body,state",
        ],
        None,
    )
    matches = [i for i in json.loads(out or "[]") if i.get("title") == title]
    if not matches:
        return None
    return min(matches, key=lambda i: i["number"])


def sync(gh: Gh, *, repo: str, image: str, findings: Sequence[dict], digest: str,
         version: str, run_url: str, exempt: bool) -> str:
    """Apply the table in the module docstring. Returns what it did."""
    title = title_for(image)
    current = find_issue(gh, repo, title, "open")

    if not findings:
        if current is None:
            return "clean; no open issue"
        gh(
            [
                "issue", "close", str(current["number"]), "--repo", repo, "--reason", "completed",
                "--comment",
                f"The nightly scan of `{image}:latest` (`{digest}`, version `{version or 'unknown'}`) "
                f"reports no HIGH or CRITICAL finding. Closing. Run: {run_url}",
            ],
            None,
        )
        return f"clean; closed #{current['number']}"

    body = render_body(image, findings, digest=digest, version=version, run_url=run_url, exempt=exempt)
    new_print = fingerprint(findings)

    if current is not None:
        old_print = body_fingerprint(current.get("body", ""))
        gh(["issue", "edit", str(current["number"]), "--repo", repo, "--body-file", "-"], body)
        if old_print == new_print:
            return f"unchanged findings; refreshed #{current['number']}"
        gh(
            [
                "issue", "comment", str(current["number"]), "--repo", repo, "--body",
                f"The finding set changed: now {len(findings)} HIGH or CRITICAL finding(s) in "
                f"`{image}:latest` (`{digest}`). The table above is current. Run: {run_url}",
            ],
            None,
        )
        return f"findings changed; updated #{current['number']}"

    closed = find_issue(gh, repo, title, "closed")
    if closed is not None:
        gh(["issue", "reopen", str(closed["number"]), "--repo", repo], None)
        gh(["issue", "edit", str(closed["number"]), "--repo", repo, "--body-file", "-"], body)
        gh(
            [
                "issue", "comment", str(closed["number"]), "--repo", repo, "--body",
                f"Reopened: the nightly scan of `{image}:latest` (`{digest}`) reports "
                f"{len(findings)} HIGH or CRITICAL finding(s) again. Run: {run_url}",
            ],
            None,
        )
        return f"regressed; reopened #{closed['number']}"

    out = gh(
        [
            "issue", "create", "--repo", repo, "--title", title, "--body-file", "-",
            *[arg for label in LABELS for arg in ("--label", label)],
        ],
        body,
    )
    return f"opened {out.strip()}"


def main(argv: Optional[Sequence[str]] = None, gh: Gh = real_gh) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n", 1)[0])
    parser.add_argument("--report", type=pathlib.Path, required=True)
    parser.add_argument("--image", required=True)
    parser.add_argument("--expect-type", required=True)
    parser.add_argument("--blocking", choices=("true", "false"), required=True)
    parser.add_argument("--digest", required=True)
    parser.add_argument("--version", default="")
    parser.add_argument("--run-url", required=True)
    parser.add_argument("--repo", required=True)
    args = parser.parse_args(argv)

    try:
        findings = load_findings(args.report, args.expect_type)
    except ReportError as exc:
        print(f"::error::{args.image}: {exc}; no issue was opened, updated or closed")
        return 2

    outcome = sync(
        gh,
        repo=args.repo,
        image=args.image,
        findings=findings,
        digest=args.digest,
        version=args.version,
        run_url=args.run_url,
        exempt=args.blocking == "false",
    )
    print(f"{args.image}: {len(findings)} finding(s); {outcome}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
