// Scenarios for check-workflow-branch-coverage.mjs (new with the port: the
// Python version had none). The text reader is the whole check, so its
// shapes are held here.

import assert from "node:assert/strict";
import { test } from "node:test";

import { pushBranches, pushPaths } from "./check-workflow-branch-coverage.mjs";

const WORKFLOW = [
  "name: X",
  "on:",
  "  pull_request:",
  "    paths:",
  '      - "a/**"',
  "  push:",
  "    branches:",
  "      - main",
  '      - "next"',
  "    paths:",
  '      - "a/**"',
  '      - ".github/workflows/x.yml"',
  "",
  "jobs:",
  "  x: {}",
  "",
].join("\n");

test("the push arm's branches and paths are read, the pull_request arm's are not", () => {
  assert.deepEqual(pushBranches(WORKFLOW), ["main", "next"]);
  assert.deepEqual(pushPaths(WORKFLOW), ["a/**", ".github/workflows/x.yml"]);
});

test("no push arm is null; a push arm without branches is an empty list, never a pass", () => {
  assert.equal(pushBranches("on:\n  pull_request:\n\njobs: {}\n"), null);
  assert.deepEqual(pushBranches("on:\n  push:\n    tags: [v*]\n\njobs: {}\n"), []);
  assert.equal(pushPaths("on:\n  push:\n    branches:\n      - main\n"), null);
});

test("an item with a trailing comment is not read, as the Python pattern did not read it", () => {
  assert.deepEqual(pushBranches('on:\n  push:\n    branches:\n      - main\n      - "x"  # why\n'), ["main"]);
});
