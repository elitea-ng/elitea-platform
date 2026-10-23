// Scenarios for image-scan-issue.mjs, driven by a fake github-script client.
//
// The two fixtures under testdata/ are REAL trivy 0.74 reports, trimmed to the
// fields the script reads: alpine:3.10 (one CRITICAL in apk-tools) and
// ghcr.io/elitea-ng/elitea-scheduler:latest on 2026-10-20 (clean: a `debian`
// and a `gobinary` target, no finding).

import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

import * as tool from "./image-scan-issue.mjs";

const TESTDATA = path.join(path.dirname(fileURLToPath(import.meta.url)), "testdata");
const VULNERABLE = path.join(TESTDATA, "trivy-alpine310.json");
const CLEAN = path.join(TESTDATA, "trivy-scheduler.json");
const REPO = "elitea-ng/elitea-platform";
const IMAGE = "elitea-web";
const TITLE = tool.titleFor(IMAGE);

/**
 * An in-memory issue tracker that answers the REST calls the script makes.
 * Each write is recorded as the verb the Python suite named: `create`, `edit`
 * (a body update), `comment`, `close` and `reopen` (a state update). gh's
 * `issue close --comment` was one call; over REST it is a comment, then the
 * close, so the close arm records ["comment", "close"].
 */
class FakeGithub {
  constructor(issues = []) {
    this.issues = new Map(issues.map((i) => [i.number, { labels: ["image-scan"], ...i }]));
    this.calls = [];
    this.comments = new Map();
    const self = this;
    const scope = (params) => {
      assert.equal(`${params.owner}/${params.repo}`, REPO);
    };
    this.rest = {
      issues: {
        listForRepo: { endpoint: "listForRepo" },
        async create(params) {
          scope(params);
          self.calls.push(["create", params]);
          const number = Math.max(0, ...self.issues.keys()) + 1;
          self.issues.set(number, { number, title: params.title, body: params.body, state: "OPEN", labels: params.labels });
          return { data: { number, html_url: `https://github.com/${REPO}/issues/${number}` } };
        },
        async update(params) {
          scope(params);
          const issue = self.issues.get(params.issue_number);
          if (params.state === "closed") {
            assert.equal(params.state_reason, "completed");
            self.calls.push(["close", params]);
            issue.state = "CLOSED";
          } else if (params.state === "open") {
            self.calls.push(["reopen", params]);
            issue.state = "OPEN";
          } else if (params.body !== undefined) {
            self.calls.push(["edit", params]);
            issue.body = params.body;
          } else {
            throw new Error(`unexpected update ${JSON.stringify(params)}`);
          }
          return { data: {} };
        },
        async createComment(params) {
          scope(params);
          self.calls.push(["comment", params]);
          if (!self.comments.has(params.issue_number)) self.comments.set(params.issue_number, []);
          self.comments.get(params.issue_number).push(params.body);
          return { data: {} };
        },
      },
    };
  }

  async paginate(method, params) {
    assert.equal(method, this.rest.issues.listForRepo);
    this.calls.push(["list", params]);
    return [...this.issues.values()].filter(
      (i) => i.state.toLowerCase() === params.state && (i.labels || []).includes(params.labels),
    );
  }

  verbs() {
    return this.calls.filter(([verb]) => verb !== "list").map(([verb]) => verb);
  }
}

async function run(github, report, { expectType = "alpine", blocking = "true", digest = "sha256:aaa" } = {}) {
  const lines = [];
  const code = await tool.main({
    github,
    log: (line) => lines.push(line),
    args: {
      report,
      image: IMAGE,
      expectType,
      blocking,
      digest: `ghcr.io/elitea-ng/${IMAGE}@${digest}`,
      version: "4.58.0",
      runUrl: "https://example.invalid/run/1",
      repo: REPO,
    },
  });
  return [code, lines.join("\n")];
}

const existing = ({ state = "OPEN", body = "old body", number = 7, title = TITLE } = {}) => ({ number, title, body, state });

// ── reading the report ─────────────────────────────────────────────────────

test("real vulnerable report parses to one critical", () => {
  const findings = tool.loadFindings(VULNERABLE, "alpine");
  assert.deepEqual(
    findings.map((f) => [f.id, f.package, f.installed, f.fixed, f.severity]),
    [["CVE-2021-36159", "apk-tools", "2.10.6-r0", "2.10.7-r0", "CRITICAL"]],
  );
});

test("real clean report parses to no finding", () => {
  assert.deepEqual(tool.loadFindings(CLEAN, "gobinary"), []);
});

test("missing report is refused and touches no issue", async () => {
  const gh = new FakeGithub([existing()]);
  const [code, out] = await run(gh, path.join(TESTDATA, "does-not-exist.json"));
  assert.equal(code, 2, out);
  assert.deepEqual(gh.calls, []);
});

test("unparsable report is refused", async () => {
  const file = path.join(fs.mkdtempSync(path.join(os.tmpdir(), "image-scan-")), "report.json");
  fs.writeFileSync(file, "{not json");
  const gh = new FakeGithub();
  const [code] = await run(gh, file);
  assert.equal(code, 2);
  assert.deepEqual(gh.calls, []);
});

test("report without expected target does not close an open issue", async () => {
  // A clean report of the WRONG type is a scan that never reached the
  // image: it must not read as "fixed" and close the issue.
  const gh = new FakeGithub([existing()]);
  const [code, out] = await run(gh, CLEAN, { expectType: "rustbinary" });
  assert.equal(code, 2, out);
  assert.equal(gh.issues.get(7).state, "OPEN");
  assert.deepEqual(gh.calls, []);
});

// ── the issue table ────────────────────────────────────────────────────────

