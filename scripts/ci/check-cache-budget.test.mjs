import assert from "node:assert/strict";
import { test } from "node:test";

import { BUDGET_GIB, check, main, plannedEntries, rustCacheSteps } from "./check-cache-budget.mjs";

const budget = (fixed = 0) => ({ estimates: { rustWithTargets: 1.3, rustRegistryOnly: 0.1, go: 1.2, npm: 0.2 }, fixed: fixed ? [{ family: "f", gib: fixed }] : [] });
const rust = (schedule, with_, extra = {}) => ({ on: schedule ? { workflow_call: null, schedule: [{ cron: "1 0 * * *" }] } : { workflow_call: null }, jobs: { j: { ...extra, steps: [{ uses: "Swatinem/rust-cache@v2", with: { "save-if": "${{ github.ref == 'refs/heads/main' }}", ...with_ } }] } } });
const many = (extra = {}) => ({ ...Object.fromEntries(Array.from({ length: 12 }, (_, i) => [`w${i}.yml`, rust(true, { "shared-key": `k${i}`, "cache-targets": false })])), ...extra });

test("a derived plan under budget with scheduled writers passes", () => {
  assert.deepEqual(check({ budget: budget(), workflows: many() }).errors, []);
});

test("a new rust-cache step with targets is counted, so it cannot escape the budget", () => {
  const wfs = many();
  for (let i = 0; i < 8; i += 1) wfs[`big${i}.yml`] = rust(true, { "shared-key": `b${i}` });
  const { errors } = check({ budget: budget(), workflows: wfs });
  assert.match(errors.join("\n"), new RegExp(`over the ${BUDGET_GIB} GiB budget`));
  assert.match(errors.join("\n"), /big\d\.yml:j/);
});

test("matrix legs multiply, go and npm caches are counted once per path, a read-only go cache is free", () => {
  const wf = { on: { workflow_call: null }, jobs: { j: { strategy: { matrix: { include: [{}, {}, {}] } }, steps: [
    { uses: "Swatinem/rust-cache@v2", with: { "save-if": "refs/heads/main", "cache-targets": false } },
    { uses: "./.github/actions/go-cache", with: { "cache-dependency-path": "a/go.sum" } },
    { uses: "./.github/actions/go-cache", with: { "cache-dependency-path": "a/go.sum" } },
    { uses: "./.github/actions/go-cache", with: { "cache-dependency-path": "b/go.sum", save: "false" } },
    { uses: "./.github/actions/npm-cache", with: { "cache-dependency-path": "web/package-lock.json" } },
  ] } } };
  const total = plannedEntries({ "x.yml": wf }, budget()).reduce((s, e) => s + e.gib, 0);
  assert.equal(Math.round(total * 100) / 100, 0.1 * 3 + 1.2 + 0.2);
});

test("a saving rust-cache step with no scheduled writer fails", () => {
  const { errors } = check({ budget: budget(), workflows: many({ "cold.yml": rust(false, { "cache-targets": false }) }) });
  assert.match(errors.join("\n"), /cold\.yml \(j\).*no schedule/);
});

test("two steps with one explicit shared key fail; job-id keys do not collide", () => {
  const dup = many({ "a.yml": rust(true, { "shared-key": "k0", "cache-targets": false }) });
  assert.match(check({ budget: budget(), workflows: dup }).errors.join("\n"), /same rust-cache key/);
  const byJob = many({ "a.yml": rust(true, { "cache-targets": false }), "b.yml": rust(true, { "cache-targets": false }) });
  assert.deepEqual(check({ budget: budget(), workflows: byJob }).errors, []);
});

test("a restore-only step is not a writer", () => {
  assert.equal(rustCacheSteps({ "r.yml": rust(false, { "save-if": false }) }).length, 0);
});

test("the repository passes", () => {
  assert.equal(main({ out: () => {}, err: console.error }), 0);
});
