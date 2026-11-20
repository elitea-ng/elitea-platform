import assert from "node:assert/strict";
import { test } from "node:test";

import { BUDGET_GIB, check, drift, goCacheSteps, main, orphanReaders, plannedEntries, rustCacheSteps } from "./check-cache-budget.mjs";

const budget = (fixed = 0) => ({ estimates: { rustWithTargets: 1.3, rustRegistryOnly: 0.4, go: 1.2, npm: 0.2 }, measured: {}, fixed: fixed ? [{ family: "f", gib: fixed }] : [] });
const SCHEDULED = { workflow_call: null, schedule: [{ cron: "1 0 * * *" }] };
const MAIN = "${{ github.ref == 'refs/heads/main' }}";
const rust = (schedule, with_, extra = {}) => ({ on: schedule ? SCHEDULED : { workflow_call: null }, jobs: { j: { ...extra, steps: [{ uses: "Swatinem/rust-cache@v2", with: { "save-if": MAIN, ...with_ } }] } } });
const goJob = (cacheWith, extraSteps = []) => ({
  steps: [{ uses: "actions/setup-go@v7", with: { "go-version-file": "go.work" } }, { id: "gocache", uses: "./.github/actions/go-cache", with: { "cache-dependency-path": "a/go.sum", ...cacheWith } }, ...extraSteps],
});
const go = (schedule, jobs) => ({ on: schedule ? SCHEDULED : { workflow_call: null }, jobs });
const many = (extra = {}) => ({
  ...Object.fromEntries(Array.from({ length: 3 }, (_, i) => [`w${i}.yml`, rust(true, { "shared-key": `k${i}` })])),
  "npm.yml": { on: SCHEDULED, jobs: { n: { steps: ["a", "b", "c"].map((p) => ({ uses: "./.github/actions/npm-cache", with: { "cache-dependency-path": `${p}/package-lock.json` } })) } } },
  "go.yml": go(true, { g: goJob({}) }),
  ...extra,
});
const errorsOf = (workflows, b = budget(1.5)) => check({ budget: b, workflows }).errors.join("\n");

test("a derived plan under budget, one scheduled writer per key, passes", () => {
  assert.equal(errorsOf(many()), "");
});

test("a new rust-cache step with targets is counted, so it cannot escape the budget", () => {
  const wfs = many();
  for (let i = 0; i < 5; i += 1) wfs[`big${i}.yml`] = rust(true, { "shared-key": `b${i}` });
  assert.match(errorsOf(wfs), new RegExp(`over the ${BUDGET_GIB} GiB budget`));
  assert.match(errorsOf(wfs), /big\d\.yml:j/);
});

test("a registry-only rust-cache step is refused", () => {
  assert.match(errorsOf(many({ "r.yml": rust(true, { "shared-key": "r", "cache-targets": false }) })), /r\.yml \(j\): a registry-only rust-cache entry/);
});

test("measured sizes override the estimates, and matrix legs multiply", () => {
  const wf = { on: SCHEDULED, jobs: { j: { strategy: { matrix: { include: [{}, {}, {}] } }, steps: [{ uses: "Swatinem/rust-cache@v2", with: { "save-if": MAIN } }] } } };
  const b = budget();
  b.measured["x.yml:j"] = 0.5;
  const total = plannedEntries({ "x.yml": wf, "go.yml": go(true, { g: goJob({}) }) }, b).reduce((s, e) => s + e.gib, 0);
  assert.equal(Math.round(total * 100) / 100, 0.5 * 3 + 1.2);
  assert.match(errorsOf({ ...many(), "x.yml": wf }), /3 writers/);
});

test("a saving rust-cache step with no scheduled writer fails", () => {
  assert.match(errorsOf(many({ "cold.yml": rust(false, { "shared-key": "c" }) })), /cold\.yml \(j\).*no schedule/);
});

