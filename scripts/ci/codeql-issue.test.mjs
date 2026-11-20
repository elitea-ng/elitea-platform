// Scenarios for codeql-issue.mjs: every arm of the issue sync, with a fake client,
// and the refusal to read "no findings" off an analysis that did not run.

import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";

import { SarifError, loadResults, main, renderBody, sync, titleFor } from "./codeql-issue.mjs";

const sarif = (results) => ({ runs: [{ results }] });
const result = (rule, uri, line, text = "a finding") => ({ ruleId: rule, level: "error", message: { text }, locations: [{ physicalLocation: { artifactLocation: { uri }, region: { startLine: line } } }] });
const dirWith = (files) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "codeql-"));
  for (const [name, content] of Object.entries(files)) fs.writeFileSync(path.join(dir, name), typeof content === "string" ? content : JSON.stringify(content));
  return dir;
};

function fakeGithub(existing = []) {
  const calls = [];
  const issues = existing;
  return {
    calls,
    paginate: async (fn, params) => issues.filter((i) => i.state === params.state),
    rest: {
      issues: {
        listForRepo: () => {},
        create: async (p) => calls.push(["create", p.title]),
        update: async (p) => calls.push(["update", p.issue_number, p.state ?? "body"]),
        createComment: async (p) => calls.push(["comment", p.issue_number]),
      },
    },
  };
}

test("results are read from every SARIF file, sorted by location", () => {
  const dir = dirWith({ "a.sarif": sarif([result("go/sql", "b.go", 9), result("go/xss", "a.go", 3)]), "b.sarif": sarif([result("go/x", "c.go", 1)]) });
  const results = loadResults(dir);
  assert.deepEqual(results.map((r) => r.where), ["a.go:3", "b.go:9", "c.go:1"]);
});

test("no SARIF, unreadable SARIF and a SARIF without runs are refused", () => {
  assert.throws(() => loadResults(dirWith({})), SarifError);
  assert.throws(() => loadResults(dirWith({ "a.sarif": "{not json" })), /not JSON/);
  assert.throws(() => loadResults(dirWith({ "a.sarif": { runs: [] } })), /no runs/);
});

test("findings and no issue open one; the same findings again change nothing", async () => {
  const results = loadResults(dirWith({ "a.sarif": sarif([result("go/x", "a.go", 1)]) }));
  const gh = fakeGithub();
  assert.equal(await sync(gh, { repo: "o/r", language: "go", results, runUrl: "u" }), "opened");
  assert.deepEqual(gh.calls, [["create", titleFor("go")]]);
  const body = renderBody("go", results, { runUrl: "u" });
  const again = fakeGithub([{ number: 7, title: titleFor("go"), state: "open", body }]);
  assert.equal(await sync(again, { repo: "o/r", language: "go", results, runUrl: "u" }), "unchanged");
  assert.deepEqual(again.calls, [["update", 7, "body"]]);
});

test("a changed finding set updates the open issue and comments", async () => {
  const before = loadResults(dirWith({ "a.sarif": sarif([result("go/x", "a.go", 1)]) }));
  const after = loadResults(dirWith({ "a.sarif": sarif([result("go/x", "a.go", 1), result("go/y", "b.go", 2)]) }));
  const gh = fakeGithub([{ number: 3, title: titleFor("go"), state: "open", body: renderBody("go", before, { runUrl: "u" }) }]);
  assert.equal(await sync(gh, { repo: "o/r", language: "go", results: after, runUrl: "u" }), "updated");
  assert.deepEqual(gh.calls, [["update", 3, "body"], ["comment", 3]]);
});

test("a closed issue is reopened when the findings return; a clean run closes an open one", async () => {
  const results = loadResults(dirWith({ "a.sarif": sarif([result("go/x", "a.go", 1)]) }));
  const closed = fakeGithub([{ number: 5, title: titleFor("go"), state: "closed", body: "" }]);
  assert.equal(await sync(closed, { repo: "o/r", language: "go", results, runUrl: "u" }), "reopened");
  const open = fakeGithub([{ number: 6, title: titleFor("go"), state: "open", body: "" }]);
  assert.equal(await sync(open, { repo: "o/r", language: "go", results: [], runUrl: "u" }), "closed");
  assert.deepEqual(open.calls, [["comment", 6], ["update", 6, "closed"]]);
  assert.equal(await sync(fakeGithub(), { repo: "o/r", language: "go", results: [], runUrl: "u" }), "nothing");
});

test("main refuses a missing analysis with exit 2 and touches no issue", async () => {
  const gh = fakeGithub();
  const quiet = () => {};
  assert.equal(await main({ github: gh, args: { sarifDir: dirWith({}), repo: "o/r", language: "go", runUrl: "u" }, log: quiet }), 2);
  assert.deepEqual(gh.calls, []);
  assert.equal(await main({ github: gh, args: { sarifDir: dirWith({ "a.sarif": sarif([]) }), repo: "o/r", language: "go", runUrl: "u" }, log: quiet }), 0);
});
