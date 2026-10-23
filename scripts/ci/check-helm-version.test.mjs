// Scenarios for check-helm-version.mjs: a throwaway .github tree with enough
// setup-helm steps to clear the floor, then one step changed per case.

import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";

import * as checker from "./check-helm-version.mjs";

const step = (version, id = "") =>
  `      - ${id ? `id: ${id}\n        ` : ""}uses: azure/setup-helm@v5\n        with:\n          version: ${version}\n`;
const workflow = (steps) => `on: push\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n${steps}`;

function tree({ subject = step(checker.HELM_VERSION), helm3 = step(checker.HELM3_VERSION, "helm3"), filler = checker.MIN_STEPS } = {}) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "helm-pins-"));
  const wf = path.join(root, ".github", "workflows");
  fs.mkdirSync(wf, { recursive: true });
  for (let i = 0; i < filler; i += 1) fs.writeFileSync(path.join(wf, `f${i}.yml`), workflow(step(checker.HELM_VERSION)));
  fs.writeFileSync(path.join(wf, "subject.yml"), workflow(subject));
  fs.writeFileSync(path.join(wf, "helm-lint.yml"), workflow(step(checker.HELM_VERSION) + helm3));
  return root;
}

test("the pinned tree passes", () => {
  assert.deepEqual(checker.check(tree()).problems, []);
});

test("a step on another Helm 4 fails", () => {
  const { problems } = checker.check(tree({ subject: step("v4.2.0") }));
  assert.match(problems.join("\n"), /subject\.yml: installs Helm 'v4\.2\.0'/);
});

test("a step with no version (the runner's helm) fails", () => {
  const { problems } = checker.check(tree({ subject: "      - uses: azure/setup-helm@v5\n" }));
  assert.match(problems.join("\n"), /no version/);
});

test("a Helm 3 pin outside the helm3 step fails", () => {
  const { problems } = checker.check(tree({ subject: step(checker.HELM3_VERSION) }));
  assert.match(problems.join("\n"), /subject\.yml: installs Helm 'v3/);
});

test("the Helm 3 render step must exist, once, on the Helm 3 pin", () => {
  assert.match(checker.check(tree({ helm3: "" })).problems.join("\n"), /found 0/);
  assert.match(checker.check(tree({ helm3: step("v3.16.0", "helm3") })).problems.join("\n"), /the Helm 3 pin/);
});

test("too few steps is a failure, not a pass", () => {
  assert.match(checker.check(tree({ filler: 0 })).problems.join("\n"), /expected at least/);
});

test("a composite action is scanned too", () => {
  const root = tree();
  fs.mkdirSync(path.join(root, ".github", "actions", "a"), { recursive: true });
  fs.writeFileSync(path.join(root, ".github", "actions", "a", "action.yml"),
    `runs:\n  using: composite\n  steps:\n${step("v3.16.0")}`);
  assert.match(checker.check(root).problems.join("\n"), /actions\/a\/action\.yml/);
});

test("behind compares numerically", () => {
  assert.equal(checker.behind("v4.3.1", "v4.10.0"), true);
  assert.equal(checker.behind("v4.3.1", "v4.3.1"), false);
  assert.equal(checker.behind("v4.3.2", "v4.3.1"), false);
});
