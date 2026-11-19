import assert from "node:assert/strict";
import { test } from "node:test";

import { BUDGET_GIB, check, main, rustCacheSteps } from "./check-cache-budget.mjs";

const entries = (n, gib) => Array.from({ length: n }, (_, i) => ({ family: `f${i}`, gib }));
const wf = (schedule, with_) => ({ on: schedule ? { workflow_call: null, schedule: [{ cron: "1 0 * * *" }] } : { workflow_call: null }, jobs: { j: { steps: [{ uses: "Swatinem/rust-cache@v2", with: { "save-if": "${{ github.ref == 'refs/heads/main' }}", ...with_ } }] } } });
const many = (extra = {}) => ({ ...Object.fromEntries(Array.from({ length: 5 }, (_, i) => [`w${i}.yml`, wf(true, { "shared-key": `k${i}` })])), ...extra });

test("a plan under budget with scheduled writers passes", () => {
  assert.deepEqual(check({ budget: { entries: entries(6, 1) }, workflows: many() }), []);
});

test("a plan over budget fails", () => {
  const errors = check({ budget: { entries: entries(6, BUDGET_GIB / 3) }, workflows: many() });
  assert.match(errors.join("\n"), /over the 8 GiB budget/);
});

test("a saving rust-cache step with no scheduled writer fails", () => {
  const errors = check({ budget: { entries: entries(6, 1) }, workflows: many({ "cold.yml": wf(false, {}) }) });
  assert.match(errors.join("\n"), /cold\.yml \(j\).*no schedule/);
});

test("two steps with one explicit shared key fail; job-id keys do not collide", () => {
  const dup = many({ "a.yml": wf(true, { "shared-key": "k0" }) });
  assert.match(check({ budget: { entries: entries(6, 1) }, workflows: dup }).join("\n"), /same rust-cache key/);
  const byJob = many({ "a.yml": wf(true, {}), "b.yml": wf(true, {}) });
  assert.deepEqual(check({ budget: { entries: entries(6, 1) }, workflows: byJob }), []);
});

test("a restore-only step is not a writer", () => {
  const steps = rustCacheSteps({ "r.yml": { on: { workflow_call: null }, jobs: { j: { steps: [{ uses: "Swatinem/rust-cache@v2", with: { "save-if": false } }] } } } });
  assert.equal(steps.length, 0);
});

test("the repository passes", () => {
  assert.equal(main({ out: () => {}, err: console.error }), 0);
});
