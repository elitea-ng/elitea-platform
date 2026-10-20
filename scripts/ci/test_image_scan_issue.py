"""Scenarios for image_scan_issue.py, driven by a fake `gh`.

The two fixtures under testdata/ are REAL trivy 0.74 reports, trimmed to the
fields the script reads: alpine:3.10 (one CRITICAL in apk-tools) and
ghcr.io/elitea-ng/elitea-scheduler:latest on 2026-10-20 (clean: a `debian`
and a `gobinary` target, no finding).
"""

from __future__ import annotations

import io
import json
import pathlib
import sys
import tempfile
import unittest
from contextlib import redirect_stdout

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import image_scan_issue as tool  # noqa: E402

TESTDATA = pathlib.Path(__file__).resolve().parent / "testdata"
VULNERABLE = TESTDATA / "trivy-alpine310.json"
CLEAN = TESTDATA / "trivy-scheduler.json"
REPO = "elitea-ng/elitea-platform"
IMAGE = "elitea-ui"
TITLE = tool.title_for(IMAGE)


class FakeGh:
    """An in-memory issue tracker that answers the gh calls the script makes."""

    def __init__(self, issues=None):
        self.issues = {i["number"]: dict(i) for i in (issues or [])}
        self.calls: list[list[str]] = []
        self.comments: dict[int, list[str]] = {}

    def _arg(self, args, flag):
        return args[args.index(flag) + 1]

    def __call__(self, args, stdin=None):
        args = list(args)
        self.calls.append(args)
        verb = args[1]
        if verb == "list":
            state = self._arg(args, "--state")
            return json.dumps([i for i in self.issues.values() if i["state"].lower() == state])
        if verb == "create":
            number = max(self.issues, default=0) + 1
            self.issues[number] = {
                "number": number, "title": self._arg(args, "--title"), "body": stdin, "state": "OPEN",
                "labels": [args[i + 1] for i, a in enumerate(args) if a == "--label"],
            }
            return f"https://github.com/{REPO}/issues/{number}\n"
        number = int(args[2])
        if verb == "edit":
            self.issues[number]["body"] = stdin
        elif verb == "comment":
            self.comments.setdefault(number, []).append(self._arg(args, "--body"))
        elif verb == "close":
            self.issues[number]["state"] = "CLOSED"
            self.comments.setdefault(number, []).append(self._arg(args, "--comment"))
        elif verb == "reopen":
            self.issues[number]["state"] = "OPEN"
        else:
            raise AssertionError(f"unexpected gh call {args}")
        return ""

    def verbs(self):
        return [c[1] for c in self.calls if c[1] != "list"]


def run(gh, report, *, expect_type="alpine", blocking="true", digest="sha256:aaa"):
    out = io.StringIO()
    with redirect_stdout(out):
        code = tool.main(
            [
                "--report", str(report), "--image", IMAGE, "--expect-type", expect_type,
                "--blocking", blocking, "--digest", f"ghcr.io/elitea-ng/{IMAGE}@{digest}",
                "--version", "4.58.0", "--run-url", "https://example.invalid/run/1", "--repo", REPO,
            ],
            gh=gh,
        )
    return code, out.getvalue()


def existing(state="OPEN", body="old body", number=7, title=TITLE):
    return {"number": number, "title": title, "body": body, "state": state}


