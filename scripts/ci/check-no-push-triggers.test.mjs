// Scenarios for check-no-push-triggers.mjs. The text reader is the whole
// check, so its shapes are held here, and the repository itself is run last.

import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";

import { main, MINIMUM_WORKFLOW_COUNT, triggerEvents } from "./check-no-push-triggers.mjs";

test("the block form reads the first-level keys only", () => {
  const text = [
    "name: X",
    "on:",
    "  pull_request:",
    "    paths:",
    '      - "push/**"',
    "  # push: a comment is not a trigger",
    "  workflow_dispatch:",
    "    inputs:",
    "      push:",
    "        type: boolean",
    "  schedule:",
    '    - cron: "7 2 * * *"',
    "",
    "jobs:",
    "  x: {}",
    "",
  ].join("\n");
  assert.deepEqual(triggerEvents(text), ["pull_request", "workflow_dispatch", "schedule"]);
});

test("a push key in the block form is read", () => {
  assert.deepEqual(triggerEvents("on:\n  push:\n    branches: [main]\n  pull_request:\n\njobs: {}\n"), ["push", "pull_request"]);
  assert.deepEqual(triggerEvents('"on":\n    push:\n\njobs: {}\n'), ["push"]);
});

test("the inline forms are read", () => {
  assert.deepEqual(triggerEvents("on: push\njobs: {}\n"), ["push"]);
  assert.deepEqual(triggerEvents("on: [pull_request, 'push']  # why\njobs: {}\n"), ["pull_request", "push"]);
});

test("no on: is null, an empty block is an empty list — never a pass", () => {
  assert.equal(triggerEvents("name: x\njobs: {}\n"), null);
  assert.deepEqual(triggerEvents("on:\njobs: {}\n"), []);
});

function tree(subject) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "no-push-"));
  const wf = path.join(root, ".github", "workflows");
  fs.mkdirSync(wf, { recursive: true });
  for (let i = 0; i < MINIMUM_WORKFLOW_COUNT; i += 1) fs.writeFileSync(path.join(wf, `f${i}.yml`), "on:\n  pull_request:\n\njobs: {}\n");
  if (subject !== null) fs.writeFileSync(path.join(wf, "subject.yml"), subject);
  return root;
}

function run(root) {
  const lines = [];
  const code = main({ root, out: (l) => lines.push(l), err: (l) => lines.push(l) });
  return [code, lines.join("\n")];
}

test("a push trigger fails, and names the workflow", () => {
  const [code, text] = run(tree("on:\n  push:\n    branches: [main]\n\njobs: {}\n"));
  assert.equal(code, 1);
  assert.match(text, /::error::subject\.yml starts on a push/);
});

test("an unreadable on: fails rather than passes", () => {
  const [code, text] = run(tree("name: x\njobs: {}\n"));
  assert.equal(code, 1);
  assert.match(text, /subject\.yml: read no trigger/);
});

test("too few workflows fails rather than passes", () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "no-push-"));
  const [code, text] = run(root);
  assert.equal(code, 1);
  assert.match(text, /expected at least/);
});

test("this repository holds the rule", () => {
  const [code, text] = run(path.resolve(path.dirname(new URL(import.meta.url).pathname), "..", ".."));
  assert.equal(code, 0, text);
});