test("findings and no issue opens one with labels and table", async () => {
  const gh = new FakeGithub();
  const [code, out] = await run(gh, VULNERABLE);
  assert.equal(code, 0, out);
  assert.deepEqual(gh.verbs(), ["create"]);
  const issue = gh.issues.get(1);
  assert.equal(issue.title, "Image scan: elitea-web — vulnerabilities in latest");
  assert.deepEqual(issue.labels, ["security", "image-scan"]);
  const body = issue.body;
  assert.ok(body.includes("| CVE-2021-36159 | apk-tools | 2.10.6-r0 → 2.10.7-r0 | CRITICAL |"));
  assert.ok(body.includes("ghcr.io/elitea-ng/elitea-web@sha256:aaa"));
  assert.ok(body.includes("4.58.0"));
  assert.ok(body.includes("https://example.invalid/run/1"));
  assert.ok(out.endsWith(`elitea-web: 1 finding(s); opened https://github.com/${REPO}/issues/1`), out);
});

test("findings and open issue updates without a duplicate", async () => {
  const gh = new FakeGithub([existing()]);
  const [code] = await run(gh, VULNERABLE);
  assert.equal(code, 0);
  assert.deepEqual(gh.verbs(), ["edit", "comment"]);
  assert.equal(gh.issues.size, 1);
  assert.ok(gh.issues.get(7).body.includes("CVE-2021-36159"));
});

test("same findings next night refreshes body without comment", async () => {
  const gh = new FakeGithub();
  await run(gh, VULNERABLE);
  gh.calls.length = 0;
  const [code, out] = await run(gh, VULNERABLE, { digest: "sha256:bbb" });
  assert.equal(code, 0);
  assert.deepEqual(gh.verbs(), ["edit"], out);
  assert.ok(gh.issues.get(1).body.includes("sha256:bbb"));
  assert.equal(gh.comments.has(1), false);
});

test("clean and open issue closes with comment", async () => {
  const gh = new FakeGithub([existing()]);
  const [code, out] = await run(gh, CLEAN, { expectType: "gobinary" });
  assert.equal(code, 0, out);
  assert.deepEqual(gh.verbs(), ["comment", "close"]);
  assert.equal(gh.issues.get(7).state, "CLOSED");
  assert.ok(gh.comments.get(7)[0].includes("no HIGH or CRITICAL finding"));
});

test("clean and no issue does nothing", async () => {
  const gh = new FakeGithub();
  const [code] = await run(gh, CLEAN, { expectType: "gobinary" });
  assert.equal(code, 0);
  assert.deepEqual(gh.verbs(), []);
});

test("findings and only closed issue reopens it", async () => {
  const gh = new FakeGithub([existing({ state: "CLOSED" })]);
  const [code] = await run(gh, VULNERABLE);
  assert.equal(code, 0);
  assert.deepEqual(gh.verbs(), ["reopen", "edit", "comment"]);
  assert.equal(gh.issues.get(7).state, "OPEN");
  assert.equal(gh.issues.size, 1);
});

test("a similar title is not taken for this image", async () => {
  // `elitea-web-admin` must not update `elitea-web`'s issue.
  const gh = new FakeGithub([existing({ title: tool.titleFor("elitea-web-admin") })]);
  await run(gh, VULNERABLE);
  assert.deepEqual(gh.verbs(), ["create"]);
});

test("exempt image says so", async () => {
  const gh = new FakeGithub();
  await run(gh, VULNERABLE, { blocking: "false" });
  assert.ok(gh.issues.get(1).body.includes("exempt"));
});

test("table cells escape pipes", () => {
  const findings = [{ severity: "HIGH", id: "CVE-1", package: "a|b", installed: "1", fixed: "", target: "t" }];
  const body = tool.renderBody("x", findings, { digest: "d", version: "v", runUrl: "u", exempt: false });
  assert.ok(body.includes("a\\|b"));
  assert.ok(body.includes("1 → no fix yet"));
});

test("fingerprint ignores order and digest", () => {
  const a = { severity: "HIGH", id: "CVE-1", package: "p", installed: "1", fixed: "2", target: "t" };
  const b = { severity: "CRITICAL", id: "CVE-2", package: "q", installed: "1", fixed: "", target: "t" };
  assert.equal(tool.fingerprint([a, b]), tool.fingerprint([a, b]));
  const body = tool.renderBody("x", [a, b], { digest: "d1", version: "v", runUrl: "u", exempt: false });
  assert.equal(tool.bodyFingerprint(body), tool.fingerprint([a, b]));
});

// ── new with the port ──────────────────────────────────────────────────────

test("the fingerprint is the one the Python version wrote into open issues", () => {
  // sha256("CRITICAL|CVE-2021-36159|apk-tools|2.10.6-r0|2.10.7-r0")[:16], as
  // image_scan_issue.py computed it: an issue opened before the port must not
  // read as "the finding set changed" on the first night after it.
  assert.equal(tool.fingerprint(tool.loadFindings(VULNERABLE, "alpine")), "688eb43cefa2fb79");
});

test("a pull request with the title is not taken for the issue", async () => {
  const gh = new FakeGithub([{ ...existing(), pull_request: { url: "x" } }]);
  await run(gh, VULNERABLE);
  assert.deepEqual(gh.verbs(), ["create"]);
});

test("a bad blocking value is refused and touches no issue", async () => {
  const gh = new FakeGithub([existing()]);
  const [code, out] = await run(gh, VULNERABLE, { blocking: "maybe" });
  assert.equal(code, 2, out);
  assert.match(out, /--blocking: invalid choice: 'maybe'/);
  assert.deepEqual(gh.calls, []);
});