class ImageScanIssue(unittest.TestCase):
    # ── reading the report ─────────────────────────────────────────────────
    def test_real_vulnerable_report_parses_to_one_critical(self):
        findings = tool.load_findings(VULNERABLE, "alpine")
        self.assertEqual(
            [(f["id"], f["package"], f["installed"], f["fixed"], f["severity"]) for f in findings],
            [("CVE-2021-36159", "apk-tools", "2.10.6-r0", "2.10.7-r0", "CRITICAL")],
        )

    def test_real_clean_report_parses_to_no_finding(self):
        self.assertEqual(tool.load_findings(CLEAN, "gobinary"), [])

    def test_missing_report_is_refused_and_touches_no_issue(self):
        gh = FakeGh([existing()])
        code, out = run(gh, TESTDATA / "does-not-exist.json")
        self.assertEqual(code, 2, out)
        self.assertEqual(gh.calls, [])

    def test_unparsable_report_is_refused(self):
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
            f.write("{not json")
        gh = FakeGh()
        code, _ = run(gh, pathlib.Path(f.name))
        self.assertEqual(code, 2)
        self.assertEqual(gh.calls, [])

    def test_report_without_expected_target_does_not_close_an_open_issue(self):
        # A clean report of the WRONG type is a scan that never reached the
        # image: it must not read as "fixed" and close the issue.
        gh = FakeGh([existing()])
        code, out = run(gh, CLEAN, expect_type="rustbinary")
        self.assertEqual(code, 2, out)
        self.assertEqual(gh.issues[7]["state"], "OPEN")
        self.assertEqual(gh.calls, [])

    # ── the issue table ────────────────────────────────────────────────────
    def test_findings_and_no_issue_opens_one_with_labels_and_table(self):
        gh = FakeGh()
        code, out = run(gh, VULNERABLE)
        self.assertEqual(code, 0, out)
        self.assertEqual(gh.verbs(), ["create"])
        issue = gh.issues[1]
        self.assertEqual(issue["title"], "Image scan: elitea-ui — vulnerabilities in latest")
        self.assertEqual(issue["labels"], ["security", "image-scan"])
        body = issue["body"]
        self.assertIn("| CVE-2021-36159 | apk-tools | 2.10.6-r0 → 2.10.7-r0 | CRITICAL |", body)
        self.assertIn("ghcr.io/elitea-ng/elitea-ui@sha256:aaa", body)
        self.assertIn("4.58.0", body)
        self.assertIn("https://example.invalid/run/1", body)

    def test_findings_and_open_issue_updates_without_a_duplicate(self):
        gh = FakeGh([existing()])
        code, _ = run(gh, VULNERABLE)
        self.assertEqual(code, 0)
        self.assertEqual(gh.verbs(), ["edit", "comment"])
        self.assertEqual(len(gh.issues), 1)
        self.assertIn("CVE-2021-36159", gh.issues[7]["body"])

    def test_same_findings_next_night_refreshes_body_without_comment(self):
        gh = FakeGh()
        run(gh, VULNERABLE)
        gh.calls.clear()
        code, out = run(gh, VULNERABLE, digest="sha256:bbb")
        self.assertEqual(code, 0)
        self.assertEqual(gh.verbs(), ["edit"], out)
        self.assertIn("sha256:bbb", gh.issues[1]["body"])
        self.assertNotIn(1, gh.comments)

    def test_clean_and_open_issue_closes_with_comment(self):
        gh = FakeGh([existing()])
        code, out = run(gh, CLEAN, expect_type="gobinary")
        self.assertEqual(code, 0, out)
        self.assertEqual(gh.verbs(), ["close"])
        self.assertEqual(gh.issues[7]["state"], "CLOSED")
        self.assertIn("no HIGH or CRITICAL finding", gh.comments[7][0])

    def test_clean_and_no_issue_does_nothing(self):
        gh = FakeGh()
        code, _ = run(gh, CLEAN, expect_type="gobinary")
        self.assertEqual(code, 0)
        self.assertEqual(gh.verbs(), [])

    def test_findings_and_only_closed_issue_reopens_it(self):
        gh = FakeGh([existing(state="CLOSED")])
        code, _ = run(gh, VULNERABLE)
        self.assertEqual(code, 0)
        self.assertEqual(gh.verbs(), ["reopen", "edit", "comment"])
        self.assertEqual(gh.issues[7]["state"], "OPEN")
        self.assertEqual(len(gh.issues), 1)

    def test_a_similar_title_is_not_taken_for_this_image(self):
        # gh's search is fuzzy; `elitea-ui-admin` must not update `elitea-ui`'s issue.
        gh = FakeGh([existing(title=tool.title_for("elitea-ui-admin"))])
        run(gh, VULNERABLE)
        self.assertEqual(gh.verbs(), ["create"])

    def test_exempt_image_says_so(self):
        gh = FakeGh()
        run(gh, VULNERABLE, blocking="false")
        self.assertIn("exempt", gh.issues[1]["body"])

    def test_table_cells_escape_pipes(self):
        findings = [{"severity": "HIGH", "id": "CVE-1", "package": "a|b", "installed": "1", "fixed": "", "target": "t"}]
        body = tool.render_body("x", findings, digest="d", version="v", run_url="u", exempt=False)
        self.assertIn("a\\|b", body)
        self.assertIn("1 → no fix yet", body)

    def test_fingerprint_ignores_order_and_digest(self):
        a = {"severity": "HIGH", "id": "CVE-1", "package": "p", "installed": "1", "fixed": "2", "target": "t"}
        b = {"severity": "CRITICAL", "id": "CVE-2", "package": "q", "installed": "1", "fixed": "", "target": "t"}
        self.assertEqual(tool.fingerprint([a, b]), tool.fingerprint([a, b]))
        body = tool.render_body("x", [a, b], digest="d1", version="v", run_url="u", exempt=False)
        self.assertEqual(tool.body_fingerprint(body), tool.fingerprint([a, b]))


if __name__ == "__main__":
    unittest.main()
