// Tests for scripts/ci/browser-bakeoff-report.mjs: the ADR-0049 decision 9
// admission rule over synthetic lines (never real measurements).

import assert from "node:assert/strict";
import { test } from "node:test";

import * as r from "./browser-bakeoff-report.mjs";

const line = (engine, task, kind, ok, extra = {}) => ({ engine, task, class: "docs", kind, ok, crashed: false, hung: false, refs: 10, first_snapshot_ms: 100, peak_memory_bytes: 100 << 20, ...extra });

// Ten tasks: five read/extract, five act.
const kinds = ["read", "extract", "read", "extract", "read", "act", "act", "act", "act", "act"];
const run = (engine, failing = [], extra = {}) => kinds.map((kind, i) => line(engine, `t${i}`, kind, !failing.includes(i), extra[i] ?? {}));

test("lite at parity with full is admitted", () => {
  const result = r.verdict([...run("full"), ...run("lite")]);
  assert.equal(result.admitted, true);
  assert.ok(result.checks.every((check) => check.pass));
});

test("lite below 80% on read and extract is refused", () => {
  // Two of five read/extract fail: 60%.
  const result = r.verdict([...run("full", [0, 1]), ...run("lite", [0, 1])]);
  assert.equal(result.admitted, false);
  assert.equal(r.check(result, "read_extract").pass, false);
  assert.equal(r.check(result, "behind").pass, true);
});

test("lite more than 10 points behind full overall is refused", () => {
  // Full 10/10, lite 8/10 with every read/extract passing: 20 points behind.
  const result = r.verdict([...run("full"), ...run("lite", [5, 6])]);
  assert.equal(r.check(result, "read_extract").pass, true);
  assert.equal(r.check(result, "behind").pass, false);
  assert.equal(result.admitted, false);
  // Exactly 10 points behind passes.
  assert.equal(r.check(r.verdict([...run("full"), ...run("lite", [5])]), "behind").pass, true);
});

test("one crash or hang of lite is refused, even at full success", () => {
  for (const flag of ["crashed", "hung"]) {
    const result = r.verdict([...run("full"), ...run("lite", [], { 7: { [flag]: true } })]);
    assert.equal(result.admitted, false, flag);
    assert.equal(r.check(result, "stable").pass, false, flag);
  }
});

test("full and lite over different tasks, or a missing engine, is no comparison", () => {
  assert.throws(() => r.verdict(run("full")), /no full or no lite/);
  assert.throws(() => r.verdict([...run("full"), ...run("lite").slice(1)]), /different tasks/);
});

test("the summary takes nearest-rank median and p95 and ignores missing memory", () => {
  const lines = run("lite", [], Object.fromEntries(kinds.map((_, i) => [i, { peak_memory_bytes: i === 9 ? null : (i + 1) * 10 }])));
  const s = r.summarise(lines, "lite");
  assert.equal(s.memory_samples, 9);
  assert.equal(s.memory_median_bytes, 50);
  assert.equal(s.memory_p95_bytes, 90);
});

test("the report says the verdict and lists every task", () => {
  const lines = [...run("full"), ...run("lite", [5], { 5: { failure: "step a|b" } })];
  const text = r.markdown(r.verdict(lines), lines);
  assert.match(text, /\*\*Verdict: Moli is admitted\.\*\*/);
  assert.match(text, /step a\\\|b/);
  assert.equal(text.split("\n").filter((row) => /^\| t\d+ \|/.test(row)).length, 20);
});

test("a line that is not JSON names its number", () => {
  assert.throws(() => r.parse('{"engine":"full"}\nnot json\n'), /line 2/);
});

test("a run where full itself fails is no comparison, even when lite matches it", () => {
  // Every task fails on both (a start refused for all): lite is "0 points
  // behind" and never crashed, which must not read as a fair result.
  const all = kinds.map((_, i) => i);
  const result = r.verdict([...run("full", all), ...run("lite", all)]);
  assert.equal(result.comparable, false);
  assert.equal(result.admitted, false);
  assert.equal(r.check(result, "baseline").pass, false);
  assert.match(r.markdown(result, []), /no comparison: the full baseline failed/);
  // The lite rows are not shown as passing: "0 points behind" and "no
  // crash" of an engine that never started are not evidence.
  for (const id of ["read_extract", "behind", "stable"]) {
    assert.equal(r.check(result, id).pass, null, id);
    assert.match(r.check(result, id).measured, /^n\/a \(no comparison/, id);
  }
  assert.doesNotMatch(r.markdown(result, []), /\| yes \|/);
  // Half the corpus on full is enough to compare.
  const half = r.verdict([...run("full", [5, 6, 7, 8, 9]), ...run("lite", [5, 6, 7, 8, 9])]);
  assert.equal(half.comparable, true);
});

test("auto is admitted on its own result while lite crashes (run 42513255705's shape)", () => {
  // Lite crashes on two tasks; auto switched to full on both and passes them.
  const crash = { crashed: true };
  const lines = [
    ...run("full"),
    ...run("lite", [2, 8], { 2: crash, 8: crash }),
    ...run("auto", [], { 2: { switched: "engine_lost" }, 8: { switched: "engine_lost" } }),
  ];
  const result = r.verdict(lines);
  assert.equal(result.admitted, false);
  assert.equal(r.check(result, "stable").pass, false);
  assert.equal(result.auto.admitted, true);
  assert.ok(result.auto.checks.every((check) => check.pass === true));
  assert.match(r.check(result.auto, "stable").rule, /of auto$/);
  const text = r.markdown(result, lines);
  assert.match(text, /\*\*Verdict: Moli is NOT admitted\.\*\*/);
  assert.match(text, /\*\*Verdict \(auto\): auto is admitted\.\*\*/);
  assert.match(text, /t2:engine_lost, t8:engine_lost/);
});

test("auto is held to the same rules: a crash or hang of auto refuses it", () => {
  for (const flag of ["crashed", "hung"]) {
    const result = r.verdict([...run("full"), ...run("lite"), ...run("auto", [], { 3: { [flag]: true } })]);
    assert.equal(result.admitted, true, flag);
    assert.equal(result.auto.admitted, false, flag);
    assert.equal(r.check(result.auto, "stable").pass, false, flag);
  }
  // More than 10 points behind full refuses auto too.
  const behind = r.verdict([...run("full"), ...run("lite"), ...run("auto", [5, 6])]);
  assert.equal(r.check(behind.auto, "behind").pass, false);
  assert.equal(behind.auto.admitted, false);
});

test("auto has no verdict without auto lines, and none over different tasks", () => {
  const result = r.verdict([...run("full"), ...run("lite")]);
  assert.equal(result.auto, null);
  assert.doesNotMatch(r.markdown(result, []), /Verdict \(auto\)/);
  assert.throws(() => r.verdict([...run("full"), ...run("lite"), ...run("auto").slice(1)]), /full and auto ran different tasks/);
});

test("with no working baseline, auto is no comparison either", () => {
  const all = kinds.map((_, i) => i);
  const result = r.verdict([...run("full", all), ...run("lite", all), ...run("auto", all)]);
  assert.equal(result.auto.admitted, false);
  for (const id of ["read_extract", "behind", "stable"]) assert.equal(r.check(result.auto, id).pass, null, id);
  assert.match(r.markdown(result, []), /auto is NOT admitted/);
});