test("two steps with one rust-cache key fail as two writers", () => {
  assert.match(errorsOf(many({ "a.yml": rust(true, { "shared-key": "k0" }) })), /2 writers \(w0\.yml \(j\), a\.yml \(j\)\)/);
});

test("a restore-only rust-cache reader must match a scheduled writer's key, targets and CARGO_* environment", () => {
  const reader = (env) => ({ on: { workflow_call: null }, jobs: { r: { env, steps: [{ uses: "Swatinem/rust-cache@v2", with: { "shared-key": "k0", "save-if": false } }] } } });
  assert.equal(orphanReaders({ ...many(), "r.yml": reader({}) }).length, 0);
  assert.equal(orphanReaders({ ...many(), "r.yml": reader({ CARGO_PROFILE_DEV_DEBUG: "0" }) }).length, 1);
  assert.match(errorsOf(many({ "r.yml": reader({ RUSTFLAGS: "-Dwarnings" }) })), /r\.yml \(r\): a restore-only rust-cache step reads/);
  assert.equal(rustCacheSteps({ "r.yml": reader({}) }).length, 0);
});

test("go-cache: one writer per key; a second saving job fails, a read-only one is free", () => {
  assert.match(errorsOf(many({ "go2.yml": go(true, { h: goJob({}) }) })), /go-cache a\/go\.sum \[file:go\.work\]: 2 writers/);
  assert.equal(errorsOf(many({ "go2.yml": go(false, { h: goJob({ save: "false" }) }) })), "");
});

test("go-cache: a later actions/cache/save of the step's key makes it the writer", () => {
  const saver = goJob({ save: "false" }, [{ uses: "actions/cache/save@v6", with: { key: "${{ steps.gocache.outputs.key }}" } }]);
  const steps = goCacheSteps({ "x.yml": go(true, { s: saver }) });
  assert.equal(steps[0].writer, true);
  const wfs = many({ "go.yml": go(true, { s: saver }) });
  assert.equal(errorsOf(wfs), "");
  assert.match(errorsOf({ ...wfs, "go2.yml": go(true, { h: goJob({}) }) }), /2 writers/);
});

test("go-cache: a reader with no writer, or a writer with no schedule, fails", () => {
  const noWriter = many({ "go.yml": go(true, { g: goJob({ save: "false" }) }) });
  assert.match(errorsOf(noWriter), /go\.yml \(g\): reads go-cache a\/go\.sum .* which no job saves/);
  assert.match(errorsOf(many({ "go.yml": go(false, { g: goJob({}) }) })), /no schedule, so main never writes it/);
});

test("go-cache: the Go version source is part of the key", () => {
  const other = { steps: [{ uses: "actions/setup-go@v7", with: { "go-version": "1.27" } }, { uses: "./.github/actions/go-cache", with: { "cache-dependency-path": "a/go.sum", save: "false" } }] };
  assert.match(errorsOf(many({ "o.yml": go(false, { o: other }) })), /\[version:1\.27\], which no job saves/);
});

test("drift warns when main's entries leave the plan or the budget, and names the largest", () => {
  const entries = [{ what: "x", gib: 3 }];
  const G = 1024 ** 3;
  const near = drift({ caches: [{ key: "a", ref: "refs/heads/main", size_in_bytes: 3.5 * G }, { key: "pr", ref: "refs/pull/1/merge", size_in_bytes: 5 * G }], entries });
  assert.deepEqual(near.warnings, []);
  assert.equal(near.largest[0], "3.50 a");
  const far = drift({ caches: [{ key: "a", ref: "refs/heads/main", size_in_bytes: 9 * G }], entries });
  assert.equal(far.warnings.length, 2);
  assert.match(far.warnings.join("\n"), /over the 8 GiB budget/);
  assert.match(far.warnings.join("\n"), /update its measured sizes/);
});

test("the repository passes", () => {
  assert.equal(main({ out: () => {}, err: console.error }), 0);
});
